package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"
)

// asksOnHarness is planHarness with plan tools in the set, so it keeps asks.
func asksOnHarness(t *testing.T) *Harness {
	t.Helper()
	h := planHarness(t, false)
	names := []string{taskgraph.ToolAdd, taskgraph.ToolClose, taskgraph.ToolShow, "ask_user", asksToolName}
	h.toolNames.Store(&names)
	return h
}

// ask_user is refused in a round dun added: after the turn-end check, right
// after a plan nudge, and in an automatic turn with no open ask (a blocked
// one is not open). The question never reaches the person.
func TestAskGuard_RefusedInAutomaticRounds(t *testing.T) {
	h := asksOnHarness(t)
	asked := 0
	d := withAskGuard(withAsk(nil, func(context.Context, string, []string, bool, bool, []string) (string, error) {
		asked++
		return "a", nil
	}, nil), h)
	call := func() string {
		tc := llm.ToolCall{}
		tc.Function.Name = "ask_user"
		tc.Function.Arguments = `{"question":"which?","options":["a","b"],"recommended":"a"}`
		out, _ := d(context.Background(), tc)
		return out
	}
	h.turnBegin(originPerson)
	if out := call(); out != "a" {
		t.Fatalf("the person's turn: %q", out)
	}
	h.idle.mark(planRev{})
	if out := call(); out != askAutoRefused {
		t.Errorf("after the turn-end check: %q", out)
	}
	h.turnBegin(originPerson)
	h.nudge.lastAt = 3
	h.turnRounds.n.Store(4)
	if out := call(); out != askAutoRefused {
		t.Errorf("the round after a nudge: %q", out)
	}
	h.turnRounds.n.Store(5)
	if out := call(); out != "a" {
		t.Errorf("two rounds after a nudge: %q", out)
	}
	h.nudge.reset()
	h.turnBegin(originAuto)
	if out := call(); out != askAutoRefused {
		t.Errorf("an automatic turn, no ask open: %q", out)
	}
	if !h.askOpen("what is the plan?") {
		t.Fatal("no ask")
	}
	if out := call(); out != "a" {
		t.Errorf("an automatic turn with an ask open: %q", out)
	}
	h.asksCall(`{"asks":[{"id":1,"state":"blocked","note":"which region?"}]}`)
	if out := call(); out != askAutoRefused || !strings.Contains(out, `"outcome":"block"`) {
		t.Errorf("an automatic turn, the ask blocked: %q", out)
	}
	h.turnBegin(originUserSet)
	if out := call(); out != "a" {
		t.Errorf("a loop's firing: %q", out)
	}
	if asked != 4 {
		t.Errorf("the person was asked %d times, want 4", asked)
	}
}
