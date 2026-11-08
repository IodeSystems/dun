package dun

import (
	"context"
	"strings"
	"testing"
)

// --non-interactive: a turn that did work is checked once before it ends; a
// one-round answer is not. Interactive: nothing extra unless the plan is open.
func TestOnIdle(t *testing.T) {
	ctx := context.Background()
	h := planHarness(t, false)

	if got := h.onIdle(ctx, 5, 0); got != "" {
		t.Errorf("interactive, no plan: %q, want nothing", got)
	}
	h.cfg.NonInteractive = true
	if got := h.onIdle(ctx, 1, 0); got != "" {
		t.Errorf("one-round answer was checked: %q", got)
	}
	if got := h.onIdle(ctx, 5, 0); !strings.Contains(got, "list each requirement") || !strings.Contains(got, "do not end the turn: keep working") {
		t.Errorf("non-interactive work turn not checked, or the check reads as an order to finish: %q", got)
	}
	if got := h.onIdle(ctx, 6, 1); got != "" {
		t.Errorf("asked twice in one turn: %q", got)
	}

	h.cfg.NonInteractive = false
	planCall(t, h, `{"action":"add","title":"write the parser","done_when":"tests pass"}`)
	if got := h.onIdle(ctx, 3, 0); !strings.Contains(got, "Open plan nodes") || !strings.Contains(got, "write the parser") {
		t.Errorf("interactive with an open plan: %q", got)
	}
}

// The check lists each open node with its done_when, beside the requirement
// check; a done or dropped node is not listed. A child is not told.
func TestOnIdle_ListsOpenNodesDoneWhen(t *testing.T) {
	ctx := context.Background()
	h := planHarness(t, false)
	h.cfg.NonInteractive = true
	planCall(t, h, `{"action":"add","title":"write the parser","done_when":"go test ./parser passes"}`)
	planCall(t, h, `{"action":"add","title":"old idea","done_when":"never"}`)
	planCall(t, h, `{"action":"add","title":"read the spec","done_when":"spec read"}`)
	planCall(t, h, `{"action":"drop","id":"2","reason":"not needed"}`)
	planCall(t, h, `{"action":"update","id":"3","status":"doing"}`)
	if out := planCall(t, h, `{"action":"done","id":"3","account":"read it"}`); !strings.Contains(out, "done") {
		t.Fatalf("done: %s", out)
	}
	got := h.onIdle(ctx, 5, 0)
	for _, want := range []string{
		"list each requirement", // the requirement check stays
		"Open plan nodes:",
		"- 1 write the parser; done_when: go test ./parser passes",
		"Finish each, or mark it done with how done_when was met, or drop it with why.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, not := range []string{"old idea", "read the spec"} {
		if strings.Contains(got, not) {
			t.Errorf("a finished node is listed (%q):\n%s", not, got)
		}
	}
}
