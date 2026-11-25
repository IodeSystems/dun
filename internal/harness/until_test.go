package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/proto"
)

// untilRunner is a model for stop-condition runs. A turn's first request
// (the task, or the [until] note last) gets act(k)'s call, k counting
// turns from 1; a request after a tool result gets text. The goal judge's
// requests get judge(n)'s answer.
type untilRunner struct {
	mu     sync.Mutex
	turns  int
	judged int
	act    func(k int) *llm.ToolCall
	judge  func(n int) string
}

func (r *untilRunner) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	out := make(chan llm.StreamChunk, 2)
	defer close(out)
	last := msgs[len(msgs)-1]
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case strings.HasPrefix(msgs[0].Content, "You check whether a coding agent"):
		r.judged++
		ans := `{"met": false, "reason": "not yet"}`
		if r.judge != nil {
			ans = r.judge(r.judged)
		}
		out <- llm.StreamChunk{Content: ans}
	case last.Role != "tool" && (strings.Contains(last.Content, "[until] The user set") || strings.HasPrefix(strings.TrimSpace(last.Content), "go now")):
		r.turns++
		var tc *llm.ToolCall
		if r.act != nil {
			tc = r.act(r.turns)
		}
		if tc != nil {
			out <- llm.StreamChunk{ToolCall: tc}
		} else {
			out <- llm.StreamChunk{Content: "nothing to do"}
		}
	default:
		out <- llm.StreamChunk{Content: "ok"}
	}
	out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
	return out, nil
}

func (r *untilRunner) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (r *untilRunner) turnCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turns
}

func untilExec(k int, cmd string) *llm.ToolCall {
	return nudgeCall("x"+string(rune('a'+k)), "exec", `{"command":"`+cmd+`"}`)
}

func startUntil(t *testing.T, r *untilRunner, dir string, mut func(*Config)) *Harness {
	t.Helper()
	cfg := Config{Workspace: dir, Servers: []mcpserver.Server{}, Client: r, Exec: &HostShell{Dir: dir},
		SessionFile: filepath.Join(dir, "s.jsonl"), PlanNudgeRounds: -1, PlanNudgeFails: -1, PlanStallRounds: -1, SettleDelay: -1}
	if mut != nil {
		mut(&cfg)
	}
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// drive is the engine loop's part (cmd/dun until.go): the person's message,
// then each next turn the run asks for. It returns why the run stopped.
func drive(t *testing.T, h *Harness) string {
	t.Helper()
	ctx := context.Background()
	if _, err := h.Ask(ctx, "go now"); err != nil {
		t.Fatal(err)
	}
	for range 60 {
		st := h.UntilNext(ctx)
		if st.Stopped != "" {
			return st.Stopped
		}
		if !st.Continue {
			return ""
		}
		h.UntilContinue(ctx)
		_, _ = h.Continue(ctx)
	}
	t.Fatal("the run did not stop")
	return ""
}

func untilSet(t *testing.T, h *Harness, line string) {
	t.Helper()
	if _, err := h.UntilCmd(line); err != nil {
		t.Fatal(err)
	}
}

func TestUntil_TurnCap(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	r := &untilRunner{act: func(k int) *llm.ToolCall { return untilExec(k, "true") }}
	h := startUntil(t, r, t.TempDir(), nil)
	defer h.Close()
	untilSet(t, h, "turns 3")
	if got := drive(t, h); got != "turn cap 3 reached (turn 3 of 3)" {
		t.Fatalf("stopped %q", got)
	}
	if r.turnCount() != 3 {
		t.Errorf("turns run %d, want 3", r.turnCount())
	}
	s := h.Until()
	if s.Running || len(s.Conds) != 1 || !strings.Contains(s.Stopped, "turn cap") {
		t.Errorf("after the stop: %+v", s)
	}
	// The conditions stay: the next message starts a new run.
	if got := drive(t, h); !strings.HasPrefix(got, "turn cap 3") || r.turnCount() != 6 {
		t.Errorf("second run %q after %d turns", got, r.turnCount())
	}
}

func TestUntil_NoProgress(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	r := &untilRunner{} // no tool calls
	h := startUntil(t, r, t.TempDir(), nil)
	defer h.Close()
	untilSet(t, h, "turns 10")
	if got := drive(t, h); got != "no progress in 2 turns (turn 2 of 10)" {
		t.Fatalf("stopped %q", got)
	}
}

func TestUntil_PlanDrained(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	r := &untilRunner{}
	h := startUntil(t, r, t.TempDir(), nil)
	defer h.Close()
	planCall(t, h, "plan_add", `{"title":"one","done_when":"done"}`)
	planCall(t, h, "plan_add", `{"title":"two","done_when":"done"}`)
	planCall(t, h, "plan_update", `{"id":"1","status":"doing"}`)
	planCall(t, h, "plan_update", `{"id":"2","status":"doing"}`)
	r.act = func(k int) *llm.ToolCall {
		if k > 2 {
			return nil
		}
		id := string(rune('0' + k))
		return nudgeCall("p"+id, "plan_close", `{"outcome":"done","id":"`+id+`","account":"done it"}`)
	}
	untilSet(t, h, "plan")
	if got := drive(t, h); got != "plan drained (turn 2 of 20)" {
		t.Fatalf("stopped %q", got)
	}
}

// A plan with no ready node and no progress: the plan stops it, not the cap.
func TestUntil_PlanEmpty(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h := startUntil(t, &untilRunner{}, t.TempDir(), nil)
	defer h.Close()
	untilSet(t, h, "plan turns 5")
	if got := drive(t, h); got != "plan empty (turn 1 of 5)" {
		t.Fatalf("stopped %q", got)
	}
}

func TestUntil_GoalJudge(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	r := &untilRunner{act: func(k int) *llm.ToolCall { return untilExec(k, "true") },
		judge: func(n int) string {
			if n < 2 {
				return `{"met": false, "reason": "the tests were not run"}`
			}
			return "sure: {\"met\": true, \"reason\": \"the tests pass\"}"
		}}
	h := startUntil(t, r, t.TempDir(), nil)
	defer h.Close()
	untilSet(t, h, `goal "the tests pass"`)
	if got := drive(t, h); got != "goal met: the tests pass (turn 2 of 20)" {
		t.Fatalf("stopped %q", got)
	}
	if r.judged != 2 {
		t.Errorf("judged %d times", r.judged)
	}
}

// --check alone decides: no judge call.
func TestUntil_GoalCheck(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	r := &untilRunner{act: func(k int) *llm.ToolCall {
		if k == 2 {
			return untilExec(k, "touch done.txt")
		}
		return untilExec(k, "true")
	}}
	h := startUntil(t, r, dir, nil)
	defer h.Close()
	untilSet(t, h, `goal "a done file" --check "test -f done.txt" turns 6`)
	if got := drive(t, h); got != "goal met: the check passed (turn 2 of 6)" {
		t.Fatalf("stopped %q", got)
	}
	if r.judged != 0 {
		t.Errorf("the judge was asked %d times", r.judged)
	}
	if ok, why := h.untilCheck(context.Background(), "echo nope; exit 3"); ok || !strings.Contains(why, "exited 3") || !strings.Contains(why, "nope") {
		t.Errorf("failing check: %v %q", ok, why)
	}
}

// ask_user in a run's turn stops it; the continue turns are user-set, so
// it is not refused there.
func TestUntil_AskStops(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	asked := 0
	r := &untilRunner{act: func(k int) *llm.ToolCall {
		if k == 2 {
			return nudgeCall("q", "ask_user", `{"question":"which?","options":["a","b"],"recommended":"a"}`)
		}
		return untilExec(k, "true")
	}}
	h := startUntil(t, r, t.TempDir(), func(c *Config) {
		c.Ask = func(context.Context, string, []string, bool, bool, []string) (string, error) {
			asked++
			return "a", nil
		}
	})
	defer h.Close()
	untilSet(t, h, "turns 5")
	if got := drive(t, h); got != "a question was put to the user (turn 2 of 5)" {
		t.Fatalf("stopped %q", got)
	}
	if asked != 1 {
		t.Errorf("asked %d", asked)
	}
}

// The state machine's stops for a turn that was cut: by the person (stop),
// by a restart (the run is owed its next turn), by an error (stop); and an
// interrupt between turns.
func TestUntil_TurnEnds(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h := startUntil(t, &untilRunner{}, t.TempDir(), nil)
	defer h.Close()
	ctx := context.Background()
	untilSet(t, h, "turns 5")
	end := func(err error, reason string) UntilStep {
		h.untilTurnBegin()
		h.intMu.Lock()
		h.intReason = reason
		h.intMu.Unlock()
		h.untilTurnEnd(agent.TurnResult{}, err)
		return h.UntilNext(ctx)
	}
	if st := end(ErrInterrupted, ""); st.Stopped != "interrupted (turn 1 of 5)" {
		t.Errorf("person's interrupt: %+v", st)
	}
	if st := end(ErrInterrupted, "dund is restarting"); st.Continue || st.Stopped != "" || !h.Until().Running {
		t.Errorf("restart cut: %+v %+v", st, h.Until())
	}
	if st := h.UntilNext(ctx); !st.Continue {
		t.Errorf("owed after the cut: %+v", st)
	}
	if st := end(errors.New("provider down"), ""); st.Stopped != "the turn failed: provider down (turn 2 of 5)" {
		t.Errorf("error: %+v", st)
	}
	h.untilTurnBegin()
	h.untilTurnEnd(agent.TurnResult{}, nil)
	if why := h.UntilInterrupt(); why != "interrupted (after turn 1 of 5)" || h.Until().Running {
		t.Errorf("between turns: %q %+v", why, h.Until())
	}
	if st := h.UntilNext(ctx); st.Continue || st.Stopped != "" {
		t.Errorf("after the interrupt: %+v", st)
	}
	// off mid-turn: the turn ends as usual, nothing after it.
	h.untilTurnBegin()
	if msg, _ := h.UntilCmd("off"); !strings.Contains(msg, "the turn running ends") {
		t.Errorf("off: %q", msg)
	}
	h.untilTurnEnd(agent.TurnResult{}, nil)
	if st := h.UntilNext(ctx); st.Continue || st.Stopped != "" {
		t.Errorf("after off: %+v", st)
	}
	// Set while a turn runs: that turn is the run's first.
	h.untilTurnBegin()
	untilSet(t, h, "turns 2")
	if s := h.Until(); !s.Running || s.Turn != 1 {
		t.Errorf("set mid-turn: %+v", s)
	}
}

// A run under way survives the engine: the sidecar holds it, and the new
// engine owes it its next turn.
func TestUntil_Persists(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	r := &untilRunner{act: func(k int) *llm.ToolCall { return untilExec(k, "true") }}
	h := startUntil(t, r, dir, nil)
	untilSet(t, h, `goal "say \"hi\"" turns 4`)
	if _, err := h.Ask(context.Background(), "go now"); err != nil {
		t.Fatal(err)
	}
	if st := h.UntilNext(context.Background()); !st.Continue {
		t.Fatalf("step %+v", st)
	}
	h.Close()
	if _, err := os.Stat(filepath.Join(dir, "s.until.json")); err != nil {
		t.Fatal(err)
	}
	h2 := startUntil(t, r, dir, nil)
	defer h2.Close()
	s := h2.Until()
	if !s.Running || s.Turn != 1 || len(s.Conds) != 2 || s.Conds[0].Goal != `say "hi"` || s.Cap != 4 {
		t.Fatalf("resumed %+v", s)
	}
	if st := h2.UntilNext(context.Background()); !st.Continue {
		t.Errorf("not owed its next turn: %+v", st)
	}
	if _, err := h2.UntilCmd("off"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "s.until.json")); !os.IsNotExist(err) {
		t.Errorf("off kept the sidecar: %v", err)
	}
}

// The continue note says the turn, the goal's verdict and the plan.
func TestUntil_Note(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h := startUntil(t, &untilRunner{}, t.TempDir(), nil)
	defer h.Close()
	planCall(t, h, "plan_add", `{"title":"one","done_when":"done"}`)
	untilSet(t, h, `plan goal "ship it"`)
	h.untilTurnBegin()
	h.until.goalNote = "not shipped"
	n := h.untilNote(context.Background())
	for _, want := range []string{"[until]", "turn 2 of at most 20", "Goal: ship it.", "Not met yet: not shipped.", "1 node(s) open, 1 ready"} {
		if !strings.Contains(n, want) {
			t.Errorf("note lacks %q: %s", want, n)
		}
	}
	if l := h.Until().Line; l != "until: turn 1/20 · plan 1 open, 1 ready · goal not met" {
		t.Errorf("line %q", l)
	}
}

func TestParseUntil(t *testing.T) {
	for line, want := range map[string]string{
		"turns 10":      "turns 10",
		"plan turns 15": "plan turns 15",
		`goal "tests pass" --check "make test" turns 10`: `goal "tests pass" --check "make test" turns 10`,
		"goal the tests pass plan":                       `goal "the tests pass" plan`,
		`goal 'it\'s'`:                                   "",
		`goal "say \"hi\" \\ ok"`:                        `goal "say \"hi\" \\ ok"`,
		"PLAN Turns 3":                                   "plan turns 3",
		"goal fix it --check make test":                  `goal "fix it" --check "make test"`,
	} {
		conds, off, err := ParseUntil(line)
		if want == "" {
			if err == nil {
				t.Errorf("%s: want an error, got %+v", line, conds)
			}
			continue
		}
		if err != nil || off {
			t.Errorf("%s: %v %v", line, err, off)
			continue
		}
		got := untilWords(conds)
		if got != want {
			t.Errorf("%s: %q, want %q", line, got, want)
		}
		// proto.UntilLine's line reads back the same.
		again, _, err := ParseUntil(got)
		if err != nil || untilWords(again) != got {
			t.Errorf("%s: round trip %q %v", line, untilWords(again), err)
		}
	}
	if _, off, err := ParseUntil("off"); !off || err != nil {
		t.Errorf("off: %v %v", off, err)
	}
	for _, bad := range []string{"turns", "turns 0", "turns x", "plan plan", "goal", "--check x", "plan off", "soon", `goal "open`, "goal x --check"} {
		if _, _, err := ParseUntil(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	if l := proto.UntilLine(nil); l != "" {
		t.Errorf("no conditions: %q", l)
	}
}

func TestParseUntilVerdict(t *testing.T) {
	for s, want := range map[string]bool{
		`{"met": true, "reason": "x"}`:  true,
		"```json\n{\"met\":false}\n```": false,
		"no":                            false,
		`{"reason": "x"}`:               false,
	} {
		if got, _ := parseUntilVerdict(s); got != want {
			t.Errorf("%q: %v", s, got)
		}
	}
}
