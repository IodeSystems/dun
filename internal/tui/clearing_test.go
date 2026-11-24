package tui

import (
	"strings"
	"testing"
	"time"
)

// nowTS is an event timestamp of now: a timing is said only for an
// operation just done (freshEvent).
func nowTS() string { return time.Now().Format(time.RFC3339Nano) }

// /clear mid-turn: the engine's clearing event shows a row naming what the
// clear waits for; a message sent meanwhile is queued; the reset takes the
// old conversation and the row, keeps the queued message, and says nothing
// of the time when it was quick.
func TestClearing_RowThenResetKeepsTheQueue(t *testing.T) {
	m := busyModel(t)
	m = m.feed(evMsg{"type": "user", "id": "u0", "content": "the old task"})
	m = m.feed(evMsg{"type": "token", "text": "Working on the old task."})
	m = m.feed(evMsg{"type": "clearing", "waiting": "exec"})
	inOrder(t, m, "Working on the old task.", "clearing: waiting for exec to stop")
	m = m.feed(evMsg{"type": "user", "id": "m1", "content": "the new task", "queued": true})
	inOrder(t, m, "clearing: waiting for exec to stop", "the new task (queued)")

	m = m.feed(evMsg{"type": "reset", "ts": nowTS(), "timing": map[string]any{"total_ms": 40.0,
		"phases": []any{map[string]any{"name": "stopping exec", "ms": 40.0}}}})
	m.refresh()
	view := stripANSI(m.View().Content)
	for _, gone := range []string{"the old task", "clearing:", "clear took"} {
		if strings.Contains(view, gone) {
			t.Errorf("%q still drawn after the reset:\n%s", gone, view)
		}
	}
	inOrder(t, m, "session cleared", "the new task (queued)")

	// Delivered into the cleared conversation: an ordinary row.
	m = m.feed(evMsg{"type": "delivered", "ids": []any{"m1"}})
	if strings.Contains(stripANSI(m.View().Content), "(queued)") {
		t.Error("still queued after delivered")
	}
}

// A slow clear says where its time went, once, in one line; a replayed
// reset (an old timestamp) does not.
func TestClearing_SlowClearSaysWhy(t *testing.T) {
	timing := map[string]any{"total_ms": 4200.0, "phases": []any{
		map[string]any{"name": "stopping exec", "ms": 4100.0},
		map[string]any{"name": "clear", "ms": 100.0},
	}}
	m := busyModel(t)
	m = m.feed(evMsg{"type": "reset", "ts": nowTS(), "timing": timing})
	inOrder(t, m, "session cleared", "clear took 4.2s: stopping exec 4.1s")
	if strings.Contains(stripANSI(m.View().Content), "clear 0.1s") {
		t.Error("a phase under a tenth of the total is named")
	}

	m = busyModel(t)
	m = m.feed(evMsg{"type": "reset", "ts": time.Now().Add(-time.Minute).Format(time.RFC3339Nano), "timing": timing})
	if strings.Contains(stripANSI(m.View().Content), "clear took") {
		t.Error("a replayed reset said its timing")
	}
}

// A slow start, watched by this TUI: one line naming the slowest phases,
// the time before the engine began counting among them.
func TestStartLine(t *testing.T) {
	ev := evMsg{"startup": map[string]any{"total_ms": 1800.0, "phases": []any{
		map[string]any{"name": "built-in servers", "ms": 1500.0},
		map[string]any{"name": "tools", "ms": 90.0},
	}}}
	if l := startLine(ev, time.Time{}); l != "" {
		t.Errorf("a 1.8s start said %q", l)
	}
	l := startLine(ev, time.Now().Add(-4*time.Second))
	if !strings.HasPrefix(l, "start took 4.") || !strings.Contains(l, ": launching the engine 2.") ||
		!strings.HasSuffix(l, ", built-in servers 1.5s") {
		t.Errorf("start line = %q", l)
	}

	// From the event: said once at ready, for a start this TUI waited on.
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = resized(m, 120, 40)
	m.starting, m.startingStart = true, time.Now().Add(-5*time.Second)
	m = m.feed(evMsg{"type": "ready", "ts": nowTS(), "tools": []any{"exec"}, "startup": ev["startup"]})
	inOrder(t, m, "start took 5.", "launching the engine 3.", "built-in servers 1.5s")
}
