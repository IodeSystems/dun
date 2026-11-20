package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"
	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/internal/heartbeat"
)

// Sub-agents — spending someone else's context.
//
// A sub-agent is a second Harness. The Store and the Shaper hang off a Session,
// so context isolation forces the split there: nothing short of a whole harness
// gives a child its own window.
//
// The purpose is context OFFLOADING first and parallelism second. A child does
// the expensive thing — fetch a page, read a 30k-line log — and returns two
// sentences; the tokens that produced them die with it. Every default here is
// therefore the cheap side: a child inherits its parent's worktree and MCP
// manager (no second index, no second server), runs on a configured child model,
// and gets no ask_user.
//
// Role picks the tool set (see rebuildTools), and THAT is the enforcement, not a
// convenience: a child never receives `agent`, so depth-1 holds with no counter
// to get wrong; a child never receives `ask_user`, so it structurally cannot
// reach the human; a root never receives `tell_parent`, so it is absent rather
// than a tool that errors when called.
//
// The transport is in-process for a reason that is not stylistic: agentkit's
// mcpmgr is request/response only — CallTool and nothing else, no server→client
// push — so an out-of-process agents server could not notify the parent at all.
// In-process makes tell_parent a direct Notify onto the lift path that already
// exists.

// agentState is what a child IS, as both the model and the human see it.
type agentState string

const (
	agentRunning agentState = "running"
	agentIdle    agentState = "idle" // answered and waiting; NOT gone
	// agentStopped is a child of a RESUMED session: its transcript survived,
	// its process did not. Distinguished from idle because a model that spawned
	// an agent last session must not assume it is still working.
	agentStopped   agentState = "stopped"
	agentDismissed agentState = "dismissed"
)

// subAgent is one child: its harness, what it is doing, and what it has cost.
type subAgent struct {
	num      int
	prompt   string
	model    string
	provider string // "" is the parent session's
	tier     string // the tier that picked its model (tiers.go); "" none did
	note     string // why it is not on the model or tier asked for ("" it is); said once, in the spawn result
	effort   string // "" is the saved /effort level
	// effective is the level its requests send (its effort, or the saved
	// level it inherits, clamped to its model; roles.go), for the strip and
	// dun ps; why says why it is not the level asked for.
	effective, effortWhy string
	parent               *Harness
	iso                  isolation // where it works, when not in its parent's tree (subisolation.go)
	callID               string    // the parent's agent call that spawned it, "" for one from before (agentRecord); under mu

	mu sync.Mutex
	h  *Harness // nil for a stopped child until it is resumed
	// cleared: its row was cleared from the UI list (ClearDoneAgents). It is
	// still in Harness.agents, so the agent tool reaches it by id and resume
	// works; Agents() hides it only while it is stopped or dismissed.
	cleared bool
	// state/status/final are what the child says about itself. status
	// OVERWRITES (it is a state, not a log); final is its answer.
	state  agentState
	status string
	final  string
	// account is what the child published on its plan nodes (plan_close done
	// with the account, or dropped with why). It is the answer: it wins over
	// final and over the prose a turn ends with.
	account string
	// started/ended bound THIS run, not the child's life. A child that is told
	// something new starts a new run, and the report has to be about that one —
	// see startRunLocked.
	started time.Time
	ended   time.Time
	tokens  int
	cancel  context.CancelFunc
	done    chan struct{} // closed when the current run settles
	restore string        // transcript path, for a stopped child
	// hb is the "still there" reminder (internal/heartbeat), debounced by anything
	// this child says — which is why every notification goes through s.notify.
	hb *heartbeat.Beat
	// escalations counts the heartbeats that woke the parent (capped at
	// maxHeartbeatEscalations, asidettl.go).
	escalations atomic.Int32
	// lastActivity is the last time the child showed any sign of life inside
	// its harness: a tool call, a chat round completing, or a transcript entry.
	// This is the cheap signal that distinguishes "working" from "wedged" —
	// a child blocked in Go (compaction that never returns, a channel read
	// with no writer) will stop updating this while remaining agentRunning.
	lastActivity time.Time
	// closed: the parent is shutting down. A run cut by that is not the
	// child's state, so nothing more is saved (saveState).
	closed bool

	// A person reading this child's conversation (AgentWatch): its entries
	// are forwarded while watchers > 0, from after watchFrom (the store seq
	// the history they were sent is current to). watchMu orders the history
	// before the entries.
	watchMu   sync.Mutex
	watchers  int
	watchFrom uint64
}

// notify is every word this child says to its parent, and the ONE place the
// reminder's debounce is reset. Hooked here rather than at each call site so a
// new kind of report cannot forget to count as a sign of life. The reminder
// itself deliberately does not go through here.
func (s *subAgent) notify(text string) {
	if s.hb != nil {
		s.hb.Spoke()
	}
	s.parent.notifyAndWake(text)
}

// heartbeat reminds the parent that a child is still there.
//
// Not only a RUNNING one. An idle child is resident until dismissed and there
// is no cap on how many, so a forgotten one costs its harness forever — and
// "idle for 40 minutes" is precisely the shape of a leak.
func (s *subAgent) heartbeat() {
	if s.hb == nil {
		return
	}
	s.hb.Run(
		func() bool {
			state, _, _, _ := s.snapshot()
			return state == agentDismissed
		},
		func(quiet time.Duration) {
			state, status, _, _ := s.snapshot()
			what := "is still running"
			switch {
			case state == agentIdle:
				what = "has been idle"
			case state == agentStopped:
				return // a stopped child is not doing anything and said so once
			}
			line := fmt.Sprintf("agent #%d %s, quiet for %s — task: %s",
				s.num, what, roundDur(quiet), oneLine(s.prompt, 120))
			if status != "" {
				line += "\nlast status: " + status
			}
			switch {
			case state == agentIdle:
				line += fmt.Sprintf("\nIt is holding its context open. Give it work with "+
					"agent({id:%d, tell:\"…\"}) or end it with agent({id:%d, dismiss:true}).", s.num, s.num)
			default:
				// Check if the child is possibly wedged: it is agentRunning but
				// has not shown any internal activity (tool call, chat round) for
				// longer than the heartbeat interval. A child blocked in Go
				// (compaction that never returns, a channel read with no writer)
				// will stop updating lastActivity while remaining agentRunning.
				s.mu.Lock()
				inactive := time.Since(s.lastActivity)
				s.mu.Unlock()
				if inactive > quiet {
					line += fmt.Sprintf("\nIt has not made progress for %s and may be wedged. "+
						"agent({id:%d}) shows its report; agent({id:%d, dismiss:true}) ends it.",
						roundDur(inactive), s.num, s.num)
				} else {
					line += "\nNothing is necessarily wrong; this is so a wedged agent does not " +
						"look like a working one."
				}
			}
			// Not s.notify: the reminder must not reset the silence it is
			// reporting on. It is an aside, and buys a turn only by escalating:
			// a RUNNING child's, undelivered after asideEscalate, wakes the
			// parent, at most maxHeartbeatEscalations times for this child.
			// An idle child's never does: it is waiting for the parent, not
			// wedged, and a child that asked something is idle, its question
			// the report that already woke the parent (USER 2026-11-18).
			s.parent.notifyQuietlyWith(line, s.parent.heartbeatOpts(fmt.Sprintf("agent:%d:quiet", s.num),
				&s.escalations, state == agentRunning))
		},
	)
}

func (s *subAgent) stop() {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

// startRunLocked is the ONE place a run's clock starts. Caller holds mu.
//
// It exists because tell() used to restart a child without clearing `ended`,
// and snapshot measures to `ended` whenever it is set — so every later report
// gave the duration of the child's FIRST task. Measured live: three polls
// minutes apart all answered "RUNNING after 2m1s".
func (s *subAgent) startRunLocked() {
	if s.h != nil {
		// From now a tell is queued for the run (turngate.go), as a message
		// is for the session's running turn.
		s.h.Gate().Begin()
	}
	s.state = agentRunning
	s.started, s.ended = time.Now(), time.Time{}
	s.lastActivity = time.Now()
	s.done = make(chan struct{})
}

// noteActivity records that the child showed a sign of life inside its harness.
// Called from OnToolCall and noteUsage callbacks so the heartbeat can distinguish
// "working" from "wedged" — a child blocked in Go stops updating this.
func (s *subAgent) noteActivity() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

// settle ends the current run.
func (s *subAgent) settle(state agentState) { s.settleThen(state, nil) }

// settleReporting ends the run and reports it to the parent BEFORE a waiter
// is released: closed first, a waiter (jobs wait, a test) could look for the
// report before it was queued (flaky 1 in 10–20; the same order as the bg
// job's doneCh, 2755d6f).
func (s *subAgent) settleReporting(state agentState) {
	s.settleThen(state, func() {
		if s.hb != nil {
			s.hb.Spoke()
		}
		s.parent.notifyEnded(s.report())
	})
}

func (s *subAgent) settleThen(state agentState, before func()) {
	s.mu.Lock()
	s.state = state
	if s.ended.IsZero() {
		s.ended = time.Now()
	}
	closed, final := s.closed, s.final
	s.mu.Unlock()
	if !closed && s.parent != nil { // the turn ledger (ledger.go)
		outcome := "done"
		switch {
		case state == agentDismissed:
			outcome = "dismissed"
		case strings.HasPrefix(final, "FAILED"):
			outcome = "failed"
		}
		s.parent.ledgerAgent(s.num, headLine(s.prompt, 80), "finished", outcome)
	}
	// Saved before the run is over to a waiter: one that resumes the
	// session right after reads the state this run ended in.
	s.saveState()
	if before != nil {
		before()
	}
	s.mu.Lock()
	if s.done != nil {
		select {
		case <-s.done:
		default:
			close(s.done)
		}
	}
	s.mu.Unlock()
	s.parent.agentsChanged()
}

func (s *subAgent) waitCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.done
}

// setStatus overwrites what the child is doing. Deliberately does NOT notify:
// a status is a state, and a parent woken by every "still reading the log" is
// paying for its child's narration.
func (s *subAgent) setStatus(status string) {
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
	// A status never notifies, but it is still the child speaking — so it
	// debounces the reminder. A child narrating its progress is exactly the
	// case that should never be asked whether it is alive.
	if s.hb != nil {
		s.hb.Spoke()
	}
	s.parent.agentsChanged()
}

// addAccount records one published node's account as part of the answer.
func (s *subAgent) addAccount(line string) {
	s.mu.Lock()
	if s.account != "" {
		s.account += "\n"
	}
	s.account += line
	s.mu.Unlock()
	s.parent.agentsChanged()
}

// answerLocked is the answer the parent reads: the plan accounts when there
// are any, else final. Callers hold mu.
func (s *subAgent) answerLocked() string {
	if s.account != "" {
		return s.account
	}
	return s.final
}

func (s *subAgent) setFinal(msg string) {
	s.mu.Lock()
	s.final = msg
	s.mu.Unlock()
	s.parent.agentsChanged()
}

// snapshot reads the child's whole state under one lock. `since` is the
// duration of THIS run: to `ended` when it has one, to now while it runs.
func (s *subAgent) snapshot() (state agentState, status, final string, since time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started.IsZero() {
		return s.state, s.status, s.answerLocked(), 0
	}
	if !s.ended.IsZero() {
		return s.state, s.status, s.answerLocked(), s.ended.Sub(s.started)
	}
	return s.state, s.status, s.answerLocked(), time.Since(s.started)
}

// line is the child as one row in a list.
func (s *subAgent) line() string {
	state, status, _, since := s.snapshot()
	out := fmt.Sprintf("#%d %s", s.num, state)
	if since > 0 {
		out += " for " + roundDur(since).String()
	}
	if n := s.spent(); n > 0 {
		out += " · " + tokenCount(n)
	}
	if c := s.ctxLine(); c != "" {
		out += " · " + c
	}
	if status != "" {
		out += " — " + status
	}
	return out
}

// ctxLine is how full the child's context is: the last prompt it built,
// against its window ("ctx 12.3k of 131.1k tokens"), so the parent can tell a
// child near its limit (USER 2026-10-28: "when it's an agent, we give ctx
// length info"). "" before its first request.
func (s *subAgent) ctxLine() string {
	s.mu.Lock()
	h := s.h
	s.mu.Unlock()
	if h == nil {
		return ""
	}
	used, win := h.meter.LastPromptTokens(), h.windowTokens()
	if used <= 0 {
		return ""
	}
	if win <= 0 {
		return "ctx " + tokenCount(used)
	}
	return fmt.Sprintf("ctx %s of %s (%d%%)", strings.TrimSuffix(tokenCount(used), " tokens"), tokenCount(win), 100*used/win)
}

// report is what the parent's model reads when a child goes idle, and what
// agent({id:N}) answers with. It leads with the ANSWER, because that is the
// only reason the child was spawned.
func (s *subAgent) report() string {
	state, status, final, since := s.snapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "agent #%d is %s after %s (%s)", s.num, state, roundDur(since), tokenCount(s.spent()))
	if w := s.where(); w != "" {
		fmt.Fprintf(&b, " on %s", w)
	}
	if c := s.ctxLine(); c != "" {
		b.WriteString("\n" + c)
	}
	b.WriteString("\ntask: " + oneLine(s.prompt, 200))
	if status != "" {
		b.WriteString("\nstatus: " + status)
	}
	if final != "" {
		b.WriteString("\nanswer: " + final)
	} else if state == agentIdle {
		b.WriteString("\nanswer: (none: it ended without closing a plan node or replying — plan_show has the node, or ask it with agent({id:" +
			strconv.Itoa(s.num) + ", tell:\"…\"}))")
	}
	b.WriteString(s.iso.status())
	b.WriteString("\nIt is still resident and keeps its context. agent({id:" + strconv.Itoa(s.num) +
		", tell:\"…\"}) is its next message: your answer, if it asked you something, or more work. " +
		"dismiss:true ends it.")
	return b.String()
}

func (s *subAgent) spent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens
}

// setTokens records the child's cumulative spend. ASSIGNED, not added: agentkit
// reports Usage.Total as a running total, so `+=` double-counted every earlier
// turn each time a run finished.
func (s *subAgent) setTokens(n int) {
	s.mu.Lock()
	changed := n != s.tokens
	s.tokens = n
	s.mu.Unlock()
	if changed {
		s.parent.agentsChanged()
	}
}

func tokenCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d tokens", n)
	}
	return fmt.Sprintf("%.1fk tokens", float64(n)/1000)
}

// transcript is where this child's conversation lives on disk.
func (s *subAgent) transcript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.h != nil {
		return s.h.cfg.SessionFile
	}
	// A stopped child has no harness but still has its transcript, which is the
	// part worth pointing at.
	return s.restore
}

// childSessionFile puts a child's transcript beside its parent's, so the path
// in the report is real and the child is inspectable after the fact. An
// in-memory parent gets in-memory children rather than a surprise file.
func childSessionFile(parent string, num int) string {
	if parent == "" {
		return ""
	}
	return fmt.Sprintf("%s.sub%d.jsonl", strings.TrimSuffix(parent, ".jsonl"), num)
}

// where is what the child runs on: its model, its provider when that is not
// the session's, and its effort when it was given one; led by its tier when
// a tier picked the model ("fast: m (gw)").
func (s *subAgent) where() string {
	w := s.model
	if s.tier != "" {
		w = s.tier + ": " + w
	}
	if s.provider != "" {
		w += " (" + s.provider + ")"
	}
	if s.effort != "" {
		if w != "" {
			w += ", "
		}
		w += "effort " + s.effort
	}
	return w
}

// noted is s led by the spawn's note, when the child is not on the model or
// tier asked for ("tier fast unavailable: …; ran on m.").
func (s *subAgent) noted(out string) string {
	if s.note == "" {
		return out
	}
	return s.note + ".\n" + out
}

// tierName is the tier that picked the child's model, "" when none did.
func (s *subAgent) tierName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tier
}

// ChildClient is what Config.ClientFor builds: the runner, and the endpoint
// it talks to, so the child learns its own model's context window and not
// its parent's.
type ChildClient struct {
	Runner   agent.LLMRunner
	Endpoint Endpoint
}

// childConfig derives a child's Config from its parent's.
//
// Everything cheap is inherited and everything expensive is dropped: the
// parent's worktree (so no new tree and no new servers), its exec backend, its
// tool servers. ask_user is NOT inherited — interrupting the human is the root's.
func childConfig(parent *Harness, num int, cc childRun, iso isolation) Config {
	cfg := parent.cfg
	// The exception to inheriting the exec backend: a child gets its OWN shell
	// on the same directory. The persistent shell is shared by POINTER, so
	// without this a child's `export` would be visible to its parent and to
	// every sibling — and every agent's commands would queue behind each other
	// in one shell. Per-agent environment is the whole reason the shell
	// persists at all.
	if hs, ok := cfg.Exec.(*HostShell); ok {
		cfg.Exec = &HostShell{Dir: hs.Dir}
	}
	cfg.Parent = parent
	cfg.AgentID = num
	cfg.Client = cc.client
	cfg.ChildModel = cc.model
	cfg.Effort = cc.effort
	if cc.provider != "" {
		// A child on another provider: its grandchildren default to it too,
		// and it reports its own endpoint and provider.
		cfg.ChildProvider, cfg.Provider = cc.provider, cc.provider
	}
	if cc.endpoint != (Endpoint{}) {
		cfg.Endpoint = cc.endpoint
	}
	cfg.SessionFile = childSessionFile(parent.cfg.SessionFile, num)
	cfg.Servers = parent.specs // shared manager; nothing is spawned from these
	cfg.Ask = nil
	cfg.Peers = nil // other sessions talk to the root, not to its children
	cfg.Loops = nil // and a loop fires into the root
	// A child's tokens, retries and compactions are its own business. Forwarding
	// them to the parent's UI would interleave N children's streams into one
	// conversation, which is the context re-merge the split exists to prevent.
	cfg.OnToken = nil
	cfg.OnReasoning = nil
	cfg.OnRequest = nil
	cfg.OnRound = nil
	cfg.OnToolCall = nil
	cfg.OnToolStart = nil
	cfg.OnToolEnd = nil
	cfg.OnNotify = nil
	cfg.OnDelivered = nil
	cfg.OnDocs = nil
	cfg.OnCompaction = nil
	cfg.OnAgents = nil
	cfg.OnAgentEntry = nil
	cfg.OnJobs = nil
	iso.apply(&cfg) // after the shell: an isolated child's exec is on its own tree
	return cfg
}

// childRun is what a child runs on, as clientFor resolved it.
type childRun struct {
	client          agent.LLMRunner
	provider, model string
	effort          string   // "": the saved /effort level
	endpoint        Endpoint // zero: the parent's
	// fellBack is why the model asked for is not the one this runs on
	// (clientFor fell back to the parent's client); "" when it is.
	fellBack string
}

// name is the model a child runs on, in words.
func (r childRun) name() string {
	if r.model == "" {
		return "the session's model"
	}
	return r.model
}

// clientFor resolves the runner for a named model, falling back to the parent's
// own. The names are returned too, so the report can say what it actually ran
// on rather than what was asked for. A model on another provider has no
// fallback: an error is the spawn's.
func (h *Harness) clientFor(provider, model string) (childRun, error) {
	parent := childRun{client: h.cfg.Client, model: h.cfg.ChildModel}
	if provider == "" {
		if model == "" {
			return parent, nil
		}
		if h.cfg.ClientFor == nil {
			parent.fellBack = "this session builds no client for another model"
			return parent, nil
		}
		c, err := h.cfg.ClientFor("", model)
		if err != nil || c.Runner == nil {
			parent.fellBack = "no client for " + model
			if err != nil {
				parent.fellBack = err.Error()
			}
			return parent, nil
		}
		return childRun{client: c.Runner, model: model, endpoint: c.Endpoint}, nil
	}
	if model == "" {
		return childRun{}, fmt.Errorf("provider %q needs a model: name one of its models", provider)
	}
	if h.cfg.ClientFor == nil {
		return childRun{}, fmt.Errorf("this session cannot run a child on another provider")
	}
	c, err := h.cfg.ClientFor(provider, model)
	if err != nil {
		return childRun{}, err
	}
	if c.Runner == nil {
		return childRun{}, fmt.Errorf("provider %q: no client for %s", provider, model)
	}
	return childRun{client: c.Runner, provider: provider, model: model, endpoint: c.Endpoint}, nil
}

// spawnAgentFor starts a child and registers it. The child is RUNNING when
// this returns; the caller decides whether to wait for it. With node (a ready
// one, planCanAssign) the child is given it before its first turn, and the
// node's brief follows prompt. The model's only way here is spawnTool, which
// always names a node (USER 2026-11-10); a child with no node is the tests'.
// dir is where it works ("" is h's workspace; subisolation.go).
func (h *Harness) spawnAgentFor(ctx context.Context, prompt, tier, provider, model, effort, mode, dir, node string) (*subAgent, error) {
	if node != "" {
		// Before the record is made, so the prompt kept (the agents pane,
		// a resume) is the brief the child got.
		prompt += h.planBrief(node)
	}
	// The role (roles.go): child:<tier>, its model and effort by one
	// precedence; a model given overrides the tier, a tier with no model is
	// the child default.
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier != "" && !isTierName(tier) {
		return nil, fmt.Errorf("tier %q: want %s", tier, strings.Join(TierNames, ", "))
	}
	if effort != "" {
		e, ok := NormalizeEffort(effort)
		if !ok {
			return nil, fmt.Errorf("effort %q: want %s", effort, EffortChoices)
		}
		effort = e
	}
	rr := h.resolver(ctx)
	rr.Profile = nil // clamped below, to the model the child gets
	arg := RoleArg{Provider: provider, Model: model, Effort: effort}
	role := rr.Resolve(ChildRole(tier), arg)
	tierName := strings.TrimPrefix(role.Role, "child:")
	// A level from the layers below the call (--child-effort, facets.child,
	// a tier) that is no level is refused, not dropped: cmd/dun warned at
	// start, and a typo must not quietly mean the saved level.
	checkEffort := func(c RoleChoice) error {
		if w := c.Effort.Wanted; w != "" {
			if _, ok := NormalizeEffort(w); !ok {
				return fmt.Errorf("effort %q (%s): want %s", w, c.EffortSource, EffortChoices)
			}
		}
		return nil
	}
	if err := checkEffort(role); err != nil {
		return nil, err
	}
	// A tier that cannot be served (its provider not logged in, no client
	// for its model, a model the profile knows takes no tool calls) is the
	// child default for this spawn, said in the result (note): one bad tier
	// must not stop every spawn.
	note := role.Note
	var run childRun
	var err error
	if note == "" {
		run, err = h.clientFor(role.Provider, role.Model)
		switch {
		case err != nil && role.Tier == "":
			return nil, err
		case err != nil:
			note = "tier " + role.Tier + " unavailable: " + err.Error()
		case role.Tier != "" && run.fellBack == "":
			if p := h.runProfile(run); p.NoTools() {
				note = "tier " + role.Tier + " unavailable: " + run.name() + " takes no tool calls (" + CapToolsSupported + "=false)"
			}
		}
	}
	if note != "" {
		role = rr.ChildDefault(tierName, RoleArg{Effort: effort})
		if err := checkEffort(role); err != nil {
			return nil, err
		}
		if run, err = h.clientFor(role.Provider, role.Model); err != nil {
			return nil, fmt.Errorf("%s; the child default: %w", note, err)
		}
	}
	tier = role.Tier
	if run.fellBack != "" {
		what := role.Model
		if tier != "" {
			what = "tier " + tier
		}
		if note != "" {
			note += "; then "
		}
		note += what + " unavailable: " + run.fellBack
		tier = ""
	}
	switch {
	case run.fellBack != "":
		note += "; ran on this session's model" // the parent's own client
	case note != "":
		note += "; ran on " + run.name()
	}
	// The effort, clamped to the model the child runs on (a tier's own, not
	// the session's). Unset: the child loads the saved /effort level, as
	// before roles; shown clamped to its model.
	prof := h.runProfile(run)
	eff := efforts.Clamp(prof, role.Effort.Wanted)
	if role.Effort.Wanted == "" {
		eff = efforts.Clamp(prof, LoadEffort())
	} else {
		run.effort = eff.Effective
	}
	iso, err := h.newIsolation(mode, dir)
	if err != nil {
		return nil, err
	}
	h.agMu.Lock()
	h.agSeq++
	num := h.agSeq
	if h.agents == nil {
		h.agents = map[int]*subAgent{}
	}
	h.agMu.Unlock()

	sa := &subAgent{num: num, prompt: prompt, tier: tier, note: note, model: run.model, provider: run.provider, effort: run.effort, effective: eff.Effective, effortWhy: eff.Reason, parent: h, hb: heartbeat.New(), iso: iso}

	// Start on a background context: a child outlives the TURN that spawned it
	// (that is the whole point of an async spawn), so tying it to the caller's
	// context would kill it the moment the parent's turn ended.
	runCtx, cancel := context.WithCancel(context.Background())
	ccfg := childConfig(h, num, run, iso)
	child, err := Start(runCtx, ccfg)
	if err != nil {
		cancel()
		iso.release()
		return nil, err
	}
	saveIsolation(ccfg.SessionFile, iso)
	// self is how the child's own callbacks reach its record here — noteUsage
	// reads it after every chat round. Written before any turn can run.
	child.self = sa

	// OnToolCall is the liveness probe: every tool call proves the child is
	// not wedged. It does NOT forward to the parent's UI — it only updates
	// sa.lastActivity, which the heartbeat checks.
	child.cfg.OnToolCall = func(string, map[string]any, string) {
		sa.noteActivity()
	}

	sa.mu.Lock()
	sa.h = child
	sa.cancel = cancel
	sa.startRunLocked()
	sa.mu.Unlock()

	h.agMu.Lock()
	h.agents[num] = sa
	h.agMu.Unlock()
	sa.hook(child)
	sa.saveState()
	// Given only once the child is on record: a crash before this leaves the
	// node with nobody, not with a child no resume knows of.
	if node != "" {
		if err := h.planAssign(node, num); err != nil {
			sa.quit()
			return nil, err
		}
	}
	h.agentsChanged()

	h.ledgerAgent(num, headLine(prompt, 80), "started", "") // the turn ledger (ledger.go)
	go sa.run(runCtx, prompt+iso.brief())
	go sa.heartbeat()
	return sa, nil
}

// run works one task and settles. A child going idle is NEWS — it is the answer
// the parent is waiting for — so it goes onto the lift path and drives a turn.
func (s *subAgent) run(ctx context.Context, prompt string) {
	s.mu.Lock()
	h := s.h
	s.mu.Unlock()
	// A child whose harness failed to start would otherwise panic the whole
	// process from its own goroutine. Fail this one agent instead.
	if h == nil {
		s.setFinal("agent #" + strconv.Itoa(s.num) + " has no harness — it did not start")
		s.settle(agentIdle)
		return
	}

	res, err := s.work(ctx, h, UserInput{Text: prompt})
	switch {
	case ctx.Err() != nil:
		s.settle(agentDismissed)
		return
	case err != nil:
		s.setFinal("FAILED: " + err.Error())
	default:
		// A child that answered in prose, without a plan_close account, still
		// answered. Silence is distinguished from failure; an unreported answer
		// is not.
		if reply := strings.TrimSpace(res.Reply); reply != "" && s.finalIsEmpty() {
			s.setFinal(oneLine(reply, 2000))
		}
	}
	s.settleReporting(agentIdle)
}

// autoContinues bounds the turns one run takes for news nobody typed (a
// job's finish, a plan change told to it): a run never loops on its own
// news (2df72e1). A child has no automatic-turn budget (autoturn.go: asks
// are off for it), so this is its guard. A message from the person or the
// parent is never counted: it always gets its turn.
const autoContinues = 8

// work is one run of the child, driven as the engine's loop drives the
// main session (cmd/dun/engine.go): first's turn, then a turn for each
// message with images held meanwhile, then a turn for what is queued (a
// message from its last round, which no tool result carried), then — once
// a run — a reminder of the plan nodes it still owes a publish for. After a
// failed turn, a message that came in since that turn began is delivered
// and starts a turn, as the session's queued message does after an error.
// The run ends only through the gate (TurnGate.Settle), so a message is
// never left between its last look and its settle: it is either taken here
// or starts the next run (tell).
func (s *subAgent) work(ctx context.Context, h *Harness, first UserInput) (agent.TurnResult, error) {
	g := h.Gate()
	defer g.End() // on every way out: a tell after it starts the next run
	mark := g.News()
	res, err := s.ask(ctx, h, first)
	autos, reminded := 0, false
	// queued says whether what is queued gets a turn now.
	queued := func(news uint64) bool {
		if h.Pending() == 0 {
			return false
		}
		if err != nil { // only what came in since the failed turn began
			return news != mark
		}
		return len(h.QueuedUser()) > 0 || autos < autoContinues
	}
	keep := func(next agent.TurnResult, nerr error) {
		if err = nerr; err == nil && strings.TrimSpace(next.Reply) != "" {
			res = next
		}
	}
	for ctx.Err() == nil {
		if m, ok := g.TakeHeld(); ok {
			mark = g.News()
			keep(s.ask(ctx, h, m))
			continue
		}
		if queued(g.News()) {
			if len(h.QueuedUser()) == 0 {
				autos++
			}
			mark = g.News()
			keep(h.Continue(ctx))
			continue
		}
		if owed := s.owed(); err == nil && len(owed) > 0 && !reminded {
			reminded = true
			var next agent.TurnResult
			if next, err = h.Ask(ctx, "[control/plan] Before you stop: you have not published plan node(s) "+
				strings.Join(owed, ", ")+". For each, plan_close it done with an account of what you found or made, "+
				"or drop it with why. If one cannot be finished, drop it with why, "+
				"or leave it and say why in your reply."); err == nil && strings.TrimSpace(next.Reply) != "" {
				res.Reply = strings.TrimSpace(res.Reply + "\n\n" + next.Reply) // the answer stays; this adds to it
			}
			continue
		}
		if g.Settle(queued) {
			break
		}
	}
	if owed := s.owed(); len(owed) > 0 && err == nil && ctx.Err() == nil {
		res.Reply = strings.TrimSpace(res.Reply + "\n\n(Still owes a publish for plan node(s) " + strings.Join(owed, ", ") + ".)")
	}
	return res, err
}

// ask is one message's turn, with its images as the session takes them
// (AskImages; images.go).
func (s *subAgent) ask(ctx context.Context, h *Harness, m UserInput) (agent.TurnResult, error) {
	if len(m.Images) > 0 {
		return h.AskImages(ctx, m.Text, m.Images)
	}
	return h.Ask(ctx, m.Text)
}

// owed is the ids and titles of the plan nodes this child has not published.
func (s *subAgent) owed() []string {
	if s.parent == nil {
		return nil
	}
	var out []string
	for _, n := range s.parent.planOwed(planOwner(s.num)) {
		out = append(out, fmt.Sprintf("%s (%s)", n.ID, n.Title))
	}
	return out
}

func (s *subAgent) isDismissed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == agentDismissed
}

func (s *subAgent) finalIsEmpty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.final == ""
}

// tell is the child's next message: more work, or the answer to the question
// its last turn ended with. Those are one gesture — a question is a reply that
// ends a turn, and the answer starts the next turn with the child's context
// intact — so one argument covers both. Nothing blocks on either side: a
// child never waits inside a tool for its parent (docs/decisions.md,
// "Sub-agents ask by ending their turn").
//
// A child mid-run takes it the way the main session takes a message typed
// mid-turn, through the same gate (turngate.go): text is queued on its
// harness and read at its next round boundary — inside the next tool
// result, or in a turn of its own after its last round (work); a message
// with images is held and starts the turn after the running one. Its one
// report covers it (docs/decisions.md, "Telling a working sub-agent").
func (s *subAgent) tell(msg string) string { return s.tellInput(UserInput{Text: msg}) }

// tellInput is tell for a message with images.
func (s *subAgent) tellInput(in UserInput) string {
	s.mu.Lock()
	h := s.h
	s.mu.Unlock()
	if h != nil {
		switch h.Gate().Offer(h, in, nil) {
		case OfferQueued:
			return fmt.Sprintf("Queued for agent #%d: it reads it at its next step, and its report covers it.", s.num)
		case OfferHeld:
			return fmt.Sprintf("Queued for agent #%d: its images wait for its current turn to end, and its report covers it.", s.num)
		}
	}
	s.mu.Lock()
	if s.state == agentRunning {
		h, done := s.h, s.done
		s.mu.Unlock()
		if h != nil && h.Gate().Active() {
			return s.tellInput(in) // a run started since the offer: it takes it
		}
		// Between the run's settle at the gate and its settle here: the next
		// run takes it.
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			return fmt.Sprintf("agent #%d is ending its turn — tell it again in a moment.", s.num)
		}
		return s.tellInput(in)
	}
	if s.h == nil {
		s.mu.Unlock()
		return fmt.Sprintf("agent #%d is stopped. Restart it with agent({id:%d, resume:true}) first.", s.num, s.num)
	}
	// A new task is a new RUN: the clock and the answer both reset, or the next
	// report describes the previous task.
	s.final, s.account = "", ""
	s.startRunLocked() // a tell right behind this one joins the run it starts
	h, ctx := s.h, context.Background()
	if s.cancel != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		s.cancel = cancel
	}
	s.mu.Unlock()
	s.saveState()
	s.parent.agentsChanged()

	go func() {
		res, err := s.work(ctx, h, in)
		if err != nil {
			s.setFinal("FAILED: " + err.Error())
		} else if reply := strings.TrimSpace(res.Reply); reply != "" && s.finalIsEmpty() {
			s.setFinal(oneLine(reply, 2000))
		}
		s.settleReporting(agentIdle)
	}()
	return fmt.Sprintf("Told agent #%d. It is working; you will hear when it is done.", s.num)
}

// quit dismisses a child for good. Children are resident until this is called,
// which is what makes a leaked one a choice rather than an accident.
func (s *subAgent) quit() string {
	s.mu.Lock()
	h := s.h
	// The transcript path lives on the HARNESS, so it has to be salvaged before
	// the harness is dropped — and kept, because after dismissal the transcript
	// is the only thing left of this child. Found live: the dismissal notice
	// said "Its transcript is at ." while the file sat there perfectly well.
	if h != nil && s.restore == "" {
		s.restore = h.cfg.SessionFile
	}
	s.h = nil
	s.mu.Unlock()
	s.stop()
	if h != nil {
		h.Close()
	}
	s.settle(agentDismissed)
	if s.parent != nil {
		s.parent.planRelease(s.num)
	}
	rel := s.iso.release()
	if path := s.transcript(); path != "" {
		return fmt.Sprintf("Dismissed agent #%d. Its transcript is at %s.%s", s.num, path, rel)
	}
	return fmt.Sprintf("Dismissed agent #%d.%s", s.num, rel)
}

// peek reads the last n lines of the child's transcript, which is the only way
// to see what it actually DID rather than what it chose to report.
func (s *subAgent) peek(n int) string {
	path := s.transcript()
	if path == "" {
		return "(no transcript — this session is in memory)"
	}
	f, err := os.Open(path)
	if err != nil {
		return "(transcript unreadable: " + err.Error() + ")"
	}
	defer f.Close()

	ring := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e struct {
			Kind     string `json:"kind"`
			Content  string `json:"content"`
			ToolName string `json:"tool_name"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		line := e.Kind
		if e.ToolName != "" {
			line += " " + e.ToolName
		}
		if c := oneLine(e.Content, 300); c != "" {
			line += ": " + c
		}
		if len(ring) == n && n > 0 {
			ring = ring[1:]
		}
		ring = append(ring, line)
	}
	if len(ring) == 0 {
		return "(transcript is empty)"
	}
	return strings.Join(ring, "\n")
}

// oneLine flattens whitespace and clips, so a 30k-line answer cannot become the
// parent's context window by accident.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// restoreAgents registers the children of a RESUMED session. Their processes
// are gone; their transcripts are not. A dismissed child stays dismissed. In
// a session that persists (Config.Persist) the ones that were running or idle
// are restarted from their transcripts and isolation, idle; otherwise, and
// for one that was already stopped, they come back stopped. It returns the
// restarted ones, and which of those were cut off mid-task.
func (h *Harness) restoreAgents() (resumed []*subAgent, cut map[int]bool) {
	base := h.cfg.SessionFile
	if base == "" {
		return nil, nil
	}
	matches, _ := filepath.Glob(strings.TrimSuffix(base, ".jsonl") + ".sub*.jsonl")
	sort.Strings(matches)
	cut = map[int]bool{}
	var wake []*subAgent
	live := false
	h.agMu.Lock()
	if h.agents == nil {
		h.agents = map[int]*subAgent{}
	}
	for _, path := range matches {
		num, ok := subNumFromPath(base, path)
		if !ok || h.agents[num] != nil {
			continue
		}
		rec := loadAgentState(path)
		sa := &subAgent{
			num: num, parent: h, state: agentStopped, restore: path,
			prompt: rec.Prompt, iso: loadIsolation(path), tier: rec.Tier, model: rec.Model, provider: rec.Provider, effort: loadedEffort(rec.Effort),
			callID: rec.CallID, cleared: rec.Cleared,
		}
		if sa.prompt == "" {
			sa.prompt = firstUserEntry(path)
		}
		switch agentState(rec.State) {
		case agentDismissed:
			sa.state = agentDismissed
		case agentRunning, agentIdle:
			if h.cfg.Persist {
				wake = append(wake, sa)
				cut[num] = agentState(rec.State) == agentRunning
			}
		}
		h.agents[num] = sa
		live = live || sa.state != agentDismissed
		if num > h.agSeq {
			h.agSeq = num
		}
	}
	h.agMu.Unlock()
	for _, sa := range wake {
		h.resumeAgent(sa)
		if sa.harness() != nil {
			resumed = append(resumed, sa)
		}
	}
	if !h.isChild() {
		h.planReleaseOrphans()
	}
	return resumed, cut
}

// agentRecord is what a child's sidecar (<sub>.agent.json) keeps of it for a
// resume: its state when last settled, its model and its task.
type agentRecord struct {
	State    string `json:"state"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"` // "" is the parent session's
	Tier     string `json:"tier,omitempty"`     // "" no tier picked its model
	Effort   string `json:"effort,omitempty"`   // "" is the saved /effort level
	Prompt   string `json:"prompt,omitempty"`   // as asked: the transcript's first message has the isolation brief too
	// CallID is the parent's agent tool call that spawned the child: what
	// lets a replayed history name the child on that call (agentSpawns).
	CallID string `json:"call_id,omitempty"`
	// Cleared: its row was cleared from the UI list (ClearDoneAgents); a
	// resume of the session keeps it hidden. Resuming the child resets it.
	Cleared bool `json:"cleared,omitempty"`
}

func agentFile(sessionFile string) string {
	if sessionFile == "" {
		return ""
	}
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".agent.json"
}

// saveState writes the child's state beside its transcript. Skipped once the
// parent is closing: a run cut by the shutdown is not the child's state, and
// a child that was running then must resume as one that was.
func (s *subAgent) saveState() {
	s.mu.Lock()
	rec, closed := agentRecord{State: string(s.state), Model: s.model, Provider: s.provider, Tier: s.tier, Effort: s.effort, Prompt: s.prompt, CallID: s.callID, Cleared: s.cleared}, s.closed
	path := s.restore
	if s.h != nil {
		path = s.h.cfg.SessionFile
	}
	s.mu.Unlock()
	if closed || path == "" {
		return
	}
	if b, err := json.Marshal(rec); err == nil {
		_ = fsutil.WriteAtomic(agentFile(path), b, 0o600)
	}
}

// agentSpawns maps each spawning agent call of the session in sessionFile to
// the child it started, from the children's sidecars: a history read with no
// harness (SessionHistory) names them as History does.
func agentSpawns(sessionFile string) map[string]int {
	if sessionFile == "" {
		return nil
	}
	matches, _ := filepath.Glob(strings.TrimSuffix(sessionFile, ".jsonl") + ".sub*.jsonl")
	out := map[string]int{}
	for _, path := range matches {
		num, ok := subNumFromPath(sessionFile, path)
		if !ok {
			continue
		}
		if rec := loadAgentState(path); rec.CallID != "" {
			out[rec.CallID] = num
		}
	}
	return out
}

// noticeAgentRe is the head every notice from or about a child starts with,
// as this file and plan.go write them: its message ("agent #N: …"), its
// report ("agent #N is idle after …"), its reminder ("agent #N is still
// running, quiet for …") and its plan publish ("[control/plan] agent #N
// published …"). The writers are here, so the reading is too.
var noticeAgentRe = regexp.MustCompile(`^(?:\[control/plan\] )?agent #(\d+)[: ]`)

// NoticeAgent is the child a notice is from or about, or 0: what the
// notification event and its history item carry as agent, so a client
// opens the child without reading the text. A session entry keeps no field
// for it, so the replay reads it from the same head the live event does.
func NoticeAgent(text string) int {
	m := noticeAgentRe.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// loadAgentState reads a child's sidecar. A transcript from before the
// sidecar existed has none: it comes back stopped, as it always did.
func loadAgentState(sessionFile string) agentRecord {
	var rec agentRecord
	if b, err := os.ReadFile(agentFile(sessionFile)); err == nil {
		_ = json.Unmarshal(b, &rec)
	}
	return rec
}

func (s *subAgent) harness() *Harness {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.h
}

// subNumFromPath recovers a child's id from its transcript filename.
func subNumFromPath(base, path string) (int, bool) {
	prefix := strings.TrimSuffix(base, ".jsonl") + ".sub"
	if !strings.HasPrefix(path, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, prefix), ".jsonl"))
	if err != nil {
		return 0, false
	}
	return n, true
}

// firstUserEntry is the prompt a child was spawned with — the first user
// message in its transcript.
func firstUserEntry(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e struct {
			Kind    string `json:"kind"`
			Content string `json:"content"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Kind == string(agent.KindUser) {
			return oneLine(e.Content, 200)
		}
	}
	return ""
}

// resumeAgent restarts a stopped child from its transcript.
func (h *Harness) resumeAgent(sa *subAgent) string {
	sa.mu.Lock()
	if sa.h != nil {
		sa.mu.Unlock()
		return fmt.Sprintf("agent #%d is already running.", sa.num)
	}
	provider, model, effort := sa.provider, sa.model, sa.effort
	sa.mu.Unlock()

	run, err := h.clientFor(provider, model)
	if err != nil {
		return fmt.Sprintf("agent #%d could not be resumed: %v", sa.num, err)
	}
	run.effort = effort
	ctx, cancel := context.WithCancel(context.Background())
	child, err := Start(ctx, childConfig(h, sa.num, run, sa.iso))
	if err != nil {
		cancel()
		return fmt.Sprintf("agent #%d could not be resumed: %v", sa.num, err)
	}
	child.self = sa
	child.cfg.OnToolCall = func(string, map[string]any, string) {
		sa.noteActivity()
	}

	sa.mu.Lock()
	note := ""
	if run.fellBack != "" {
		sa.tier = "" // clientFor fell back to the parent's client
		note = model + " unavailable: " + run.fellBack + "; it runs on this session's model. "
	}
	sa.h, sa.cancel, sa.model, sa.provider = child, cancel, run.model, run.provider
	// IDLE, not running, and with a ZEROED clock: a restored child has not been
	// asked anything yet, and leaving `started` set made it show a growing age
	// in the field that means "how long its last run took".
	sa.state = agentIdle
	sa.cleared = false
	sa.started, sa.ended = time.Time{}, time.Time{}
	sa.mu.Unlock()
	sa.hook(child)
	sa.saveState()
	h.agentsChanged()
	return note + fmt.Sprintf("agent #%d is back, idle, with its transcript loaded. "+
		"Give it work with agent({id:%d, tell:\"…\"}).", sa.num, sa.num)
}

// closeAgents stops every child. Called before the parent's servers go, because
// a child left running against closed tools is worse than a stopped one.
// agentCloseGrace bounds how long Close waits for a cancelled child to settle.
const agentCloseGrace = 5 * time.Second

func (h *Harness) closeAgents() {
	h.agMu.Lock()
	agents := make([]*subAgent, 0, len(h.agents))
	for _, sa := range h.agents {
		agents = append(agents, sa)
	}
	h.agMu.Unlock()

	for _, sa := range agents {
		sa.mu.Lock()
		sa.closed = true
		sa.mu.Unlock()
		sa.stop()
		// The run goroutine writes the child's transcript and sidecars until
		// it settles. Closing without waiting let it write after the caller
		// returned (a TempDir removed under it). Bounded: a wedged child
		// must not hang Close.
		select {
		case <-sa.waitCh():
		case <-time.After(agentCloseGrace):
		}
		sa.mu.Lock()
		child := sa.h
		sa.h = nil
		sa.mu.Unlock()
		if child != nil {
			child.Close()
		}
	}
}

// agentList is every child of this session, oldest first.
func (h *Harness) agentList() []*subAgent {
	h.agMu.Lock()
	defer h.agMu.Unlock()
	out := make([]*subAgent, 0, len(h.agents))
	for _, sa := range h.agents {
		out = append(out, sa)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].num < out[b].num })
	return out
}

func (h *Harness) agentByID(id int) *subAgent {
	h.agMu.Lock()
	defer h.agMu.Unlock()
	return h.agents[id]
}

// isChild is what rebuildTools switches the tool set on.
func (h *Harness) isChild() bool { return h != nil && h.parent != nil }

// restoredAgentsNote tells a resumed session which children it used to have:
// the ones restarted with it, and the ones that are stopped. Dismissed ones
// are not mentioned.
func (h *Harness) restoredAgentsNote(resumed []*subAgent, cut map[int]bool) string {
	var parts []string
	if len(resumed) > 0 {
		names := make([]string, 0, len(resumed))
		for _, sa := range resumed {
			n := fmt.Sprintf("#%d (%s)", sa.num, oneLine(sa.prompt, 80))
			if cut[sa.num] {
				n += " — it was working when the session stopped and its run was cut off"
			}
			names = append(names, n)
		}
		parts = append(parts, "Sub-agents resumed with this session (it persists), idle, with their transcripts: "+
			strings.Join(names, ", ")+". Give one work, or tell it to carry on, with agent({id:N, tell:\"…\"}).")
	}
	var stopped []string
	for _, sa := range h.agentList() {
		if state, _, _, _ := sa.snapshot(); state == agentStopped {
			stopped = append(stopped, fmt.Sprintf("#%d (%s)", sa.num, oneLine(sa.prompt, 80)))
		}
	}
	if len(stopped) > 0 {
		parts = append(parts, "Sub-agents from the resumed session are STOPPED — their transcripts survived, their processes did not: "+
			strings.Join(stopped, ", ")+". Restart one with agent({id:N, resume:true}) if you still need it.")
	}
	return strings.Join(parts, "\n")
}

// roleSystem is the part of the system prompt that depends on which side of the
// relationship this harness is on. A child is told its role; a root has
// nothing to add to the agent tool's description.
func roleSystem(child bool) string {
	if child {
		return "\n\nYou are a SUB-AGENT working a plan node for another agent, not for a human. Do only that node. " +
			"Publish it with plan_close: done with the full answer as the account, or drop with why. " +
			"tell_parent status says what you do now. " +
			"To ask your parent something, end your turn with the question. " +
			"You cannot reach the human and you cannot spawn agents; if the node needs either, say so in the account."
	}
	// A root's side is the agent tool's description: said once, there.
	return ""
}

// ── the tools ───────────────────────────────────────────────────────

// agentEffortEnum is the agent tool's effort enum: every level, and lowest
// (the least the child's model allows). off, the old name, is still read.
var agentEffortEnum = append(append([]string(nil), EffortLevels...), EffortLowest)

// agentToolDef is the agent tool. providers are the logged-in ones; with
// more than one, a spawn may name another. tiers (tiers.go) add the tier
// param and a line per tier only when one names a model: with none the tool
// is byte for byte as before tiers.
func agentToolDef(defaultModel, defaultProvider, defaultEffort string, providers []string, tiers []Tier, defaultTier string) llm.ToolDef {
	model := "the session's child model"
	if defaultModel != "" {
		model = defaultModel
	}
	if defaultProvider != "" {
		model += " on provider " + defaultProvider
	}
	effort := "the session's /effort level"
	if defaultEffort != "" {
		effort = defaultEffort
	}
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "agent"
	td.Function.Description = "Start a sub-agent on a plan node: for work that would cost you a lot of context for a " +
		"small answer (reading a long log, scanning a large file), or for work on Y that does not depend on X, so X continues. Add the node with plan_add first, then agent({node:\"<id>\"}): " +
		"the node's title, details and done_when are the child's whole brief (it cannot see your conversation), and " +
		"it publishes its answer onto the node. It spends its own context; what it read never enters yours. " +
		"isolation none: it has your tools, in your tree. worktree and docker: your tools, in its own tree. " +
		"dir puts it in another directory, a git worktree of that directory's repo with worktree. " +
		"nodir: NO project and NO file tools; its shell runs in a scratch dir, deleted on dismiss. Never use nodir " +
		"for work that reads or edits project files. If a child drops its node with \"missing tools\", start it again " +
		"with isolation none or worktree. It returns at once and you are notified each time the child ends a turn, unless wait:true. " +
		"Then act on it by id: agent({id:N, tell:\"…\"}) answers its question or gives it new work, node gives it " +
		"another plan node, resume restarts a stopped one, dismiss:true ends it. It stays resident until you dismiss " +
		"it; nothing cleans it up for you. Default model: " + model + ". Default effort: " + effort + "."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"model": map[string]any{"type": "string", "description": "run this child on a specific model"},
			"effort": map[string]any{"type": "string", "enum": agentEffortEnum,
				"description": "how much this child thinks before it answers: none is fastest, max thinks most, lowest is the least the model allows, default leaves it to the server"},
			"wait": map[string]any{"type": "boolean", "description": "block until it answers, instead of being notified later"},
			"node": map[string]any{"type": "string", "description": "the plan node a new child works: a ready one (todo, no child on it, " +
				"its needs done). Its title, details and done_when are the child's brief; it owns the node alone and owes you a " +
				"publish of it (done with notes, or dropped with why); the node leaves your ready list. It gets plan tools that " +
				"change only its own nodes. With id: give idle agent #id this node; tell adds to its brief"},
			"id":      map[string]any{"type": "integer", "description": "act on existing agent #id instead of starting one"},
			"tell":    map[string]any{"type": "string", "description": "with id: its next message: the answer to its question, or new work"},
			"resume":  map[string]any{"type": "boolean", "description": "with id: restart a stopped agent from its transcript"},
			"dismiss": map[string]any{"type": "boolean", "description": "with id: end it; what it did not finish of a plan node goes back"},
			"isolation": map[string]any{"type": "string", "enum": []string{IsoNone, IsoWorktree, IsoDocker, IsoNoDir},
				"description": "where the child works. none (default): this tree. worktree: its own git worktree branched " +
					"from this tree's last commit (uncommitted changes are not in it); it commits there and you merge its " +
					"branch — use it for children that edit at the same time. docker: the same, its commands in a " +
					"container. nodir: no project, no file tools, shell in a scratch dir deleted on dismiss; only for work that needs no project files."},
			"dir": map[string]any{"type": "string", "description": "a new child's directory, absolute, ~/ or relative to " +
				"your workspace (default: your workspace). With isolation none it works there in place; with worktree or " +
				"docker its worktree is branched from that directory's git repository, and it works in the same subdirectory of it"},
		},
	}
	if tiered(tiers) {
		if defaultTier == "" {
			defaultTier = TierWork
		}
		lines := make([]string, 0, len(tiers))
		for _, t := range tiers {
			on := "the default model"
			if t.on() && t.Off != "" {
				on = "unavailable, " + t.Off + ": the default model"
			} else if t.on() {
				on = t.Model
				if t.Provider != "" {
					on += " on " + t.Provider
				}
				if t.Effort != "" {
					on += ", effort " + t.Effort
				}
			}
			lines = append(lines, t.Name+": "+t.Use+" ("+on+")")
		}
		td.Function.Description += " Pick the tier by the work: " + strings.Join(lines, "; ") +
			". Default tier: " + defaultTier + ". A model given overrides the tier."
		td.Function.Parameters.(map[string]any)["properties"].(map[string]any)["tier"] = map[string]any{
			"type": "string", "enum": TierNames,
			"description": "the kind of work, which picks the child's model and effort (the tiers above)",
		}
	}
	if len(providers) > 1 {
		td.Function.Parameters.(map[string]any)["properties"].(map[string]any)["provider"] = map[string]any{
			"type": "string", "enum": providers,
			"description": "run this child on another logged-in provider (then name one of its models); " +
				"omitted, it runs on this session's provider",
		}
	}
	return td
}

func withAgent(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "agent" {
			return inner(ctx, tc)
		}
		var args struct {
			// Prompt is read only to refuse it: the brief is the node's
			// (USER 2026-11-10, "Subagents should only be launchable via plan nodes").
			Prompt    string `json:"prompt"`
			Provider  string `json:"provider"`
			Tier      string `json:"tier"`
			Model     string `json:"model"`
			Effort    string `json:"effort"`
			Wait      bool   `json:"wait"`
			Isolation string `json:"isolation"`
			Dir       string `json:"dir"`
			Node      string `json:"node"`
			// With id: act on agent #id (these were jobs(agent:N, …) until
			// 2026-11-06, USER: "Agent stuff moves").
			ID      *int   `json:"id"`
			Tell    string `json:"tell"`
			Resume  bool   `json:"resume"`
			Dismiss bool   `json:"dismiss"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		var out string
		num := 0 // the child this call started or acted on, for the tool_result event
		switch {
		case strings.TrimSpace(args.Prompt) != "":
			out = agentNoPrompt
		case args.ID != nil && strings.TrimSpace(args.Dir) != "" && !h.agentInDir(*args.ID, args.Dir):
			out = fmt.Sprintf("ERROR: dir is a new agent's; agent #%d keeps the directory it was started in. Nothing was done.", *args.ID)
		case args.ID != nil:
			out = agentOp(h, *args.ID, args.Tell, args.Node, args.Resume, args.Dismiss)
			if h.agentByID(*args.ID) != nil {
				num = *args.ID
			}
		default:
			out, num = spawnCall(ctx, h, tc.ID, args.Tier, args.Provider, args.Model, args.Effort, args.Isolation, strings.TrimSpace(args.Dir), args.Node, args.Wait)
		}
		if num > 0 {
			h.toolCur.noteAgent(num)
		}
		if onCall != nil {
			call := map[string]any{"model": args.Model, "wait": args.Wait}
			if args.ID != nil {
				call = map[string]any{"id": *args.ID}
				for k, v := range map[string]any{"tell": args.Tell, "node": args.Node} {
					if v != "" {
						call[k] = v
					}
				}
				if args.Resume {
					call["resume"] = true
				}
				if args.Dismiss {
					call["dismiss"] = true
				}
			}
			if args.Provider != "" {
				call["provider"] = args.Provider
			}
			if args.Effort != "" {
				call["effort"] = args.Effort
			}
			if args.Node != "" {
				call["node"] = args.Node
			}
			if args.Dir != "" {
				call["dir"] = args.Dir
			}
			onCall("agent", call, out)
		}
		return out, nil
	}
}

// The refusals of a spawn that names no usable node (USER 2026-11-10:
// "Subagents should only be launchable via plan nodes!"). Each says the fix.
const (
	agentNoNode = "ERROR: agent starts a child only on a plan node. " + planFix + " No agent was started."
	// agentNoPrompt: the brief is the node's alone, so the task lives in the
	// plan, where the person and a resume can read it, and not in a prompt
	// only the child saw.
	agentNoPrompt = "ERROR: agent takes no prompt; a child's brief is its plan node's title, details and done_when. " +
		"Put the task in the node's details with plan_add, then call agent({node:\"<id>\"}). No agent was started."
	agentNoPlan = "ERROR: agent starts a child only on a plan node, and the plan tools are off in this session " +
		"(the user turns it on in /mcp). No agent was started."
)

// spawnTool is agent({node, …}): the model's only way to start a child. The
// child's first message is node's brief (planBrief); nothing else.
func spawnTool(ctx context.Context, h *Harness, provider, model, effort, isolation, node string, wait bool) string {
	out, _ := spawnCall(ctx, h, "", "", provider, model, effort, isolation, "", node, wait)
	return out
}

// agentInDir reports whether dir is where agent #id works (or there is no
// such agent: agentOp says so).
func (h *Harness) agentInDir(id int, dir string) bool {
	sa := h.agentByID(id)
	if sa == nil {
		return true
	}
	d, err := h.resolveDir(dir)
	if err != nil {
		return false
	}
	ws := sa.iso.workspace()
	if ws == "" {
		ws = h.Workspace()
	}
	return sameDir(d, ws) || (sa.iso.Dir != "" && sameDir(d, sa.iso.Dir))
}

// spawnCall is spawnTool for the tool call callID: it also returns the
// child's number (0 when none started), and the child's sidecar keeps
// callID, so a replayed history names the child its spawn started
// (History, agentSpawns).
func spawnCall(ctx context.Context, h *Harness, callID, tier, provider, model, effort, isolation, dir, node string, wait bool) (string, int) {
	node = strings.TrimSpace(node)
	switch {
	case node == "" && !h.hasTool(taskgraph.ToolAdd):
		return agentNoPlan, 0
	case node == "":
		return agentNoNode, 0
	}
	// Checked before the spawn too, so a bad node starts no child.
	if err := h.planCanAssign(node); err != nil {
		return "ERROR: " + strings.TrimSuffix(err.Error(), ".") + ". No agent was started.", 0
	}
	sa, err := h.spawnAgentFor(ctx, "Work this plan node.", tier, provider, model, effort, isolation, dir, node)
	if err != nil {
		return "ERROR: could not start the agent: " + err.Error(), 0
	}
	if callID != "" {
		sa.mu.Lock()
		sa.callID = callID
		sa.mu.Unlock()
		sa.saveState()
	}
	owns := fmt.Sprintf(" It owns plan node %s, out of your ready list, and owes you a publish of it; "+
		"you are told when it publishes, with what became ready. Dismissing it gives back what it did not finish.", node)
	if !wait {
		where := ""
		if sa.iso.isolated() {
			where = " in " + sa.iso.Mode + " isolation at " + sa.iso.workspace()
			if sa.iso.Branch != "" {
				where += " (branch " + sa.iso.Branch
				if sa.iso.Dir != "" {
					where += " of " + sa.iso.Repo
				}
				where += ")"
			}
			if sa.iso.Mode == IsoNoDir {
				where += ". nodir: no file tools; shell in a scratch dir, deleted on dismiss"
			}
		} else if sa.iso.Dir != "" {
			where = " in " + sa.iso.Dir + ", in place"
		}
		// No model name when the child runs on the parent's own client: it
		// read "Started agent #1 on ." live.
		if w := sa.where(); w != "" {
			where = " on " + w + where
		}
		return sa.noted(fmt.Sprintf("Started agent #%d%s.%s It is working; you will be told when it finishes or asks you something. "+
			"Do other work, or end your turn if you have nothing to do until it answers.",
			sa.num, where, owns)), sa.num
	}
	select {
	case <-sa.waitCh():
	case <-ctx.Done():
		return sa.noted(fmt.Sprintf("agent #%d is still running; the turn was cancelled.", sa.num)), sa.num
	case <-h.userWaiting():
		return sa.noted(fmt.Sprintf("agent #%d is still running; stopped waiting because a message arrived (the user wrote, or another dun session did) — "+
			"it follows. You will be told when it finishes.", sa.num)), sa.num
	}
	return sa.noted(strings.TrimSpace(owns) + "\n" + sa.report()), sa.num
}

// agentMonitor is the agent half of the jobs tool (jobs.go): list, or one
// child's report after a kill, resume, tell or wait.
// agentOp is agent({id, …}): act on an existing child. One op per call;
// node may carry tell as more brief. With none, the child's report.
func agentOp(h *Harness, id int, tell, node string, resume, dismiss bool) string {
	sa := h.agentByID(id)
	if sa == nil {
		return fmt.Sprintf("ERROR: no agent #%d.", id)
	}
	node = strings.TrimSpace(node)
	switch {
	case (dismiss || resume) && (node != "" || tell != ""), dismiss && resume:
		return "ERROR: one of tell, node, resume or dismiss per call (node may carry tell as more brief)."
	case dismiss:
		return sa.quit()
	case resume:
		return h.resumeAgent(sa)
	case node != "":
		t, err := h.giveNode(id, node, tell)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		tell = t
	}
	var b strings.Builder
	if tell != "" {
		b.WriteString(sa.tell(tell) + "\n")
	}
	b.WriteString(sa.report())
	return b.String()
}

func tellParentToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "tell_parent"
	td.Function.Description = "Report to the agent that spawned you. `status` OVERWRITES — it is what you are doing " +
		"right now, not a log, and it never interrupts your parent. `message` is an event worth its attention. " +
		"With a plan node, the answer is not here: publish it with plan_close. `final` is for a child with no node."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":  map[string]any{"type": "string", "description": "what you are doing now (overwrites the last one)"},
			"message": map[string]any{"type": "string", "description": "something your parent should know now"},
			"final":   map[string]any{"type": "string", "description": "for a child with no plan node: your answer, complete on its own. With a node, publish with plan_close instead"},
		},
	}
	return td
}

func withTellParent(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "tell_parent" {
			return inner(ctx, tc)
		}
		var args struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Final   string `json:"final"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		out := tellParent(h, args.Status, args.Message, args.Final)
		if onCall != nil {
			onCall("tell_parent", map[string]any{
				"status": args.Status, "message": args.Message, "final": args.Final,
			}, out)
		}
		return out, nil
	}
}

func tellParent(h *Harness, status, message, final string) string {
	self := h.self
	if self == nil || h.parent == nil {
		return "ERROR: you have no parent to tell."
	}
	var did []string
	if status != "" {
		self.setStatus(status)
		did = append(did, "status set")
	}
	if message != "" {
		// An event, not state: delivered now, and it accumulates.
		self.notify(fmt.Sprintf("agent #%d: %s", self.num, message))
		did = append(did, "message delivered")
	}
	if final != "" {
		self.setFinal(final)
		did = append(did, "answer recorded — you can stop now")
	}
	if len(did) == 0 {
		return "Nothing to report: tell_parent needs at least one of status, message or final."
	}
	return strings.Join(did, "; ") + "."
}

// ask_parent is a HIDDEN alias, kept for old transcripts and for a model's
// habit; it is not in a child's tool list. A child asks by ending its turn with
// the question (USER 2026-10-27: sub-agent messages go out of band, like
// dun↔dun ones). The alias is tell_parent(final: question) plus an instruction
// to end the turn: the question becomes the child's report, the turn end
// delivers it to the parent, and the parent's tell starts the next turn. It
// never blocks — there is no answer channel to wait on.
func withAskParent(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "ask_parent" {
			return inner(ctx, tc)
		}
		var args struct {
			Question string `json:"question"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		out := askParent(h, args.Question)
		if onCall != nil {
			onCall("ask_parent", map[string]any{"question": args.Question}, out)
		}
		return out, nil
	}
}

func askParent(h *Harness, question string) string {
	if strings.TrimSpace(question) == "" {
		return "ERROR: ask_parent requires a question."
	}
	if out := tellParent(h, "", "", question); strings.HasPrefix(out, "ERROR") {
		return out
	}
	return "Your question is your report to your parent. End your turn now; its answer arrives as your next message."
}

// ── the human's view ────────────────────────────────────────────────

// AgentInfo is one child as the UI sees it.
type AgentInfo struct {
	ID    int    `json:"id"`
	Model string `json:"model,omitempty"`
	Tier  string `json:"tier,omitempty"` // the tier that picked Model; "" none did
	// Effort is the level its requests send (roles.go: clamped to its model).
	Effort  string `json:"effort,omitempty"`
	State   string `json:"state"`
	Status  string `json:"status,omitempty"`
	Prompt  string `json:"prompt"`
	Tokens  int    `json:"tokens"`
	Seconds int    `json:"seconds"`
	// Dir is the tree an isolated child works in, or the spawn's dir; ""
	// is its parent's tree. dun ps matches processes against it.
	Dir string `json:"dir,omitempty"`
	// Blocked is why the child is stuck: the reason on a plan node it owns
	// that it closed as blocked (plan_close outcome block). "" none is.
	Blocked string `json:"blocked,omitempty"`
}

// Agents is every child of this session, oldest first.
func (h *Harness) Agents() []AgentInfo {
	list := h.agentList()
	out := make([]AgentInfo, 0, len(list))
	blocked := h.planBlockedByAgent(len(list))
	for _, sa := range list {
		state, status, _, since := sa.snapshot()
		sa.mu.Lock()
		hide := sa.cleared && (state == agentStopped || state == agentDismissed)
		sa.mu.Unlock()
		if hide {
			continue
		}
		out = append(out, AgentInfo{
			ID: sa.num, Model: sa.where(), Tier: sa.tierName(), Effort: sa.effective, State: string(state), Status: status,
			Prompt: sa.prompt, Tokens: sa.spent(), Seconds: int(since.Seconds()),
			Dir: sa.iso.workspace(), Blocked: blocked[sa.num],
		})
	}
	return out
}

// ClearDoneAgents hides every stopped and dismissed child from the list the
// UI shows and pushes the list. Idle and running children stay. Nothing is
// deleted: the child stays in the registry (reachable by id for the agent
// tool and resume) and its transcript stays on disk. A cleared child that is
// resumed shows again. It returns how many rows it hid.
func (h *Harness) ClearDoneAgents() int {
	n := 0
	for _, sa := range h.agentList() {
		state, _, _, _ := sa.snapshot()
		sa.mu.Lock()
		hide := !sa.cleared && (state == agentStopped || state == agentDismissed)
		if hide {
			sa.cleared = true
			n++
		}
		sa.mu.Unlock()
		if hide {
			sa.saveState() // a session resume keeps it hidden
		}
	}
	if n > 0 {
		h.agentsChanged()
	}
	return n
}

// agentsChanged pushes the current child list to the UI. Called on every
// transition a human would want to see — spawn, status, settle, dismiss — since
// a pane that only updates when the model happens to look is not a monitor.
func (h *Harness) agentsChanged() {
	if h == nil || h.cfg.OnAgents == nil {
		return
	}
	h.cfg.OnAgents(h.Agents())
}

// AgentHistory returns a child's conversation as replayable items — the same
// neutral shape the resuming client already knows how to render, so the TUI
// gets a child's scrollback without a second protocol.
//
// PULL, not push. A child's callbacks are deliberately nil (childConfig), and
// un-nil'ing them would mean tagging and forwarding N streams nobody is looking
// at. Reading on demand also works unchanged for a STOPPED child, whose process
// is gone but whose transcript is not — which push could not do at all.
func (h *Harness) AgentHistory(id int) ([]HistoryItem, bool) {
	sa := h.agentByID(id)
	if sa == nil {
		return nil, false
	}
	sa.mu.Lock()
	child := sa.h
	sa.mu.Unlock()
	if child != nil {
		return child.History(), true
	}
	path := sa.transcript()
	if path == "" {
		return nil, true
	}
	st, err := openSessionStore(path)
	if err != nil {
		return nil, true
	}
	return (&Harness{store: st}).History(), true
}

// hook forwards the child's new transcript entries to whoever watches it.
// A new harness's store counts from zero, so the watch starts over with it.
func (s *subAgent) hook(child *Harness) {
	s.watchMu.Lock()
	s.watchFrom = 0
	s.watchMu.Unlock()
	child.store.setOnEntry(func(e agent.Entry, seq uint64) {
		s.watchMu.Lock()
		defer s.watchMu.Unlock()
		if s.watchers == 0 || seq <= s.watchFrom {
			return
		}
		cb := s.parent.cfg.OnAgentEntry
		if items := historyItems([]agent.Entry{e}); cb != nil && len(items) > 0 {
			cb(s.num, items)
		}
	})
}

// AgentWatch is a person opening a child's conversation: send gets its
// history now, and OnAgentEntry gets each entry after it until AgentUnwatch.
// One call each way per viewer; the watch is a count, so two viewers do not
// end each other's. A stopped child's history is its transcript; it has no
// entries to follow until it is resumed.
//
// PUSH only while watched: a child's callbacks stay nil (childConfig), so N
// children nobody looks at cost nothing.
func (h *Harness) AgentWatch(id int, send func([]HistoryItem)) bool {
	sa := h.agentByID(id)
	if sa == nil {
		return false
	}
	sa.watchMu.Lock()
	defer sa.watchMu.Unlock()
	if child := sa.harness(); child != nil {
		entries, seq := child.store.contextAt()
		sa.watchFrom = seq
		send(historyItems(entries))
	} else {
		items, _ := h.AgentHistory(id)
		send(items)
	}
	sa.watchers++
	return true
}

// AgentUnwatch is a viewer leaving a child's conversation.
func (h *Harness) AgentUnwatch(id int) {
	sa := h.agentByID(id)
	if sa == nil {
		return
	}
	sa.watchMu.Lock()
	if sa.watchers > 0 {
		sa.watchers--
	}
	sa.watchMu.Unlock()
}

// AgentTell is the human steering a child directly, which is the same gesture
// the parent's agent({id:N, tell:}) makes — deliberately the same code path, so
// there is one definition of what telling a child means.
func (h *Harness) AgentTell(id int, text string) string { return h.AgentTellImages(id, text, nil) }

// AgentTellImages is AgentTell for a message with images: the child takes
// them as the session does (tell).
func (h *Harness) AgentTellImages(id int, text string, imgs []ImageInput) string {
	sa := h.agentByID(id)
	if sa == nil {
		return fmt.Sprintf("no agent #%d", id)
	}
	out := sa.tellInput(UserInput{Text: text, Images: imgs})
	if strings.HasPrefix(out, "Told agent") || strings.HasPrefix(out, "Queued for agent") {
		// The parent reads the child's next report, and without this it
		// is an answer to something it never asked (live: "a leftover
		// instruction from its pre-restart transcript"). An aside: news
		// for the turn that report starts, not a turn of its own.
		h.Aside(fmt.Sprintf("The user told agent #%d directly: %q. Its next report answers that.", id, oneLine(text, 300)))
	}
	return out
}

// AgentImagesRefused is the child's ImagesRefused: "" when a message with
// images may go to it, or there is no such child (AgentTell says so).
func (h *Harness) AgentImagesRefused(id int) string {
	if sa := h.agentByID(id); sa != nil {
		if ch := sa.harness(); ch != nil {
			return ch.ImagesRefused()
		}
	}
	return ""
}

// AgentContinue is dund's resume of a child that a restart cut mid-run
// (USER 2026-11-17): restarted from its transcript if it came back stopped,
// then told text, so it carries on with its task rather than wait idle.
// The parent is told, in an aside, that the child was resumed.
func (h *Harness) AgentContinue(id int, text string) string {
	sa := h.agentByID(id)
	if sa == nil {
		return fmt.Sprintf("no agent #%d", id)
	}
	if state, _, _, _ := sa.snapshot(); state == agentDismissed {
		return fmt.Sprintf("agent #%d was dismissed", id)
	}
	if sa.harness() == nil {
		if out := h.resumeAgent(sa); sa.harness() == nil {
			return out
		}
	}
	out := sa.tell(text)
	if strings.HasPrefix(out, "Told agent") {
		h.Aside(fmt.Sprintf("Agent #%d (%s) was working when dund restarted; dund resumed it with its task. "+
			"Its next report comes as usual.", id, oneLine(sa.prompt, 80)))
	}
	return out
}

// AgentPrompt is what a child was asked to do, for the UI's task line.
func (h *Harness) AgentPrompt(id int) string {
	if sa := h.agentByID(id); sa != nil {
		return sa.prompt
	}
	return ""
}
