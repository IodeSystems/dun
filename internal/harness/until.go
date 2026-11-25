package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"

	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

// Stop conditions: "you're dun when…" (USER 2026-11-24; docs/decisions.md,
// "Stop conditions"). /until sets a session's conditions; while a run is
// under way the engine starts the next turn itself, until one condition
// fires or a safety stop does.
//
//   - Conditions, any number at once: turns N (N turns have run), goal
//     <text> [--check <cmd>] (the goal is met), plan (no plan node is ready
//     or under way). The run stops at the first that fires. goal or plan
//     without turns stops at untilDefaultCap turns.
//   - Safety stops, always: the person interrupted, the turn failed, a
//     question was put to the person in the turn (ask_user, a file-access
//     question), and untilStallTurns turns in a row with no progress.
//   - A run starts with the next turn that begins (the person's message is
//     turn 1, or the turn running when /until was given). The conditions
//     stay after a stop: the next turn starts a new run. /until off clears
//     them; the turn running ends as usual.
//   - Kept in <session>.until.json: a run under way when the engine
//     stopped (a restart, a model switch) goes on when it starts again.
//   - The engine's own turns are user-set (autoturn.go): not under the
//     automatic-turn budget, and ask_user is not refused in them.

const (
	untilDefaultCap  = 20
	untilMaxTurns    = 1000
	untilStallTurns  = 2
	untilCheckTimout = 10 * time.Minute
	// untilJudgeMaxTokens bounds the judge's reply, its thinking
	// included (reasoningBudget).
	untilJudgeMaxTokens = 2048
	untilReplyMax       = 6000 // the reply the judge reads, in bytes
	untilCheckOutMax    = 2000 // the check's output a continue note quotes
)

// Condition kinds.
const (
	UntilTurns = proto.UntilTurns
	UntilGoal  = proto.UntilGoal
	UntilPlan  = proto.UntilPlan
)

// UntilCond is one stop condition.
type UntilCond struct {
	Kind  string `json:"kind"`
	N     int    `json:"n,omitempty"`     // turns
	Goal  string `json:"goal,omitempty"`  // goal
	Check string `json:"check,omitempty"` // goal: a command whose exit 0 is "met"
}

// untilState is the session's conditions and the run's progress. The
// root session's only.
type untilState struct {
	mu    sync.Mutex
	conds []UntilCond
	// A run: started by the first turn after the conditions were set or
	// the last run stopped.
	running bool
	turn    int // turns of the run begun
	stalls  int // turns in a row with no progress
	owed    bool
	stopped string // why the last run stopped
	// The last verdicts, for the progress line.
	goalNote string
	goalMet  bool
	// The turn in flight, or the last one.
	inTurn      bool
	pendingEval bool // a turn of the run ended and was not looked at
	endErr      string
	endCut      bool // interrupted with a reason (a restart, a clear): not the person
	endInt      bool // the person interrupted it
	reply       string
	calls0      uint64
	asks0       int64
	plan0       string
	// next: the Continue about to run is the run's (UntilContinue).
	next bool
	// asks counts questions put to the person (Config.Ask).
	asks atomic.Int64
}

// untilFile is the sidecar.
func untilFile(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".until.json"
}

type untilDisk struct {
	Conds   []UntilCond `json:"conds,omitempty"`
	Running bool        `json:"running,omitempty"`
	Turn    int         `json:"turn,omitempty"`
	Stalls  int         `json:"stalls,omitempty"`
	Stopped string      `json:"stopped,omitempty"`
	Goal    string      `json:"goal_note,omitempty"`
}

// untilSave writes the sidecar; u.mu is held.
func (h *Harness) untilSave(u *untilState) {
	if h.cfg.SessionFile == "" || h.isChild() {
		return
	}
	f := untilFile(h.cfg.SessionFile)
	if len(u.conds) == 0 {
		_ = os.Remove(f)
		return
	}
	b, _ := json.Marshal(untilDisk{Conds: u.conds, Running: u.running, Turn: u.turn, Stalls: u.stalls, Stopped: u.stopped, Goal: u.goalNote})
	_ = fsutil.WriteAtomic(f, append(b, '\n'), 0o600)
}

// untilResume reads the sidecar at start. A run under way when the engine
// stopped is owed its next turn.
func (h *Harness) untilResume() {
	if h.cfg.SessionFile == "" || h.isChild() {
		return
	}
	b, err := os.ReadFile(untilFile(h.cfg.SessionFile))
	if err != nil {
		return
	}
	var d untilDisk
	if json.Unmarshal(b, &d) != nil {
		return
	}
	u := &h.until
	u.mu.Lock()
	u.conds, u.running, u.turn, u.stalls, u.stopped, u.goalNote = d.Conds, d.Running && len(d.Conds) > 0, d.Turn, d.Stalls, d.Stopped, d.Goal
	u.owed = u.running
	u.mu.Unlock()
}

// untilWrapAsk counts the questions Config.Ask puts to the person. Called
// once, in New, for the root session.
func (h *Harness) untilWrapAsk() {
	ask := h.cfg.Ask
	if ask == nil || h.isChild() {
		return
	}
	h.cfg.Ask = func(ctx context.Context, q string, opts []string, multi, noCustom bool, rec []string) (string, error) {
		h.until.asks.Add(1)
		return ask(ctx, q, opts, multi, noCustom, rec)
	}
}

func (h *Harness) untilChanged() {
	if h.cfg.OnUntil != nil {
		h.cfg.OnUntil()
	}
}

// untilCap is the run's turn cap; u.mu is held.
func untilCap(conds []UntilCond) int {
	for _, c := range conds {
		if c.Kind == UntilTurns {
			return c.N
		}
	}
	return untilDefaultCap
}

func untilHas(conds []UntilCond, kind string) *UntilCond {
	for i := range conds {
		if conds[i].Kind == kind {
			return &conds[i]
		}
	}
	return nil
}

// untilTurnBegin notes a turn starting (turnBegin): the first of a run
// starts it.
func (h *Harness) untilTurnBegin() {
	if h.isChild() {
		return
	}
	var calls uint64
	if h.store != nil {
		calls = h.store.toolCallsNow()
	}
	plan := h.untilPlanKey()
	u := &h.until
	u.mu.Lock()
	u.inTurn, u.pendingEval = true, false
	u.calls0, u.asks0, u.plan0 = calls, u.asks.Load(), plan
	if len(u.conds) == 0 {
		u.mu.Unlock()
		return
	}
	if !u.running {
		u.running, u.turn, u.stalls, u.stopped, u.goalNote, u.goalMet = true, 0, 0, "", "", false
	}
	u.turn++
	u.owed = false
	h.untilSave(u)
	u.mu.Unlock()
	h.untilChanged()
}

// untilTurnEnd notes how a turn ended (after finishInterrupt).
func (h *Harness) untilTurnEnd(res agent.TurnResult, err error) {
	if h.isChild() {
		return
	}
	h.intMu.Lock()
	reason := h.intReason
	h.intMu.Unlock()
	u := &h.until
	u.mu.Lock()
	defer u.mu.Unlock()
	u.inTurn = false
	if !u.running {
		return
	}
	u.pendingEval = true
	u.endErr, u.endCut, u.endInt = "", false, false
	switch {
	case err == nil:
	case errors.Is(err, ErrInterrupted) && reason != "":
		u.endCut = true
	case errors.Is(err, ErrInterrupted):
		u.endInt = true
	default:
		u.endErr = textfmt.OneLine(err.Error())
	}
	u.reply = res.Reply
}

// untilStopReset: the conversation was cleared. A run under way stops; the
// conditions stay.
func (h *Harness) untilStopReset() {
	u := &h.until
	u.mu.Lock()
	if !u.running {
		u.mu.Unlock()
		return
	}
	u.running, u.owed, u.pendingEval, u.stopped = false, false, false, "the conversation was cleared"
	h.untilSave(u)
	u.mu.Unlock()
	h.untilChanged()
}

// UntilInterrupt is an interrupt that found no turn running: a run between
// its turns (its goal being judged) stops. It reports the stop's reason,
// "" when no run was under way.
func (h *Harness) UntilInterrupt() string {
	u := &h.until
	u.mu.Lock()
	if !u.running || u.inTurn {
		u.mu.Unlock()
		return ""
	}
	u.running, u.owed, u.pendingEval = false, false, false
	u.stopped = fmt.Sprintf("interrupted (after turn %d of %d)", u.turn, untilCap(u.conds))
	stopped := u.stopped
	h.untilSave(u)
	u.mu.Unlock()
	h.untilChanged()
	return stopped
}

// untilTakeNext reports whether the Continue starting now is the run's
// own (UntilContinue): a user-set turn.
func (h *Harness) untilTakeNext() bool {
	u := &h.until
	u.mu.Lock()
	defer u.mu.Unlock()
	n := u.next
	u.next = false
	return n
}

// UntilStep is what the engine does after a turn: Continue says to start
// the next turn of the run (UntilContinue), Stopped says the run stopped
// and why. Both empty: nothing to do (no run, or no turn ended since the
// last step). It runs the goal's check or judge, so it can take a while.
type UntilStep struct {
	Continue bool
	Stopped  string
}

// UntilNext decides the step after the turn that just ended, or the turn a
// run resumed after a restart is owed.
func (h *Harness) UntilNext(ctx context.Context) UntilStep {
	if h.isChild() {
		return UntilStep{}
	}
	u := &h.until
	u.mu.Lock()
	if !u.running || u.inTurn {
		u.mu.Unlock()
		return UntilStep{}
	}
	if !u.pendingEval {
		owed := u.owed
		u.mu.Unlock()
		return UntilStep{Continue: owed}
	}
	u.pendingEval = false
	conds := append([]UntilCond(nil), u.conds...)
	turn, capN := u.turn, untilCap(u.conds)
	endErr, endCut, endInt, reply := u.endErr, u.endCut, u.endInt, u.reply
	calls0, asks0, plan0 := u.calls0, u.asks0, u.plan0
	u.mu.Unlock()

	if endCut { // a restart or a clear cut it: the run goes on when the engine does
		u.mu.Lock()
		u.owed = u.running
		h.untilSave(u)
		u.mu.Unlock()
		return UntilStep{}
	}
	stop := ""
	switch {
	case endInt:
		stop = "interrupted"
	case endErr != "":
		stop = "the turn failed: " + endErr
	case u.asks.Load() > asks0:
		stop = "a question was put to the user"
	}
	var calls uint64
	if h.store != nil {
		calls = h.store.toolCallsNow() - calls0
	}
	planKey := h.untilPlanKey()
	planMoved := planKey != plan0
	if stop == "" && untilHas(conds, UntilPlan) != nil {
		stop = h.untilPlanStop(ctx)
	}
	goalNote, goalMet := "", false
	if g := untilHas(conds, UntilGoal); stop == "" && g != nil {
		goalMet, goalNote = h.untilGoal(ctx, *g, reply, int(calls))
		if goalMet {
			stop = "goal met"
			if goalNote != "" {
				stop += ": " + goalNote
			}
		}
	}
	if stop == "" && turn >= capN {
		stop = fmt.Sprintf("turn cap %d reached", capN)
	}
	// Progress: a node changed state (plan), or the turn made a tool call
	// (goal, turns).
	progress := false
	if untilHas(conds, UntilPlan) != nil && planMoved {
		progress = true
	}
	if (untilHas(conds, UntilGoal) != nil || untilHas(conds, UntilTurns) != nil) && calls > 0 {
		progress = true
	}
	u.mu.Lock()
	defer func() {
		h.untilSave(u)
		u.mu.Unlock()
		h.untilChanged()
	}()
	if !u.running { // /until off while the step ran
		return UntilStep{}
	}
	if untilHas(conds, UntilGoal) != nil {
		u.goalNote, u.goalMet = goalNote, goalMet
	}
	if stop == "" {
		if progress {
			u.stalls = 0
		} else {
			u.stalls++
		}
		if u.stalls >= untilStallTurns {
			stop = fmt.Sprintf("no progress in %d turns", u.stalls)
		}
	}
	if stop != "" {
		u.running, u.owed = false, false
		u.stopped = fmt.Sprintf("%s (turn %d of %d)", stop, turn, capN)
		return UntilStep{Stopped: u.stopped}
	}
	u.owed = true
	return UntilStep{Continue: true}
}

// UntilContinue readies the run's next turn: the note that says what it is
// for, as a notification the next Continue reads, which runs as user-set.
// The engine runs Continue next.
func (h *Harness) UntilContinue(ctx context.Context) {
	note := h.untilNote(ctx)
	u := &h.until
	u.mu.Lock()
	u.next = true
	u.mu.Unlock()
	h.Notify(note)
}

// untilNote is the continue note. Model-facing.
func (h *Harness) untilNote(ctx context.Context) string {
	u := &h.until
	u.mu.Lock()
	conds := append([]UntilCond(nil), u.conds...)
	turn, capN, goalNote := u.turn+1, untilCap(u.conds), u.goalNote
	u.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "[until] The user set stop conditions (/until): keep working toward them. This is turn %d of at most %d, started by dun; the user may not be watching.", turn, capN)
	if g := untilHas(conds, UntilGoal); g != nil {
		fmt.Fprintf(&b, " Goal: %s.", g.Goal)
		if goalNote != "" {
			fmt.Fprintf(&b, " Not met yet: %s.", goalNote)
		}
	}
	if untilHas(conds, UntilPlan) != nil {
		open, ready := h.untilPlanCounts(ctx)
		fmt.Fprintf(&b, " Plan: %d node(s) open, %d ready; work them and close each with plan_close.", open, ready)
	}
	b.WriteString(" When the work is done, say so in one or two sentences; when you need the user's decision, ask it.")
	return b.String()
}

// untilPlanKey is the plan's node states, to tell whether a turn changed one.
func (h *Harness) untilPlanKey() string {
	p := h.planNow()
	if p == nil || p.store == nil {
		return ""
	}
	g, err := p.store.View(context.Background())
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, n := range g.Nodes {
		b.WriteString(n.ID + ":" + string(n.Status) + ";")
	}
	return b.String()
}

// untilPlanCounts is the open nodes (todo, doing, blocked) and how many of
// them are ready or under way.
func (h *Harness) untilPlanCounts(ctx context.Context) (open, ready int) {
	p := h.planNow()
	if p == nil || p.store == nil {
		return 0, 0
	}
	g, err := p.store.View(ctx)
	if err != nil {
		return 0, 0
	}
	for _, n := range g.Nodes {
		switch n.Status {
		case taskgraph.Todo, taskgraph.Blocked:
			open++
			if ok, _ := g.CanStart(n.ID); ok && n.Status == taskgraph.Todo {
				ready++
			}
		case taskgraph.Doing:
			open++
			ready++
		}
	}
	return open, ready
}

// untilPlanStop is the plan condition's stop, "" when it does not fire.
func (h *Harness) untilPlanStop(ctx context.Context) string {
	open, ready := h.untilPlanCounts(ctx)
	switch {
	case open == 0:
		if h.untilPlanKey() == "" {
			return "plan empty"
		}
		return "plan drained"
	case ready == 0:
		return fmt.Sprintf("plan: %d node(s) open, none ready", open)
	}
	return ""
}

// untilGoal judges the goal: the check's exit code when it has one, else a
// side call.
func (h *Harness) untilGoal(ctx context.Context, g UntilCond, reply string, calls int) (bool, string) {
	if g.Check != "" {
		return h.untilCheck(ctx, g.Check)
	}
	return h.untilJudge(ctx, g.Goal, reply, calls)
}

// untilCheck runs the check in the session's exec environment, in a
// subshell (a cd or an exit in it stays there), bounded by
// untilCheckTimout.
func (h *Harness) untilCheck(ctx context.Context, check string) (bool, string) {
	be := h.ExecBackend()
	if be == nil {
		return false, "the check did not run: this session has no exec"
	}
	cctx, cancel := context.WithTimeout(ctx, untilCheckTimout)
	defer cancel()
	r := be.Run(cctx, "(\n"+check+"\n)", nil)
	if !r.Failed() {
		return true, "the check passed"
	}
	why := fmt.Sprintf("the check exited %d", r.Code)
	switch {
	case cctx.Err() != nil:
		why = "the check timed out after " + untilCheckTimout.String()
	case r.Err != "":
		why = "the check did not run: " + r.Err
	}
	if out := strings.TrimSpace(r.Output); out != "" {
		if len(out) > untilCheckOutMax {
			out = "…" + safeCutFront(out[len(out)-untilCheckOutMax:])
		}
		why += "; its output ends:\n" + out
	}
	return false, why
}

// untilJudgePrompt is the judge's prompt. Model-facing.
func untilJudgePrompt(goal, reply string, calls int) string {
	if len(reply) > untilReplyMax {
		reply = safeCut(reply[:untilReplyMax]) + "…"
	}
	if strings.TrimSpace(reply) == "" {
		reply = "(no reply text)"
	}
	return "You check whether a coding agent has met its goal. Judge only from what is below; when it is not clear, it is not met.\n\n" +
		"Goal: " + goal + "\n\n" +
		"The agent's last turn made " + strconv.Itoa(calls) + " tool call(s) and ended with this reply:\n" + reply + "\n\n" +
		`Answer with one JSON object and nothing else: {"met": true or false, "reason": "<one short sentence>"}`
}

// untilJudge asks the model, outside the conversation, whether the goal is
// met. An error or an answer that does not parse is "not met".
func (h *Harness) untilJudge(ctx context.Context, goal, reply string, calls int) (bool, string) {
	if h.Session == nil || h.Session.Runner == nil {
		return false, "no model to judge the goal"
	}
	think := reasoningBudget(untilJudgeMaxTokens)
	opts := &llm.ChatOpts{MaxTokens: untilJudgeMaxTokens, ReasoningBudgetTokens: &think, ReasoningBudgetMessage: llm.DefaultBudgetMessage}
	start := time.Now()
	ch, err := h.sideRunner("until", untee(h.Session.Runner)).ChatStream(ctx,
		[]llm.Message{{Role: "user", Content: untilJudgePrompt(goal, reply, calls)}}, nil, opts)
	if err != nil {
		h.noteSideCall("until", start, nil)
		return false, "the judge failed: " + textfmt.OneLine(err.Error())
	}
	var out strings.Builder
	var usage *llm.Usage
	for c := range ch {
		if c.Error != "" {
			h.noteSideCall("until", start, nil)
			return false, "the judge failed: " + textfmt.OneLine(c.Error)
		}
		out.WriteString(c.Content)
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	h.noteSideCall("until", start, usage)
	return parseUntilVerdict(out.String())
}

// parseUntilVerdict reads the judge's {"met", "reason"}.
func parseUntilVerdict(s string) (bool, string) {
	i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if i < 0 || j <= i {
		return false, "the judge's answer was not JSON"
	}
	var v struct {
		Met    *bool  `json:"met"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(s[i:j+1]), &v) != nil || v.Met == nil {
		return false, "the judge's answer was not readable"
	}
	return *v.Met, textfmt.ClipRune(textfmt.OneLine(v.Reason), 200)
}

// ── the person's side ───────────────────────────────────────────────────

// UntilSnapshot is the conditions and the run, for a client.
type UntilSnapshot struct {
	Conds    []UntilCond
	Progress []string // one per condition: "3/10", "4 open, 2 ready", "not met: …"
	Cap      int
	Turn     int
	Running  bool
	Stopped  string
	Line     string // the whole of it in one short line, for a status bar
}

// Until is the session's conditions and the run's progress.
func (h *Harness) Until() UntilSnapshot {
	u := &h.until
	u.mu.Lock()
	s := UntilSnapshot{Conds: append([]UntilCond(nil), u.conds...), Turn: u.turn, Running: u.running, Stopped: u.stopped}
	goalNote, goalMet := u.goalNote, u.goalMet
	u.mu.Unlock()
	if len(s.Conds) == 0 {
		return s
	}
	s.Cap = untilCap(s.Conds)
	parts := []string{fmt.Sprintf("turn %d/%d", s.Turn, s.Cap)}
	for _, c := range s.Conds {
		p := ""
		switch c.Kind {
		case UntilTurns:
			if s.Running || s.Stopped != "" { // this run's, or the last one's
				p = fmt.Sprintf("%d/%d", s.Turn, c.N)
			}
		case UntilPlan:
			open, ready := h.untilPlanCounts(context.Background())
			p = fmt.Sprintf("%d open, %d ready", open, ready)
			parts = append(parts, "plan "+p)
		case UntilGoal:
			w := "goal not checked yet"
			switch {
			case goalMet:
				p, w = "met", "goal met"
			case goalNote != "":
				p, w = "not met: "+goalNote, "goal not met"
			}
			parts = append(parts, w)
		}
		s.Progress = append(s.Progress, p)
	}
	if s.Running {
		s.Line = "until: " + strings.Join(parts, " · ")
	} else {
		s.Line = "until " + textfmt.ClipRune(untilWords(s.Conds), 60)
	}
	return s
}

// untilWords are the conditions as /until takes them (proto.UntilLine).
func untilWords(conds []UntilCond) string {
	pc := make([]proto.UntilCond, len(conds))
	for i, c := range conds {
		pc[i] = proto.UntilCond{Kind: c.Kind, N: c.N, Goal: c.Goal, Check: c.Check}
	}
	return proto.UntilLine(pc)
}

// untilUsage is /until's argument form.
const untilUsage = `usage: /until [turns <N>] [goal <text> [--check "<cmd>"]] [plan] | off`

// UntilCmd is /until: "" the conditions and the run; "off" clears them;
// otherwise the conditions, which replace the ones set.
func (h *Harness) UntilCmd(action string) (string, error) {
	action = strings.TrimSpace(action)
	if action == "" {
		return h.untilStatus(), nil
	}
	conds, off, err := ParseUntil(action)
	if err != nil {
		return "", err
	}
	u := &h.until
	u.mu.Lock()
	if off {
		was := len(u.conds) > 0
		running := u.running
		u.conds, u.running, u.owed, u.pendingEval, u.stalls = nil, false, false, false, 0
		u.stopped, u.goalNote, u.goalMet = "", "", false
		if running {
			u.stopped = "/until off"
		}
		h.untilSave(u)
		inTurn := u.inTurn
		u.mu.Unlock()
		h.untilChanged()
		switch {
		case !was:
			return "until: off already (one turn per message)", nil
		case inTurn:
			return "until: off · the turn running ends as usual", nil
		}
		return "until: off (one turn per message)", nil
	}
	u.conds = conds
	if !u.running {
		u.stopped, u.goalNote, u.goalMet, u.stalls = "", "", false, 0
		if u.inTurn { // the turn running is the run's first
			u.running, u.turn = true, 1
		}
	}
	h.untilSave(u)
	running := u.running
	u.mu.Unlock()
	h.untilChanged()
	msg := "until: " + untilWords(conds)
	if untilHas(conds, UntilTurns) == nil {
		msg += fmt.Sprintf(" · at most %d turns", untilDefaultCap)
	}
	if running {
		return msg + " · the run is under way", nil
	}
	return msg + " · starts with your next message", nil
}

func (h *Harness) untilStatus() string {
	s := h.Until()
	if len(s.Conds) == 0 {
		return "until: off (one turn per message) · " + strings.TrimPrefix(untilUsage, "usage: ")
	}
	lines := []string{"until: " + untilWords(s.Conds)}
	for i, c := range s.Conds {
		p := s.Progress[i]
		if p == "" {
			p = "not checked yet"
			if c.Kind == UntilTurns {
				p = "no run yet"
			}
		}
		lines = append(lines, "  "+c.Kind+": "+p)
	}
	switch {
	case s.Running:
		lines = append(lines, fmt.Sprintf("  run: turn %d of at most %d", s.Turn, s.Cap))
	case s.Stopped != "":
		lines = append(lines, "  last run stopped: "+s.Stopped+" · the next message starts a new run")
	default:
		lines = append(lines, "  starts with your next message")
	}
	return strings.Join(lines, "\n")
}

// ParseUntil reads /until's arguments: off, or any of turns <N>, goal
// <text> [--check <cmd>], plan, each at most once. A goal or check is a
// quoted string, or the words up to the next keyword.
func ParseUntil(line string) ([]UntilCond, bool, error) {
	toks, err := untilTokens(line)
	if err != nil {
		return nil, false, err
	}
	if len(toks) == 1 && !toks[0].quoted && strings.EqualFold(toks[0].text, "off") {
		return nil, true, nil
	}
	if len(toks) == 0 {
		return nil, false, errors.New(untilUsage)
	}
	keyword := func(t untilTok) bool {
		if t.quoted {
			return false
		}
		switch strings.ToLower(t.text) {
		case UntilTurns, UntilGoal, UntilPlan, "--check":
			return true
		}
		return false
	}
	// text is a goal's or a check's: one quoted token, or words up to a keyword.
	text := func(i int) (string, int) {
		if i < len(toks) && toks[i].quoted {
			return toks[i].text, i + 1
		}
		var w []string
		for ; i < len(toks) && !keyword(toks[i]); i++ {
			w = append(w, toks[i].text)
		}
		return strings.Join(w, " "), i
	}
	var conds []UntilCond
	seen := map[string]bool{}
	for i := 0; i < len(toks); {
		t := toks[i]
		kind := strings.ToLower(t.text)
		if t.quoted || !keyword(t) {
			if strings.EqualFold(t.text, "off") {
				return nil, false, errors.New("off clears every condition: give it alone")
			}
			return nil, false, fmt.Errorf("%q: %s", t.text, untilUsage)
		}
		if kind == "--check" {
			return nil, false, errors.New("--check goes after a goal: goal <text> --check \"<cmd>\"")
		}
		if seen[kind] {
			return nil, false, fmt.Errorf("%s is given twice", kind)
		}
		seen[kind] = true
		i++
		switch kind {
		case UntilTurns:
			if i >= len(toks) {
				return nil, false, errors.New("turns needs a number: turns 10")
			}
			n, err := strconv.Atoi(toks[i].text)
			if err != nil || n < 1 || n > untilMaxTurns {
				return nil, false, fmt.Errorf("turns %q: want a number from 1 to %d", toks[i].text, untilMaxTurns)
			}
			conds = append(conds, UntilCond{Kind: UntilTurns, N: n})
			i++
		case UntilPlan:
			conds = append(conds, UntilCond{Kind: UntilPlan})
		case UntilGoal:
			var g string
			g, i = text(i)
			if strings.TrimSpace(g) == "" {
				return nil, false, errors.New(`goal needs its text: goal "the tests pass"`)
			}
			c := UntilCond{Kind: UntilGoal, Goal: strings.TrimSpace(g)}
			if i < len(toks) && !toks[i].quoted && strings.EqualFold(toks[i].text, "--check") {
				c.Check, i = text(i + 1)
				if c.Check = strings.TrimSpace(c.Check); c.Check == "" {
					return nil, false, errors.New(`--check needs a command: --check "make test"`)
				}
			}
			conds = append(conds, c)
		}
	}
	return conds, false, nil
}

type untilTok struct {
	text   string
	quoted bool
}

// untilTokens splits a line into words; "…" and '…' are one word each (\"
// and \\ inside double quotes).
func untilTokens(s string) ([]untilTok, error) {
	var out []untilTok
	rs := []rune(s)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			i++
		case r == '"' || r == '\'':
			var b strings.Builder
			i++
			closed := false
			for i < len(rs) {
				c := rs[i]
				if c == r {
					closed = true
					i++
					break
				}
				if r == '"' && c == '\\' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\') {
					b.WriteRune(rs[i+1])
					i += 2
					continue
				}
				b.WriteRune(c)
				i++
			}
			if !closed {
				return nil, errors.New("a quote is not closed")
			}
			out = append(out, untilTok{text: b.String(), quoted: true})
		default:
			j := i
			for j < len(rs) && !strings.ContainsRune(" \t\n\r", rs[j]) {
				j++
			}
			out = append(out, untilTok{text: string(rs[i:j])})
			i = j
		}
	}
	return out, nil
}
