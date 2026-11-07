package dun

import (
	"strings"
	"testing"
	"time"
)

func kinds(h *Harness) (asides, notes []string) {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	for _, q := range h.queue {
		switch q.kind {
		case queuedAside:
			asides = append(asides, q.text)
		case queuedNotification:
			notes = append(notes, q.text)
		}
	}
	return
}

// A newer aside with the same key replaces the queued one; others stay.
func TestAside_KeyReplaces(t *testing.T) {
	h := newNoteHarness(t)
	h.AsideWith("exec #1 quiet for 1m", AsideOpts{Key: "job:1:quiet"})
	h.Aside("tool set changed")
	h.AsideWith("exec #1 quiet for 5m", AsideOpts{Key: "job:1:quiet"})
	a, _ := kinds(h)
	if len(a) != 2 || a[0] != "exec #1 quiet for 5m" || a[1] != "tool set changed" {
		t.Errorf("queue %q", a)
	}
}

// Undelivered past its escalation, an aside becomes a notification and wakes
// the driver: an agent asleep on a silent job is woken to decide.
func TestAside_EscalatesAndWakes(t *testing.T) {
	h := newNoteHarness(t)
	h.AsideWith("exec #1 quiet for 5m", AsideOpts{Key: "job:1:quiet", EscalateAfter: 20 * time.Millisecond})
	if h.Pending() != 0 {
		t.Fatal("an aside counted as pending before it escalated")
	}
	select {
	case <-h.wake:
	case <-time.After(2 * time.Second):
		t.Fatal("the escalated aside did not wake the driver")
	}
	if _, n := kinds(h); len(n) != 1 || h.Pending() != 1 {
		t.Errorf("not promoted to a notification: %q pending %d", n, h.Pending())
	}
}

// Delivered first (lifted into a tool result mid-turn), it never escalates.
func TestAside_DeliveredNeverEscalates(t *testing.T) {
	h := newNoteHarness(t)
	h.AsideWith("exec #1 quiet for 5m", AsideOpts{EscalateAfter: 20 * time.Millisecond})
	if got := h.liftQueued("result"); !strings.Contains(got, "quiet for 5m") {
		t.Fatalf("not lifted: %q", got)
	}
	select {
	case <-h.wake:
		t.Error("a delivered aside still woke the driver")
	case <-time.After(100 * time.Millisecond):
	}
}

// A replacement keeps the earlier escalation time: a stream of updates cannot
// postpone the wake forever.
func TestAside_ReplacementKeepsEarlierEscalation(t *testing.T) {
	h := newNoteHarness(t)
	h.AsideWith("quiet 1m", AsideOpts{Key: "k", EscalateAfter: 30 * time.Millisecond})
	h.AsideWith("quiet 5m", AsideOpts{Key: "k", EscalateAfter: time.Hour})
	select {
	case <-h.wake:
	case <-time.After(2 * time.Second):
		t.Fatal("the replacement postponed the escalation")
	}
	if _, n := kinds(h); len(n) != 1 || n[0] != "quiet 5m" {
		t.Errorf("want the newer text escalated: %q", n)
	}
}

// Turn-scoped asides go when the next turn starts; others stay.
func TestAside_TurnScoped(t *testing.T) {
	h := newNoteHarness(t)
	h.AsideWith("this turn only", AsideOpts{TurnScoped: true})
	h.Aside("keep")
	h.dropTurnScoped()
	got := h.liftQueued("r")
	if strings.Contains(got, "this turn only") || !strings.Contains(got, "keep") {
		t.Errorf("lifted %q", got)
	}
}
