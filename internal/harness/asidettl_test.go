package harness

import (
	"strings"
	"sync/atomic"
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

// --aside-escalate-ms 0 (Config.AsideEscalate negative) and
// DUN_ASIDE_ESCALATE_MS=0 turn escalation off: a heartbeat stays an aside.
func TestAsideEscalate_ZeroIsOff(t *testing.T) {
	var n atomic.Int32
	for _, tc := range []struct {
		name string
		cfg  time.Duration
		env  string
		want time.Duration
	}{
		{"flag 0 (off)", -time.Millisecond, "", 0},
		{"env 0 (off)", 0, "0", 0},
		{"env 250", 0, "250", 250 * time.Millisecond},
		{"unset", 0, "", 5 * time.Second},
		{"flag wins", 40 * time.Millisecond, "0", 40 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DUN_ASIDE_ESCALATE_MS", tc.env)
			h := newNoteHarness(t)
			h.cfg.AsideEscalate = tc.cfg
			if got := h.asideEscalate(); got != tc.want {
				t.Errorf("asideEscalate = %s, want %s", got, tc.want)
			}
			if o := h.heartbeatOpts("job:1:quiet", &n, true); (o.EscalateAfter > 0) != (tc.want > 0) {
				t.Errorf("heartbeat EscalateAfter = %s with escalation %s", o.EscalateAfter, tc.want)
			}
		})
	}
}

// One job's (or child's) heartbeats wake the agent at most three times; the
// fourth stays an aside and wakes nothing.
func TestHeartbeat_EscalationCappedAtThree(t *testing.T) {
	h := newNoteHarness(t)
	h.cfg.AsideEscalate = 5 * time.Millisecond
	var n atomic.Int32
	for i := 1; i <= maxHeartbeatEscalations; i++ {
		h.AsideWith("exec #1 quiet", h.heartbeatOpts("job:1:quiet", &n, true))
		select {
		case <-h.wake:
		case <-time.After(2 * time.Second):
			t.Fatalf("escalation %d did not wake", i)
		}
		waitFor(t, 2*time.Second, func() bool { return n.Load() == int32(i) })
		h.liftQueued("r") // a turn carried it
	}
	o := h.heartbeatOpts("job:1:quiet", &n, true)
	if o.EscalateAfter != 0 {
		t.Fatalf("after %d escalations a heartbeat must stay an aside: %+v", n.Load(), o)
	}
	h.AsideWith("exec #1 quiet", o)
	select {
	case <-h.wake:
		t.Error("a fourth escalation woke the agent")
	case <-time.After(60 * time.Millisecond):
	}
	if a, notes := kinds(h); len(a) != 1 || len(notes) != 0 {
		t.Errorf("want one aside, got asides %q notes %q", a, notes)
	}
}

// A delivered heartbeat did not escalate, so it does not count toward the cap.
func TestHeartbeat_DeliveredDoesNotCount(t *testing.T) {
	h := newNoteHarness(t)
	h.cfg.AsideEscalate = 20 * time.Millisecond
	var n atomic.Int32
	h.AsideWith("exec #1 quiet", h.heartbeatOpts("job:1:quiet", &n, true))
	h.liftQueued("r")
	time.Sleep(60 * time.Millisecond)
	if n.Load() != 0 {
		t.Errorf("a delivered heartbeat counted as an escalation: %d", n.Load())
	}
}

// An IDLE child's heartbeat never escalates: it waits for the parent, it is
// not wedged. A running child's does.
func TestSubAgent_IdleHeartbeatNeverEscalates(t *testing.T) {
	h, sa := testAgent(t, 4, "summarize the build log")
	h.cfg.AsideEscalate = 5 * time.Millisecond
	sa.mu.Lock()
	sa.startRunLocked()
	sa.mu.Unlock()
	sa.settle(agentIdle)
	go sa.heartbeat()
	defer sa.settle(agentDismissed)

	waitFor(t, 3*time.Second, func() bool {
		a, _ := kinds(h)
		return len(a) > 0
	})
	select {
	case <-h.wake:
		t.Error("an idle child's heartbeat woke the parent")
	case <-time.After(100 * time.Millisecond):
	}
	if _, notes := kinds(h); len(notes) != 0 || sa.escalations.Load() != 0 {
		t.Errorf("idle child escalated: notes %q, count %d", notes, sa.escalations.Load())
	}
}

func TestSubAgent_RunningHeartbeatEscalates(t *testing.T) {
	h, sa := testAgent(t, 5, "long job")
	h.cfg.AsideEscalate = 5 * time.Millisecond
	sa.mu.Lock()
	sa.startRunLocked()
	sa.mu.Unlock()
	go sa.heartbeat()
	defer sa.settle(agentDismissed)
	select {
	case <-h.wake:
	case <-time.After(3 * time.Second):
		t.Fatal("a running, silent child's heartbeat did not escalate")
	}
	waitFor(t, 2*time.Second, func() bool { return sa.escalations.Load() >= 1 })
}
