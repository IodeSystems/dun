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
	if got := h.onIdle(ctx, 3, 0); !strings.Contains(got, "open items") || !strings.Contains(got, "write the parser") {
		t.Errorf("interactive with an open plan: %q", got)
	}
}
