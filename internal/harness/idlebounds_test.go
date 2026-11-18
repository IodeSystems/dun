package harness

import (
	"context"
	"strings"
	"testing"
)

// The turn-end check: not in a wake that did no tool work; a blocked node
// is not listed; not again while the plan is unchanged since the last check;
// the count is the turn's, so a retry (agentkit's count at 0 again) gets no
// new check.
func TestOnIdle_Bounds(t *testing.T) {
	ctx := context.Background()
	h := planHarness(t, false)
	planCall(t, h, "plan_add", `{"title":"write the parser","done_when":"tests pass"}`)
	planCall(t, h, "plan_add", `{"title":"pick a region","done_when":"picked"}`)
	planCall(t, h, "plan_close", `{"outcome":"block","id":"2","reason":"which region?"}`)

	h.turnBegin(originAuto)
	if got := h.onIdle(ctx, 1, 0); got != "" {
		t.Errorf("a wake with no tool work was checked: %q", got)
	}
	h.turnBegin(originPerson)
	got := h.onIdle(ctx, 3, 0)
	if !strings.Contains(got, "write the parser") || strings.Contains(got, "pick a region") {
		t.Errorf("first check (the blocked node left out): %q", got)
	}
	if got := h.onIdle(ctx, 4, 0); got != "" {
		t.Errorf("a retried attempt got a new check: %q", got)
	}
	h.turnBegin(originPerson)
	if got := h.onIdle(ctx, 3, 0); got != "" {
		t.Errorf("the plan unchanged since the last check, checked again: %q", got)
	}
	h.turnBegin(originPerson)
	planCall(t, h, "plan_add", `{"title":"write the docs","done_when":"written"}`)
	if got := h.onIdle(ctx, 3, 0); !strings.Contains(got, "write the docs") {
		t.Errorf("the plan changed, not checked: %q", got)
	}
}
