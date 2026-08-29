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
// and gets no ship and no ask_user.
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
	num    int
	prompt string
	model  string
	parent *Harness

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
	// answer is non-nil exactly while the child is blocked in ask_parent, which
	// is the one state that never resolves itself.
	answer   chan string
	question string
	done     chan struct{} // closed when the current run settles
	restore  string        // transcript path, for a stopped child
	// hb is the "still there" reminder (heartbeat.go), debounced by anything
	// this child says — which is why every notification goes through s.notify.
	hb *heartbeat
	// lastActivity is the last time the child showed any sign of life inside
	// its harness: a tool call, a chat round completing, or a transcript entry.
	// This is the cheap signal that distinguishes "working" from "wedged" —
	// a child blocked in Go (compaction that never returns, a channel read
	// with no writer) will stop updating this while remaining agentRunning.
	lastActivity time.Time
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
// "idle for 40 minutes" is precisely the shape of a leak. A child blocked on
// ask_parent is the loudest case: it will never resolve itself, and the parent
// is the only thing that can.
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
			case s.blockedOn() != "":
				what = "is WAITING FOR YOU to answer it"
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
			case s.blockedOn() != "":
				line += fmt.Sprintf("\nIt asked: %s\nAnswer with agent_monitor(agent:%d, tell:\"…\").",
					s.blockedOn(), s.num)
			case state == agentIdle:
				line += fmt.Sprintf("\nIt is holding its context open. Give it work with "+
					"agent_monitor(agent:%d, tell:\"…\") or dismiss it with quit:true.", s.num)
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
						"agent_monitor(agent:%d, tail:40) to see what it was doing.",
						roundDur(inactive), s.num)
				} else {
					line += fmt.Sprintf("\nNothing is necessarily wrong; this is so a wedged agent does not "+
						"look like a working one. agent_monitor(agent:%d, tail:40) to see what it is doing.", s.num)
				}
			}
			// Not s.notify: the reminder must not reset the silence it is
			// reporting on.
			//
			// A BLOCKED child is the one reminder that has to buy a turn. It is
			// waiting on an answer only the parent can give, so a note the parent
			// reads "next time it runs a turn anyway" is a note it may never read
			// — the child waits forever for a parent that has nothing else to do.
			// Every other case is the absence of news, and pays nothing for it.
			if s.blockedOn() != "" {
				s.parent.notifyAndWake(line)
				return
			}
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
func (s *subAgent) settle(state agentState) {
	s.mu.Lock()
	s.state = state
	if s.ended.IsZero() {
		s.ended = time.Now()
	}
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
	if status != "" {
		out += " — " + status
	}
	return out
}

// report is what the parent's model reads when a child goes idle, and what
// agent_monitor answers with. It leads with the ANSWER, because that is the
// only reason the child was spawned.
func (s *subAgent) report() string {
	state, status, final, since := s.snapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "agent #%d is %s after %s (%s)", s.num, state, roundDur(since), tokenCount(s.spent()))
	if s.model != "" {
		fmt.Fprintf(&b, " on %s", s.model)
	}
	b.WriteString("\ntask: " + oneLine(s.prompt, 200))
	if status != "" {
		b.WriteString("\nstatus: " + status)
	}
	if final != "" {
		b.WriteString("\nanswer: " + final)
	} else if state == agentIdle {
		b.WriteString("\nanswer: (it stopped without calling tell_parent — use agent_monitor(agent:" +
			strconv.Itoa(s.num) + ", tail:40) to read what it did)")
	}
	if q := s.blockedOn(); q != "" {
		b.WriteString("\nBLOCKED, waiting on you: " + q +
			"\nAnswer with agent_monitor(agent:" + strconv.Itoa(s.num) + ", tell:\"…\").")
	}
	b.WriteString("\nIt is still resident and re-askable — agent_monitor(agent:" + strconv.Itoa(s.num) +
		", tell:\"…\"), or quit:true when you are done with it.")
	return b.String()
}

func (s *subAgent) blockedOn() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answer == nil {
		return ""
	}
	return s.question
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

// childConfig derives a child's Config from its parent's.
//
// Everything cheap is inherited and everything expensive is dropped: the
// parent's worktree (so no new tree and no new servers), its exec backend, its
// tool servers. Ship and ask_user are NOT inherited — landing work and
// interrupting the human are the root's, which is the same rule ship.allow
// already encodes.
func childConfig(parent *Harness, num int, model string, client agent.LLMRunner) Config {
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
	cfg.Client = client
	cfg.ChildModel = model
	cfg.SessionFile = childSessionFile(parent.cfg.SessionFile, num)
	cfg.Servers = parent.specs // shared manager; nothing is spawned from these
	cfg.EnableShip = false
	cfg.Ask = nil
	// A child's tokens, retries and compactions are its own business. Forwarding
	// them to the parent's UI would interleave N children's streams into one
	// conversation, which is the context re-merge the split exists to prevent.
	cfg.OnToken = nil
	cfg.OnToolCall = nil
	cfg.OnNotify = nil
	cfg.OnDocs = nil
	cfg.OnCompaction = nil
	cfg.OnAgents = nil
	cfg.OnJobs = nil
	return cfg
}

// clientFor resolves the runner for a named model, falling back to the parent's
// own. The name is returned too, so the report can say what it actually ran on
// rather than what was asked for.
func (h *Harness) clientFor(model string) (agent.LLMRunner, string) {
	if model == "" || h.cfg.ClientFor == nil {
		return h.cfg.Client, h.cfg.ChildModel
	}
	c, err := h.cfg.ClientFor(model)
	if err != nil || c == nil {
		return h.cfg.Client, h.cfg.ChildModel
	}
	return c, model
}

// spawnAgent starts a child and registers it. The child is RUNNING when this
// returns; the caller decides whether to wait for it.
func (h *Harness) spawnAgent(ctx context.Context, prompt, model string) (*subAgent, error) {
	h.agMu.Lock()
	h.agSeq++
	num := h.agSeq
	if h.agents == nil {
		h.agents = map[int]*subAgent{}
	}
	h.agMu.Unlock()

	if model == "" {
		model = h.cfg.ChildModel
	}
	client, name := h.clientFor(model)
	sa := &subAgent{num: num, prompt: prompt, model: name, parent: h, hb: newHeartbeat()}

	// Start on a background context: a child outlives the TURN that spawned it
	// (that is the whole point of an async spawn), so tying it to the caller's
	// context would kill it the moment the parent's turn ended.
	runCtx, cancel := context.WithCancel(context.Background())
	child, err := Start(runCtx, childConfig(h, num, name, client))
	if err != nil {
		cancel()
		return nil, err
	}
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
	h.agentsChanged()

	go sa.run(runCtx, prompt)
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
	// Release anyone blocked on ask_parent: a child that has stopped will never
	// answer, and a parent waiting on it would wait forever.
	s.mu.Lock()
	s.releaseAskLocked()
	s.mu.Unlock()

	s.settle(agentIdle)
	s.notify(s.report())
}

func (s *subAgent) finalIsEmpty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.final == ""
}

// releaseAskLocked closes an outstanding ask_parent. Caller holds mu.
func (s *subAgent) releaseAskLocked() {
	if s.answer != nil {
		close(s.answer)
		s.answer, s.question = nil, ""
	}
}

// ask blocks the CHILD until the parent answers via agent_monitor(tell:). The
// bool is false when the child was cancelled or the session ended instead.
func (s *subAgent) ask(ctx context.Context, question string) (string, bool) {
	ch := make(chan string, 1)
	s.mu.Lock()
	s.releaseAskLocked() // a second question replaces the first
	s.answer, s.question = ch, question
	s.mu.Unlock()
	// Blocked is the one state that never resolves itself, so it is pushed to
	// the human immediately rather than waiting for the next poll.
	s.parent.agentsChanged()
	s.notify(fmt.Sprintf("agent #%d is BLOCKED waiting on you: %s\n"+
		"Answer with agent_monitor(agent:%d, tell:\"…\").", s.num, question, s.num))

	select {
	case ans, ok := <-ch:
		return ans, ok
	case <-ctx.Done():
		s.mu.Lock()
		s.releaseAskLocked()
		s.mu.Unlock()
		return "", false
	}
}

// tell re-asks a resident child, or answers the question it is blocked on.
// Those are the same gesture from the parent's side, which is why one argument
// covers both.
func (s *subAgent) tell(msg string) string {
	s.mu.Lock()
	if s.answer != nil {
		s.answer <- msg
		s.answer, s.question = nil, ""
		s.mu.Unlock()
		s.parent.agentsChanged()
		return fmt.Sprintf("Answered agent #%d. It is working again.", s.num)
	}
	if s.state == agentRunning {
		s.mu.Unlock()
		return fmt.Sprintf("agent #%d is still running — wait for its report before telling it something new "+
			"(agent_monitor(agent:%d, wait:true)).", s.num, s.num)
	}
	if s.h == nil {
		s.mu.Unlock()
		return fmt.Sprintf("agent #%d is stopped. Restart it with agent_monitor(agent:%d, resume:true) first.", s.num, s.num)
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
	s.parent.agentsChanged()

	go func() {
		res, err := h.Ask(ctx, msg)
		if err != nil {
			s.setFinal("FAILED: " + err.Error())
		} else if reply := strings.TrimSpace(res.Reply); reply != "" && s.finalIsEmpty() {
			s.setFinal(oneLine(reply, 2000))
		}
		s.settle(agentIdle)
		s.notify(s.report())
	}()
	return fmt.Sprintf("Told agent #%d. It is working; you will hear when it is done "+
		"(or agent_monitor(agent:%d, wait:true)).", s.num, s.num)
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
	s.releaseAskLocked()
	s.mu.Unlock()
	s.stop()
	if h != nil {
		h.Close()
	}
	s.settle(agentDismissed)
	if path := s.transcript(); path != "" {
		return fmt.Sprintf("Dismissed agent #%d. Its transcript is at %s.", s.num, path)
	}
	return fmt.Sprintf("Dismissed agent #%d.", s.num)
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
// are gone; their transcripts are not.
func (h *Harness) restoreAgents() {
	base := h.cfg.SessionFile
	if base == "" {
		return
	}
	matches, _ := filepath.Glob(strings.TrimSuffix(base, ".jsonl") + ".sub*.jsonl")
	sort.Strings(matches)
	h.agMu.Lock()
	if h.agents == nil {
		h.agents = map[int]*subAgent{}
	}
	for _, path := range matches {
		num, ok := subNumFromPath(base, path)
		if !ok || h.agents[num] != nil {
			continue
		}
		sa := &subAgent{
			num: num, parent: h, state: agentStopped, restore: path,
			prompt: firstUserEntry(path),
		}
		h.agents[num] = sa
		if num > h.agSeq {
			h.agSeq = num
		}
	}
	h.agMu.Unlock()
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
	model := sa.model
	sa.mu.Unlock()

	client, name := h.clientFor(model)
	ctx, cancel := context.WithCancel(context.Background())
	child, err := Start(ctx, childConfig(h, sa.num, name, client))
	if err != nil {
		cancel()
		return fmt.Sprintf("agent #%d could not be resumed: %v", sa.num, err)
	}
	child.self = sa
	child.cfg.OnToolCall = func(string, map[string]any, string) {
		sa.noteActivity()
	}

	sa.mu.Lock()
	sa.h, sa.cancel, sa.model = child, cancel, name
	// IDLE, not running, and with a ZEROED clock: a restored child has not been
	// asked anything yet, and leaving `started` set made it show a growing age
	// in the field that means "how long its last run took".
	sa.state = agentIdle
	sa.started, sa.ended = time.Time{}, time.Time{}
	sa.mu.Unlock()
	h.agentsChanged()
	return fmt.Sprintf("agent #%d is back, idle, with its transcript loaded. "+
		"Give it work with agent_monitor(agent:%d, tell:\"…\").", sa.num, sa.num)
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
		sa.stop()
		sa.mu.Lock()
		child := sa.h
		sa.h = nil
		sa.releaseAskLocked()
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

// stoppedAgentsNote tells a resumed session which children it used to have.
func (h *Harness) stoppedAgentsNote() string {
	var stopped []string
	for _, sa := range h.agentList() {
		if state, _, _, _ := sa.snapshot(); state == agentStopped {
			stopped = append(stopped, fmt.Sprintf("#%d (%s)", sa.num, oneLine(sa.prompt, 80)))
		}
	}
	if len(stopped) == 0 {
		return ""
	}
	return "Sub-agents from the resumed session are STOPPED — their transcripts survived, their processes did not: " +
		strings.Join(stopped, ", ") + ". Restart one with agent_monitor(agent:N, resume:true) if you still need it."
}

// roleSystem is the part of the system prompt that depends on which side of the
// relationship this harness is on.
func roleSystem(child bool) string {
	if child {
		return "\n\nYou are a SUB-AGENT working for another agent, not for a human. Do the task you were given and " +
			"nothing else. Report with tell_parent: set `status` as you go (it overwrites, so keep it short and " +
			"current), and call it with `final` when you are done — `final` is the ANSWER, and it is the only " +
			"thing your parent reliably reads, so make it complete on its own. Everything you looked at to " +
			"produce it stays in your context and dies with you; that is the point of you. If you need a " +
			"decision only your parent can make, ask_parent blocks until it answers. " +
			"You cannot reach the human and you cannot spawn agents of your own. If the task turns out to " +
			"need either, say so in `final` rather than working around it."
	}
	return "\n\nYou can delegate to SUB-AGENTS with `agent`. Use one when the work would cost you a lot of context " +
		"for a small answer — reading a long log, scanning a large file, checking something you only need one " +
		"line from. The child spends its own context and returns a summary; what it read never enters yours. " +
		"Spawning is asynchronous: keep working and you will be told when it finishes, or pass wait:true when " +
		"you have nothing to do until then. Children stay resident and re-askable — agent_monitor(agent:N, " +
		"tell:\"…\") gives one more work, quit:true dismisses it. Dismiss them when you are done; nothing " +
		"cleans them up for you."
}

// ── the tools ───────────────────────────────────────────────────────

func agentToolDef(defaultModel string) llm.ToolDef {
	model := "the session's child model"
	if defaultModel != "" {
		model = defaultModel
	}
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "agent"
	td.Function.Description = "Delegate a task to a sub-agent. It runs in this worktree with the same tools, spends " +
		"its OWN context, and reports back — so use it for work whose inputs are large and whose answer is " +
		"small. Returns immediately; you are notified when the child finishes, unless wait:true. Default model: " +
		model + "."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{"type": "string", "description": "the task, complete on its own — the child cannot see your conversation"},
			"model":  map[string]any{"type": "string", "description": "run this child on a specific model"},
			"wait":   map[string]any{"type": "boolean", "description": "block until it answers, instead of being notified later"},
		},
		"required": []string{"prompt"},
	}
	return td
}

func withAgent(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "agent" {
			return inner(ctx, tc)
		}
		var args struct {
			Prompt string `json:"prompt"`
			Model  string `json:"model"`
			Wait   bool   `json:"wait"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		out := spawnTool(ctx, h, args.Prompt, args.Model, args.Wait)
		if onCall != nil {
			onCall("agent", map[string]any{"prompt": args.Prompt, "model": args.Model, "wait": args.Wait}, out)
		}
		return out, nil
	}
}

func spawnTool(ctx context.Context, h *Harness, prompt, model string, wait bool) string {
	if strings.TrimSpace(prompt) == "" {
		return "ERROR: agent requires a non-empty prompt."
	}
	sa, err := h.spawnAgent(ctx, prompt, model)
	if err != nil {
		return "ERROR: could not start the agent: " + err.Error()
	}
	if !wait {
		return fmt.Sprintf("Started agent #%d on %s. It is working; you will be told when it finishes. "+
			"Carry on with something else, or agent_monitor(agent:%d, wait:true) if you have nothing to do "+
			"until it answers.", sa.num, sa.model, sa.num)
	}
	select {
	case <-sa.waitCh():
	case <-ctx.Done():
		return fmt.Sprintf("agent #%d is still running; the turn was cancelled.", sa.num)
	}
	return sa.report()
}

func agentMonitorToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "agent_monitor"
	td.Function.Description = "Check on sub-agents and steer them. With no `agent`, lists them all. With one, " +
		"reports its state, its spend and its answer. `tail` reads that many lines of its transcript — what it " +
		"actually did, not what it chose to report. `tell` gives a resident child new work, and is also how you " +
		"answer one that is blocked on ask_parent. `wait` blocks until it settles. `resume` restarts a stopped " +
		"child (one from a resumed session) from its transcript. `quit` dismisses it " +
		"for good when you no longer need it (agents stay resident until you do). An agent left over from a " +
		"finished task costs nothing to dismiss and is not free to keep."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent":  map[string]any{"type": "integer", "description": "agent id; omit to list every agent"},
			"tail":   map[string]any{"type": "integer", "description": "read this many lines from the end of its transcript"},
			"tell":   map[string]any{"type": "string", "description": "give it new work, or answer the question it is blocked on"},
			"wait":   map[string]any{"type": "boolean", "description": "block until it settles"},
			"resume": map[string]any{"type": "boolean", "description": "restart a stopped agent from its transcript"},
			"quit":   map[string]any{"type": "boolean", "description": "dismiss it for good"},
		},
	}
	return td
}

func withAgentMonitor(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "agent_monitor" {
			return inner(ctx, tc)
		}
		var args struct {
			Agent  *int   `json:"agent"`
			Tail   int    `json:"tail"`
			Tell   string `json:"tell"`
			Wait   bool   `json:"wait"`
			Resume bool   `json:"resume"`
			Quit   bool   `json:"quit"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		out := agentMonitor(ctx, h, args.Agent, args.Tail, args.Tell, args.Wait, args.Resume, args.Quit)
		if onCall != nil {
			a := map[string]any{}
			if args.Agent != nil {
				a["agent"] = *args.Agent
			}
			if args.Tell != "" {
				a["tell"] = args.Tell
			}
			onCall("agent_monitor", a, out)
		}
		return out, nil
	}
}

func agentMonitor(ctx context.Context, h *Harness, id *int, tail int, tell string, wait, resume, quit bool) string {
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
	if quit {
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
	waitLoop:
		for {
			ch := sa.waitCh()
			select {
			case <-ch:
				break waitLoop
			case <-ctx.Done():
				break waitLoop
			case <-time.After(2 * time.Second):
				// Poll: a child that called ask_parent is blocked on the
				// parent's answer, but the parent is blocked here — a mutual
				// deadlock with no auto-unblock. Breaking on a pending
				// question lets report() surface it so the model can answer
				// via agent_monitor(tell:) on the next turn.
				if sa.blockedOn() != "" {
					break waitLoop
				}
			}
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

func askParentToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "ask_parent"
	td.Function.Description = "Ask the agent that spawned you for a decision you cannot make. BLOCKS until it " +
		"answers, so use it only when you genuinely cannot continue — a question you could have answered by " +
		"looking is a question that cost your parent a turn."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "description": "what you need decided, with the context to decide it"},
		},
		"required": []string{"question"},
	}
	return td
}

func withAskParent(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "ask_parent" {
			return inner(ctx, tc)
		}
		var args struct {
			Question string `json:"question"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		out := askParent(ctx, h, args.Question)
		if onCall != nil {
			onCall("ask_parent", map[string]any{"question": args.Question}, out)
		}
		return out, nil
	}
}

func askParent(ctx context.Context, h *Harness, question string) string {
	self := h.self
	if self == nil || h.parent == nil {
		return "ERROR: you have no parent to ask."
	}
	if strings.TrimSpace(question) == "" {
		return "ERROR: ask_parent requires a question."
	}
	ans, ok := self.ask(ctx, question)
	if !ok {
		return "Your parent did not answer (the session ended or you were dismissed). " +
			"Do the best you can and report what you decided with tell_parent."
	}
	return "Your parent says: " + ans
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
	// Blocked means it is waiting on an ask_parent answer. Called out on its own
	// because it is the one state that never resolves itself.
	Blocked bool `json:"blocked,omitempty"`
}

// Agents is every child of this session, oldest first.
func (h *Harness) Agents() []AgentInfo {
	list := h.agentList()
	out := make([]AgentInfo, 0, len(list))
	for _, sa := range list {
		state, status, _, since := sa.snapshot()
		out = append(out, AgentInfo{
			ID: sa.num, Model: sa.model, State: string(state), Status: status,
			Prompt: sa.prompt, Tokens: sa.spent(), Seconds: int(since.Seconds()),
			Blocked: sa.blockedOn() != "",
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

// AgentTell is the human steering a child directly, which is the same gesture
// the parent's agent_monitor(tell:) makes — deliberately the same code path, so
// there is one definition of what telling a child means.
func (h *Harness) AgentTell(id int, text string) string {
	sa := h.agentByID(id)
	if sa == nil {
		return fmt.Sprintf("no agent #%d", id)
	}
	return sa.tell(text)
}

// AgentPrompt is what a child was asked to do, for the UI's task line.
func (h *Harness) AgentPrompt(id int) string {
	if sa := h.agentByID(id); sa != nil {
		return sa.prompt
	}
	return ""
}
