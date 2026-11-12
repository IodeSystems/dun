package dun

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// nudgeRunner is a scripted model that records each request's tool_choice
// and messages. reply decides each round from its index and whether a plan
// call was forced; refuse makes a forced request fail as a 400 would.
type nudgeRunner struct {
	mu     sync.Mutex
	choice []string
	sent   [][]llm.Message
	reply  func(i int, forced bool) *llm.ToolCall // nil: answer with text
	refuse bool
	caps   *llm.Capabilities
}

func (r *nudgeRunner) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	choice := ""
	if opts != nil {
		choice = opts.ToolChoice
	}
	forced := strings.Contains(choice, `"plan_`)
	r.mu.Lock()
	if forced && r.refuse {
		r.choice = append(r.choice, choice)
		r.sent = append(r.sent, msgs)
		r.mu.Unlock()
		return nil, errors.New("llm: 400 tool_choice is not supported")
	}
	i := len(r.choice)
	r.choice = append(r.choice, choice)
	r.sent = append(r.sent, msgs)
	r.mu.Unlock()
	var tc *llm.ToolCall
	if r.reply != nil {
		tc = r.reply(i, forced)
	}
	out := make(chan llm.StreamChunk, 2)
	if tc != nil {
		out <- llm.StreamChunk{ToolCall: tc}
	} else {
		out <- llm.StreamChunk{Content: "done"}
	}
	out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
	close(out)
	return out, nil
}

func (r *nudgeRunner) Capabilities() llm.Capabilities {
	if r.caps != nil {
		return *r.caps
	}
	return llm.Capabilities{}
}

// forcedAt is the indexes of the requests that forced a plan call.
func (r *nudgeRunner) forcedAt() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var at []int
	for i, c := range r.choice {
		if strings.Contains(c, `"plan_`) {
			at = append(at, i)
		}
	}
	return at
}

// sentHas reports whether request i carried text anywhere in its messages.
func (r *nudgeRunner) sentHas(i int, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.sent) {
		return false
	}
	for _, m := range r.sent[i] {
		if strings.Contains(m.Content, text) {
			return true
		}
	}
	return false
}

func (r *nudgeRunner) anySentHas(text string) bool {
	for i := range r.calls() {
		if r.sentHas(i, text) {
			return true
		}
	}
	return false
}

func (r *nudgeRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.choice)
}

// planNudgeCall is nudgeCall for "name {args}", the form stall scripts use.
func planNudgeCall(id, nameArgs string) *llm.ToolCall {
	name, args, _ := strings.Cut(nameArgs, " ")
	return nudgeCall(id, name, args)
}

func nudgeCall(id, name, args string) *llm.ToolCall {
	tc := &llm.ToolCall{ID: id, Type: "function"}
	tc.Function.Name, tc.Function.Arguments = name, args
	return tc
}

// execs runs `cmd` for rounds before upto, answering a forced round with a
// plan add when plan is true, and ends with text.
func execs(cmd string, upto int, plan bool) func(int, bool) *llm.ToolCall {
	return func(i int, forced bool) *llm.ToolCall {
		id := fmt.Sprintf("c%d", i)
		if forced && plan {
			return nudgeCall(id, "plan_add", `{"title":"finish","done_when":"it works"}`)
		}
		if i >= upto {
			return nil
		}
		return nudgeCall(id, "exec", fmt.Sprintf(`{"command":%q}`, cmd))
	}
}

// startNudge starts a session with the stalled-plan nudge off, so these tests
// see the no-plan nudge alone.
func startNudge(t *testing.T, r *nudgeRunner, rounds, fails int) *Harness {
	t.Helper()
	return startStall(t, r, rounds, fails, -1)
}

func startStall(t *testing.T, r *nudgeRunner, rounds, fails, stall int) *Harness {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("DUN_PLAN_NUDGE_ROUNDS", "")
	t.Setenv("DUN_PLAN_NUDGE_FAILS", "")
	t.Setenv("DUN_PLAN_STALL_ROUNDS", "")
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []Server{}, Client: r, Exec: &HostShell{Dir: dir},
		SessionFile:     filepath.Join(dir, "s.jsonl"),
		PlanNudgeRounds: rounds, PlanNudgeFails: fails, PlanStallRounds: stall,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func askOK(t *testing.T, h *Harness) {
	t.Helper()
	if _, err := h.Ask(context.Background(), "do the task"); err != nil {
		t.Fatal(err)
	}
}

// After N rounds with no plan, the next request forces a plan call and
// carries the aside; it happens once.
func TestPlanNudge_AfterRounds(t *testing.T) {
	r := &nudgeRunner{reply: execs("true", 8, true)}
	h := startNudge(t, r, 3, -1)
	askOK(t, h)
	at := r.forcedAt()
	if len(at) != 1 || at[0] != 3 {
		t.Fatalf("forced at %v of %d requests, want [3]", at, r.calls())
	}
	if c := r.choice[3]; !strings.Contains(c, `"plan_add"`) {
		t.Errorf("no plan: forced %s, want plan_add", c)
	}
	if !r.sentHas(3, "This task has run 3 rounds with no plan. Write the plan now") {
		t.Error("the forced request does not carry the aside")
	}
	if r.sentHas(2, "rounds with no plan") {
		t.Error("the aside came before the forced request")
	}
}

// A model that ignores the forced call is not forced again in the same turn;
// the next turn may be.
func TestPlanNudge_OncePerTurn(t *testing.T) {
	r := &nudgeRunner{reply: execs("true", 10, false)}
	h := startNudge(t, r, 3, 2)
	askOK(t, h)
	if at := r.forcedAt(); len(at) != 1 {
		t.Fatalf("forced at %v, want once", at)
	}
	n := r.calls()
	r.mu.Lock()
	r.reply = func(i int, forced bool) *llm.ToolCall {
		if i-n >= 5 {
			return nil
		}
		return nudgeCall(fmt.Sprintf("d%d", i), "exec", `{"command":"true"}`)
	}
	r.mu.Unlock()
	askOK(t, h)
	if at := r.forcedAt(); len(at) != 2 {
		t.Errorf("a new turn was not nudged: forced at %v", at)
	}
}

// A plan made before N: no nudge.
func TestPlanNudge_NotWithAPlan(t *testing.T) {
	r := &nudgeRunner{reply: func(i int, forced bool) *llm.ToolCall {
		switch {
		case i == 0:
			return nudgeCall("p", "plan_add", `{"title":"the work","done_when":"it works"}`)
		case i < 8:
			return nudgeCall(fmt.Sprintf("c%d", i), "exec", `{"command":"false"}`)
		}
		return nil
	}}
	h := startNudge(t, r, 3, 2)
	askOK(t, h)
	if at := r.forcedAt(); len(at) != 0 {
		t.Errorf("nudged with an open plan: forced at %v", at)
	}
}

// K failed execs in a row: a nudge that names the stall. A success between
// them starts the count again.
func TestPlanNudge_AfterFails(t *testing.T) {
	r := &nudgeRunner{reply: func(i int, forced bool) *llm.ToolCall {
		id := fmt.Sprintf("c%d", i)
		switch {
		case forced:
			return nudgeCall(id, "plan_add", `{"title":"fix it","done_when":"it runs"}`)
		case i == 0 || i == 2:
			return nudgeCall(id, "exec", `{"command":"exit 3"}`)
		case i == 1:
			return nudgeCall(id, "exec", `{"command":"true"}`)
		case i < 6:
			return nudgeCall(id, "file_read", `{"path":"/no/such/file"}`)
		}
		return nil
	}}
	h := startNudge(t, r, -1, 3)
	askOK(t, h)
	// Fails: 0 (reset by 1), 2, 3, 4 → the third in a row is result 4.
	at := r.forcedAt()
	if len(at) != 1 || at[0] != 5 {
		t.Fatalf("forced at %v of %d requests, want [5]", at, r.calls())
	}
	if !r.sentHas(5, "This task has had 3 failed runs in a row and no plan. Write the plan now") {
		t.Error("the forced request does not name the stall")
	}
}

// 0 turns each signal off: the flag (as -1) and the environment.
func TestPlanNudge_Off(t *testing.T) {
	r := &nudgeRunner{reply: execs("false", 8, true)}
	h := startNudge(t, r, -1, -1)
	askOK(t, h)
	if at := r.forcedAt(); len(at) != 0 || r.anySentHas("Write the plan now") {
		t.Errorf("nudged while off: forced at %v", at)
	}

	t.Setenv("DUN_PLAN_NUDGE_ROUNDS", "0")
	t.Setenv("DUN_PLAN_NUDGE_FAILS", "0")
	h2 := &Harness{}
	if h2.planNudgeRounds() != 0 || h2.planNudgeFails() != 0 {
		t.Error("env 0 is not off")
	}
	t.Setenv("DUN_PLAN_NUDGE_ROUNDS", "")
	t.Setenv("DUN_PLAN_NUDGE_FAILS", "7")
	if h2.planNudgeRounds() != 25 || h2.planNudgeFails() != 7 {
		t.Errorf("defaults/env: %d %d", h2.planNudgeRounds(), h2.planNudgeFails())
	}
	h2.cfg.PlanNudgeRounds, h2.cfg.PlanNudgeFails = 9, -1
	if h2.planNudgeRounds() != 9 || h2.planNudgeFails() != 0 {
		t.Errorf("config: %d %d", h2.planNudgeRounds(), h2.planNudgeFails())
	}
}

// A child is never nudged: it owns a node already.
func TestPlanNudge_NotForAChild(t *testing.T) {
	h := planHarness(t, false)
	names := []string{"exec", "plan_add"}
	h.toolNames.Store(&names)
	h.parent = &Harness{}
	for range 10 {
		h.planNudgeResult(true, churnAttempt{})
	}
	h.turnRounds.n.Store(100)
	h.cfg.PlanNudgeRounds = 3
	h.planNudgeRound()
	if h.nudge.force || h.nudge.done {
		t.Error("a child was nudged")
	}
	for _, q := range queuedTexts(h) {
		if strings.Contains(q, "Write the plan now") {
			t.Errorf("a child got the aside: %q", q)
		}
	}
	// The same harness as a root is nudged: the test is not vacuous.
	h.parent = nil
	h.planNudgeRound()
	if !h.nudge.force {
		t.Error("a root was not nudged")
	}
}

// No plan tool in the tool set: no nudge.
func TestPlanNudge_NeedsThePlanTool(t *testing.T) {
	h := planHarness(t, false)
	names := []string{"exec"}
	h.toolNames.Store(&names)
	h.turnRounds.n.Store(100)
	h.cfg.PlanNudgeRounds = 3
	h.planNudgeRound()
	if h.nudge.force {
		t.Error("nudged with no plan tool")
	}
}

// An endpoint that refuses the forced call: the request is sent again
// without it, the aside still rides, and the turn does not fail.
func TestPlanNudge_RefusedFallsBackToTheAside(t *testing.T) {
	r := &nudgeRunner{reply: execs("true", 6, true), refuse: true}
	h := startNudge(t, r, 3, -1)
	askOK(t, h)
	at := r.forcedAt()
	if len(at) != 1 || at[0] != 3 {
		t.Fatalf("forced at %v, want [3]", at)
	}
	r.mu.Lock()
	resent := len(r.choice) >= 5 && !strings.Contains(r.choice[4], "plan")
	r.mu.Unlock()
	if !resent {
		t.Fatalf("the refused request was not sent again unforced: %d calls %q", r.calls(), r.choice)
	}
	if !r.sentHas(4, "rounds with no plan") {
		t.Error("the resent request lost the aside")
	}
}

// An endpoint known not to honor a forced tool_choice gets the aside alone.
func TestPlanNudge_NotHonoredSendsTheAsideAlone(t *testing.T) {
	r := &nudgeRunner{reply: execs("true", 6, true), caps: &llm.Capabilities{
		Facts: map[string]llm.Fact{llm.CapToolsChoiceForced: {Value: "false"}},
	}}
	h := startNudge(t, r, 3, -1)
	askOK(t, h)
	if at := r.forcedAt(); len(at) != 0 {
		t.Errorf("forced at %v on an endpoint that does not honor it", at)
	}
	if !r.sentHas(3, "rounds with no plan") {
		t.Error("the aside is missing")
	}
}
