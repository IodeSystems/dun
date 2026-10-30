package dun

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
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
	parent   *Harness
	iso      isolation // where it works, when not in its parent's tree (subisolation.go)

	mu sync.Mutex
	h  *Harness // nil for a stopped child until it is resumed
	// state/status/final are what the child says about itself. status
	// OVERWRITES (it is a state, not a log); final is its answer.
	state  agentState
	status string
	final  string
	// started/ended bound THIS run, not the child's life. A child that is told
	// something new starts a new run, and the report has to be about that one —
	// see startRunLocked.
	started time.Time
	ended   time.Time
	tokens  int
	cancel  context.CancelFunc
	done    chan struct{} // closed when the current run settles
	restore string        // transcript path, for a stopped child
	// hb is the "still there" reminder (heartbeat.go), debounced by anything
	// this child says — which is why every notification goes through s.notify.
	hb *heartbeat
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
		s.hb.spoke()
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
	s.hb.run(
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
					"jobs(agent:%d, tell:\"…\") or dismiss it with kill:true.", s.num)
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
						"jobs(agent:%d, tail:40) to see what it was doing.",
						roundDur(inactive), s.num)
				} else {
					line += fmt.Sprintf("\nNothing is necessarily wrong; this is so a wedged agent does not "+
						"look like a working one. jobs(agent:%d, tail:40) to see what it is doing.", s.num)
				}
			}
			// Not s.notify: the reminder must not reset the silence it is
			// reporting on. A reminder is the absence of news, so it never buys
			// a turn. (A child that asked something is idle, and its question
			// was the report that already woke the parent.)
			s.parent.notifyQuietly(line)
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
	s.settleThen(state, func() { s.notify(s.report()) })
}

func (s *subAgent) settleThen(state agentState, before func()) {
	s.mu.Lock()
	s.state = state
	if s.ended.IsZero() {
		s.ended = time.Now()
	}
	s.mu.Unlock()
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
		s.hb.spoke()
	}
	s.parent.agentsChanged()
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
		return s.state, s.status, s.final, 0
	}
	if !s.ended.IsZero() {
		return s.state, s.status, s.final, s.ended.Sub(s.started)
	}
	return s.state, s.status, s.final, time.Since(s.started)
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
	used, win := h.meter.lastPromptTokens(), h.windowTokens()
	if used <= 0 {
		return ""
	}
	if win <= 0 {
		return "ctx " + tokenCount(used)
	}
	return fmt.Sprintf("ctx %s of %s (%d%%)", strings.TrimSuffix(tokenCount(used), " tokens"), tokenCount(win), 100*used/win)
}

// report is what the parent's model reads when a child goes idle, and what
// jobs(agent:N) answers with. It leads with the ANSWER, because that is the
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
		b.WriteString("\nanswer: (it stopped without calling tell_parent — use jobs(agent:" +
			strconv.Itoa(s.num) + ", tail:40) to read what it did)")
	}
	b.WriteString(s.iso.status())
	b.WriteString("\nIt is still resident and keeps its context. jobs(agent:" + strconv.Itoa(s.num) +
		", tell:\"…\") is its next message: your answer, if it asked you something, or more work. " +
		"kill:true dismisses it.")
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

// where is what the child runs on: its model, and its provider when that is
// not the session's.
func (s *subAgent) where() string {
	if s.provider == "" {
		return s.model
	}
	return s.model + " (" + s.provider + ")"
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
	cfg.OnToolCall = nil
	cfg.OnNotify = nil
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
	endpoint        Endpoint // zero: the parent's
}

// clientFor resolves the runner for a named model, falling back to the parent's
// own. The names are returned too, so the report can say what it actually ran
// on rather than what was asked for. A model on another provider has no
// fallback: an error is the spawn's.
func (h *Harness) clientFor(provider, model string) (childRun, error) {
	parent := childRun{client: h.cfg.Client, model: h.cfg.ChildModel}
	if provider == "" {
		if model == "" || h.cfg.ClientFor == nil {
			return parent, nil
		}
		c, err := h.cfg.ClientFor("", model)
		if err != nil || c.Runner == nil {
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

// spawnAgent starts a child and registers it. The child is RUNNING when this
// returns; the caller decides whether to wait for it.
func (h *Harness) spawnAgent(ctx context.Context, prompt, provider, model, mode string) (*subAgent, error) {
	if provider == "" && model == "" {
		provider, model = h.cfg.ChildProvider, h.cfg.ChildModel
	}
	run, err := h.clientFor(provider, model)
	if err != nil {
		return nil, err
	}
	iso, err := h.newIsolation(mode)
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

	sa := &subAgent{num: num, prompt: prompt, model: run.model, provider: run.provider, parent: h, hb: newHeartbeat(), iso: iso}

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
	h.agentsChanged()

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

	res, err := h.Ask(ctx, prompt)
	switch {
	case ctx.Err() != nil:
		s.settle(agentDismissed)
		return
	case err != nil:
		s.setFinal("FAILED: " + err.Error())
	default:
		// A child that answered in prose without calling tell_parent still
		// answered. Silence is distinguished from failure; an unreported answer
		// is not.
		if reply := strings.TrimSpace(res.Reply); reply != "" && s.finalIsEmpty() {
			s.setFinal(oneLine(reply, 2000))
		}
	}
	s.settleReporting(agentIdle)
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
func (s *subAgent) tell(msg string) string {
	s.mu.Lock()
	if s.state == agentRunning {
		s.mu.Unlock()
		return fmt.Sprintf("agent #%d is still running — wait for its report before telling it something new "+
			"(jobs(agent:%d, wait:true)).", s.num, s.num)
	}
	if s.h == nil {
		s.mu.Unlock()
		return fmt.Sprintf("agent #%d is stopped. Restart it with jobs(agent:%d, resume:true) first.", s.num, s.num)
	}
	// A new task is a new RUN: the clock and the answer both reset, or the next
	// report describes the previous task.
	s.final = ""
	s.startRunLocked()
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
		res, err := h.Ask(ctx, msg)
		if err != nil {
			s.setFinal("FAILED: " + err.Error())
		} else if reply := strings.TrimSpace(res.Reply); reply != "" && s.finalIsEmpty() {
			s.setFinal(oneLine(reply, 2000))
		}
		s.settleReporting(agentIdle)
	}()
	return fmt.Sprintf("Told agent #%d. It is working; you will hear when it is done "+
		"(or jobs(agent:%d, wait:true)).", s.num, s.num)
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
			prompt: rec.Prompt, iso: loadIsolation(path), model: rec.Model, provider: rec.Provider,
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
	return resumed, cut
}

// agentRecord is what a child's sidecar (<sub>.agent.json) keeps of it for a
// resume: its state when last settled, its model and its task.
type agentRecord struct {
	State    string `json:"state"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"` // "" is the parent session's
	Prompt   string `json:"prompt,omitempty"`   // as asked: the transcript's first message has the isolation brief too
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
	rec, closed := agentRecord{State: string(s.state), Model: s.model, Provider: s.provider, Prompt: s.prompt}, s.closed
	path := s.restore
	if s.h != nil {
		path = s.h.cfg.SessionFile
	}
	s.mu.Unlock()
	if closed || path == "" {
		return
	}
	if b, err := json.Marshal(rec); err == nil {
		_ = writeFileAtomic(agentFile(path), b, 0o600)
	}
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
	provider, model := sa.provider, sa.model
	sa.mu.Unlock()

	run, err := h.clientFor(provider, model)
	if err != nil {
		return fmt.Sprintf("agent #%d could not be resumed: %v", sa.num, err)
	}
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
	sa.h, sa.cancel, sa.model, sa.provider = child, cancel, run.model, run.provider
	// IDLE, not running, and with a ZEROED clock: a restored child has not been
	// asked anything yet, and leaving `started` set made it show a growing age
	// in the field that means "how long its last run took".
	sa.state = agentIdle
	sa.started, sa.ended = time.Time{}, time.Time{}
	sa.mu.Unlock()
	sa.hook(child)
	sa.saveState()
	h.agentsChanged()
	return fmt.Sprintf("agent #%d is back, idle, with its transcript loaded. "+
		"Give it work with jobs(agent:%d, tell:\"…\").", sa.num, sa.num)
}

// closeAgents stops every child. Called before the parent's servers go, because
// a child left running against closed tools is worse than a stopped one.
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
			strings.Join(names, ", ")+". Give one work, or tell it to carry on, with jobs(agent:N, tell:\"…\").")
	}
	var stopped []string
	for _, sa := range h.agentList() {
		if state, _, _, _ := sa.snapshot(); state == agentStopped {
			stopped = append(stopped, fmt.Sprintf("#%d (%s)", sa.num, oneLine(sa.prompt, 80)))
		}
	}
	if len(stopped) > 0 {
		parts = append(parts, "Sub-agents from the resumed session are STOPPED — their transcripts survived, their processes did not: "+
			strings.Join(stopped, ", ")+". Restart one with jobs(agent:N, resume:true) if you still need it.")
	}
	return strings.Join(parts, "\n")
}

// roleSystem is the part of the system prompt that depends on which side of the
// relationship this harness is on. A child is told its role; a root has
// nothing to add to the agent tool's description.
func roleSystem(child bool) string {
	if child {
		return "\n\nYou are a SUB-AGENT working for another agent, not for a human. Do the task you were given and " +
			"nothing else. Report with tell_parent: set `status` as you go (it overwrites, so keep it short and " +
			"current), and call it with `final` when you are done — `final` is the ANSWER, and it is the only " +
			"thing your parent reliably reads, so make it complete on its own. Everything you looked at to " +
			"produce it stays in your context and dies with you; that is the point of you. To ask your " +
			"parent something, end your turn with the question; its answer arrives as your next message. " +
			"You cannot reach the human and you cannot spawn agents of your own. If the task turns out to " +
			"need either, say so in `final` rather than working around it."
	}
	// A root's side is the agent tool's description: said once, there.
	return ""
}

// ── the tools ───────────────────────────────────────────────────────

// agentToolDef is the agent tool. providers are the logged-in ones; with
// more than one, a spawn may name another.
func agentToolDef(defaultModel, defaultProvider string, providers []string) llm.ToolDef {
	model := "the session's child model"
	if defaultModel != "" {
		model = defaultModel
	}
	if defaultProvider != "" {
		model += " on provider " + defaultProvider
	}
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "agent"
	td.Function.Description = "Delegate a task to a sub-agent: for work that would cost you a lot of context for a " +
		"small answer (reading a long log, scanning a large file). It has your tools, spends its own context, and " +
		"returns an answer; what it read never enters yours. It returns at once and you are notified each time " +
		"the child ends a turn, unless wait:true. If it ends with a question, answer with " +
		"jobs(agent:N, tell:\"…\"). It stays resident until you dismiss it with jobs(agent:N, kill:true); " +
		"nothing cleans it up for you. Default model: " + model + "."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{"type": "string", "description": "the task, complete on its own — the child cannot see your conversation"},
			"model":  map[string]any{"type": "string", "description": "run this child on a specific model"},
			"wait":   map[string]any{"type": "boolean", "description": "block until it answers, instead of being notified later"},
			"isolation": map[string]any{"type": "string", "enum": []string{IsoNone, IsoWorktree, IsoDocker, IsoNoDir},
				"description": "where the child works. none (default): this tree. worktree: its own git worktree branched " +
					"from this tree's last commit (uncommitted changes are not in it); it commits there and you merge its " +
					"branch — use it for children that edit at the same time. docker: the same, its commands in a " +
					"container. nodir: no project, no file tools; for research and summaries."},
		},
		"required": []string{"prompt"},
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
			Prompt    string `json:"prompt"`
			Provider  string `json:"provider"`
			Model     string `json:"model"`
			Wait      bool   `json:"wait"`
			Isolation string `json:"isolation"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		out := spawnTool(ctx, h, args.Prompt, args.Provider, args.Model, args.Isolation, args.Wait)
		if onCall != nil {
			call := map[string]any{"prompt": args.Prompt, "model": args.Model, "wait": args.Wait}
			if args.Provider != "" {
				call["provider"] = args.Provider
			}
			onCall("agent", call, out)
		}
		return out, nil
	}
}

func spawnTool(ctx context.Context, h *Harness, prompt, provider, model, isolation string, wait bool) string {
	if strings.TrimSpace(prompt) == "" {
		return "ERROR: agent requires a non-empty prompt."
	}
	sa, err := h.spawnAgent(ctx, prompt, provider, model, isolation)
	if err != nil {
		return "ERROR: could not start the agent: " + err.Error()
	}
	if !wait {
		where := ""
		if sa.iso.isolated() {
			where = " in " + sa.iso.Mode + " isolation at " + sa.iso.Path
			if sa.iso.Branch != "" {
				where += " (branch " + sa.iso.Branch + ")"
			}
		}
		// No model name when the child runs on the parent's own client: it
		// read "Started agent #1 on ." live.
		if w := sa.where(); w != "" {
			where = " on " + w + where
		}
		return fmt.Sprintf("Started agent #%d%s. It is working; you will be told when it finishes or asks you something. "+
			"Do other work, or jobs(agent:%d, wait:true) if you have nothing to do until it answers.",
			sa.num, where, sa.num)
	}
	select {
	case <-sa.waitCh():
	case <-ctx.Done():
		return fmt.Sprintf("agent #%d is still running; the turn was cancelled.", sa.num)
	case <-h.userWaiting():
		return fmt.Sprintf("agent #%d is still running; stopped waiting because a message arrived (the user wrote, or another dun session did) — "+
			"it follows. You will be told when it finishes.", sa.num)
	}
	return sa.report()
}

// agentMonitor is the agent half of the jobs tool (jobs.go): list, or one
// child's report after a kill, resume, tell or wait.
func agentMonitor(ctx context.Context, h *Harness, id *int, tail int, tell string, wait, resume, kill bool) string {
	if id == nil {
		agents := h.agentList()
		if len(agents) == 0 {
			return "No sub-agents. Start one with agent({prompt:…})."
		}
		lines := make([]string, 0, len(agents))
		for _, sa := range agents {
			lines = append(lines, sa.line())
		}
		return strings.Join(lines, "\n")
	}
	sa := h.agentByID(*id)
	if sa == nil {
		return fmt.Sprintf("ERROR: no agent #%d.", *id)
	}
	if kill {
		return sa.quit()
	}
	if resume {
		return h.resumeAgent(sa)
	}

	var b strings.Builder
	if tell != "" {
		b.WriteString(sa.tell(tell) + "\n")
	}
	if wait {
		// No poll for a question: a child asks by ENDING its turn, which
		// closes waitCh like any other turn end.
		select {
		case <-sa.waitCh():
		case <-ctx.Done():
		case <-h.userWaiting():
			// A message arrived: stop waiting so it rides this
			// result. The child keeps running.
			b.WriteString("Stopped waiting because a message arrived (the user wrote, or another dun session did) — it follows.\n")
		}
	}
	b.WriteString(sa.report())
	if tail > 0 {
		b.WriteString("\n\nlast " + strconv.Itoa(tail) + " transcript lines:\n" + sa.peek(tail))
	}
	return b.String()
}

func tellParentToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "tell_parent"
	td.Function.Description = "Report to the agent that spawned you. `status` OVERWRITES — it is what you are doing " +
		"right now, not a log, and it never interrupts your parent. `message` is an event worth its attention. " +
		"`final` is your ANSWER: everything your parent needs, standing on its own, because what you read to " +
		"produce it is not visible to anyone else."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":  map[string]any{"type": "string", "description": "what you are doing now (overwrites the last one)"},
			"message": map[string]any{"type": "string", "description": "something your parent should know now"},
			"final":   map[string]any{"type": "string", "description": "your answer — complete on its own"},
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
	ID      int    `json:"id"`
	Model   string `json:"model,omitempty"`
	State   string `json:"state"`
	Status  string `json:"status,omitempty"`
	Prompt  string `json:"prompt"`
	Tokens  int    `json:"tokens"`
	Seconds int    `json:"seconds"`
}

// Agents is every child of this session, oldest first.
func (h *Harness) Agents() []AgentInfo {
	list := h.agentList()
	out := make([]AgentInfo, 0, len(list))
	for _, sa := range list {
		state, status, _, since := sa.snapshot()
		out = append(out, AgentInfo{
			ID: sa.num, Model: sa.where(), State: string(state), Status: status,
			Prompt: sa.prompt, Tokens: sa.spent(), Seconds: int(since.Seconds()),
		})
	}
	return out
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
// the parent's jobs(agent:N, tell:) makes — deliberately the same code path, so
// there is one definition of what telling a child means.
func (h *Harness) AgentTell(id int, text string) string {
	sa := h.agentByID(id)
	if sa == nil {
		return fmt.Sprintf("no agent #%d", id)
	}
	out := sa.tell(text)
	if strings.HasPrefix(out, "Told agent") {
		// The parent reads the child's next report, and without this it
		// is an answer to something it never asked (live: "a leftover
		// instruction from its pre-restart transcript"). An aside: news
		// for the turn that report starts, not a turn of its own.
		h.Aside(fmt.Sprintf("The user told agent #%d directly: %q. Its next report answers that.", id, oneLine(text, 300)))
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
