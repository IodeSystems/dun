package dun

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"
)

const planStallAsk = "Update the plan now: mark done what is done"

// stallScript answers request i: first[i] when set, a plan show when the plan
// call is forced, an exec before upto (each a new command, so the loop guard
// stays out of it), and text from upto on. With open nodes the end-of-turn
// check adds one more request after the text (idlecheck.go).
func stallScript(upto int, first map[int]string) func(int, bool) *llm.ToolCall {
	return func(i int, forced bool) *llm.ToolCall {
		id := fmt.Sprintf("c%d", i)
		if a, ok := first[i]; ok {
			return planNudgeCall(id, a)
		}
		if forced {
			return nudgeCall(id, "plan_show", `{}`)
		}
		if i >= upto {
			return nil
		}
		return nudgeCall(id, "exec", fmt.Sprintf(`{"command":"true %d"}`, i))
	}
}

const (
	stallAddArgs = `{"title":"the work","done_when":"it works"}`
	stallAdd     = "plan_add " + stallAddArgs
)

// Open nodes and no move for S rounds: one forced plan call, and the aside
// names the open nodes.
func TestPlanStall_AfterRounds(t *testing.T) {
	r := &nudgeRunner{reply: stallScript(5, map[int]string{0: stallAdd})}
	h := startStall(t, r, -1, -1, 3)
	askOK(t, h)
	// The add is round 1; rounds 2, 3 and 4 do not move it. The next would be
	// after round 7, and request 6 is the last.
	at := r.forcedAt()
	if !slices.Equal(at, []int{4}) {
		t.Fatalf("forced at %v of %d requests, want [4]", at, r.calls())
	}
	if c := r.choice[4]; !strings.Contains(c, `"plan_show"`) {
		t.Errorf("stall: forced %s, want plan_show", c)
	}
	if !r.sentHas(4, "[plan] No plan node has moved in 3 rounds; 1 open: 1 the work. "+planStallAsk) {
		t.Error("the forced request does not carry the aside")
	}
	if r.sentHas(3, planStallAsk) {
		t.Error("the aside came before the forced request")
	}
}

// A status change before S starts the window again: no nudge.
func TestPlanStall_MoveResets(t *testing.T) {
	// With no move, request 4 would be forced; with it, request 6 (there is
	// none: 4 is the text, 5 the end-of-turn check).
	r := &nudgeRunner{reply: stallScript(4, map[int]string{
		0: stallAdd,
		2: `plan_update {"id":"1","status":"doing"}`,
	})}
	h := startStall(t, r, -1, -1, 3)
	askOK(t, h)
	if at := r.forcedAt(); len(at) != 0 || r.anySentHas(planStallAsk) {
		t.Errorf("nudged after a move: forced at %v", at)
	}
}

// show and ready are not moves.
func TestPlanStall_ShowAndReadyDoNotMove(t *testing.T) {
	r := &nudgeRunner{reply: stallScript(5, map[int]string{
		0: stallAdd,
		1: `plan_show {}`,
		2: `plan_show {}`,
		3: `plan_show {"id":"1"}`,
	})}
	h := startStall(t, r, -1, -1, 3)
	askOK(t, h)
	if at := r.forcedAt(); !slices.Equal(at, []int{4}) {
		t.Fatalf("forced at %v, want [4]", at)
	}
}

// After a nudge the window starts again, and a turn gets at most 3; the next
// turn may get more.
func TestPlanStall_RepeatsAtMostThree(t *testing.T) {
	r := &nudgeRunner{reply: stallScript(30, map[int]string{0: stallAdd})}
	h := startStall(t, r, -1, -1, 3)
	askOK(t, h)
	if at := r.forcedAt(); !slices.Equal(at, []int{4, 7, 10}) {
		t.Fatalf("forced at %v, want [4 7 10]", at)
	}
	n := r.calls()
	r.mu.Lock()
	r.reply = func(i int, forced bool) *llm.ToolCall {
		if i-n >= 5 {
			return nil
		}
		return nudgeCall(fmt.Sprintf("d%d", i), "exec", fmt.Sprintf(`{"command":"true %d"}`, i))
	}
	r.mu.Unlock()
	askOK(t, h)
	// Turn 2: 5 execs, the text, the end-of-turn check.
	if at := r.forcedAt(); !slices.Equal(at[3:], []int{n + 3, n + 6}) {
		t.Errorf("a new turn's count did not start again: forced at %v (turn began at %d)", at, n)
	}
}

// 0 is off: the flag (as -1) and the environment.
func TestPlanStall_Off(t *testing.T) {
	r := &nudgeRunner{reply: stallScript(20, map[int]string{0: stallAdd})}
	h := startStall(t, r, -1, -1, -1)
	askOK(t, h)
	if at := r.forcedAt(); len(at) != 0 || r.anySentHas(planStallAsk) {
		t.Errorf("nudged while off: forced at %v", at)
	}

	h2 := &Harness{}
	t.Setenv("DUN_PLAN_STALL_ROUNDS", "0")
	if h2.planStallRounds() != 0 {
		t.Error("env 0 is not off")
	}
	t.Setenv("DUN_PLAN_STALL_ROUNDS", "")
	if h2.planStallRounds() != 30 {
		t.Errorf("default: %d", h2.planStallRounds())
	}
	t.Setenv("DUN_PLAN_STALL_ROUNDS", "7")
	if h2.planStallRounds() != 7 {
		t.Errorf("env: %d", h2.planStallRounds())
	}
	h2.cfg.PlanStallRounds = 9
	if h2.planStallRounds() != 9 {
		t.Errorf("config: %d", h2.planStallRounds())
	}
}

// A plan whose nodes are all done is not stalled.
func TestPlanStall_NotWhenAllDone(t *testing.T) {
	r := &nudgeRunner{reply: stallScript(12, map[int]string{
		0: stallAdd,
		1: `plan_update {"id":"1","status":"doing"}`,
		2: `plan_close {"outcome":"done","id":"1","account":"it works"}`,
	})}
	h := startStall(t, r, -1, -1, 3)
	askOK(t, h)
	if at := r.forcedAt(); len(at) != 0 || r.anySentHas(planStallAsk) {
		t.Errorf("nudged with no open node: forced at %v", at)
	}
}

// A child never gets one; the same harness as a root does. The aside names
// at most 5 open nodes and counts them all.
func TestPlanStall_NotForAChild(t *testing.T) {
	h := planHarness(t, false)
	names := []string{"exec", "plan_add"}
	h.toolNames.Store(&names)
	for i := range 7 {
		planCall(t, h, "plan_add", fmt.Sprintf(`{"title":"step %d","done_when":"x"}`, i+1))
	}
	planCall(t, h, "plan_update", `{"id":"2","status":"doing"}`)
	if out := planCall(t, h, "plan_close", `{"outcome":"done","id":"2","account":"x"}`); !strings.Contains(out, "done") {
		t.Fatalf("done: %s", out)
	}
	h.parent = &Harness{}
	h.cfg.PlanStallRounds = 3
	h.turnRounds.n.Store(100)
	h.planStallRound()
	if h.nudge.force || h.nudge.stalls != 0 {
		t.Error("a child was nudged")
	}
	h.parent = nil
	h.planStallRound()
	if !h.nudge.force {
		t.Fatal("a root was not nudged")
	}
	want := "[plan] No plan node has moved in 100 rounds; 6 open: 1 step 1; 3 step 3; 4 step 4; 5 step 5; 6 step 6. " +
		"Update the plan now: mark done what is done (with how done_when was met), block what is stuck (with why), " +
		"or drop an approach that is not working and add the next one (replaces). Then go on with the work."
	if !slices.Contains(queuedTexts(h), want) {
		t.Errorf("aside: %q, want %q", queuedTexts(h), want)
	}
}

// No plan tool: no stall nudge.
func TestPlanStall_NeedsThePlanTool(t *testing.T) {
	h := planHarness(t, false)
	planCall(t, h, "plan_add", stallAddArgs)
	names := []string{"exec"}
	h.toolNames.Store(&names)
	h.cfg.PlanStallRounds = 3
	h.turnRounds.n.Store(100)
	h.planStallRound()
	if h.nudge.force {
		t.Error("nudged with no plan tool")
	}
}

// The no-plan nudge comes first; the plan it makes can then stall. The two
// are counted apart.
func TestPlanStall_AfterThePlanNudge(t *testing.T) {
	r := &nudgeRunner{reply: stallScript(8, nil)}
	r.reply = func(next func(int, bool) *llm.ToolCall) func(int, bool) *llm.ToolCall {
		return func(i int, forced bool) *llm.ToolCall {
			if forced && i == 3 {
				return planNudgeCall("p", stallAdd)
			}
			return next(i, forced)
		}
	}(r.reply)
	h := startStall(t, r, 3, -1, 3)
	askOK(t, h)
	// No plan: forced at 3, where the add is round 4; still to round 7.
	if at := r.forcedAt(); !slices.Equal(at, []int{3, 7}) {
		t.Fatalf("forced at %v, want [3 7]", at)
	}
	if !r.sentHas(3, "rounds with no plan") || r.sentHas(3, planStallAsk) {
		t.Error("request 3 does not carry the no-plan aside alone")
	}
	if !r.sentHas(7, planStallAsk) {
		t.Error("request 7 does not carry the stall aside")
	}
}

// What moves the plan.
func TestPlanStall_WhatMoves(t *testing.T) {
	base := &taskgraph.Node{ID: "1", Title: "a", Details: "g", DoneWhen: "d", Status: taskgraph.Todo, Needs: []string{"2"}}
	with := func(f func(n *taskgraph.Node)) *taskgraph.Node {
		n := *base
		n.Needs = slices.Clone(base.Needs)
		f(&n)
		return &n
	}
	for _, c := range []struct {
		name string
		ch   taskgraph.Change
		want bool
	}{
		{"added", taskgraph.Change{After: base}, true},
		{"status", taskgraph.Change{Before: base, After: with(func(n *taskgraph.Node) { n.Status = taskgraph.Doing })}, true},
		{"done_when", taskgraph.Change{Before: base, After: with(func(n *taskgraph.Node) { n.DoneWhen = "e" })}, true},
		{"details", taskgraph.Change{Before: base, After: with(func(n *taskgraph.Node) { n.Details = "h" })}, true},
		{"needs", taskgraph.Change{Before: base, After: with(func(n *taskgraph.Node) { n.Needs = nil })}, true},
		{"title", taskgraph.Change{Before: base, After: with(func(n *taskgraph.Node) { n.Title = "b" })}, false},
		{"account", taskgraph.Change{Before: base, After: with(func(n *taskgraph.Node) { n.Account = "n" })}, false},
		{"removed", taskgraph.Change{Before: base}, false},
	} {
		if got := planMove(c.ch); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
