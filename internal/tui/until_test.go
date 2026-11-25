package tui

import (
	"strings"
	"testing"
)

// The until event's line is on the status line, idle and busy; an event
// with no conditions clears it; a run's stop is a dim line.
func TestUntilStatus(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 160, 30)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m = m.handleEvent(evMsg{"type": "until", "conds": []any{map[string]any{"kind": "turns", "n": float64(3)}},
		"running": true, "turn": float64(2), "cap": float64(3), "line": "until: turn 2/3"})
	if got := m.readyWord() + m.untilTail(); got != "ready · until: turn 2/3" {
		t.Fatalf("idle status %q", got)
	}
	m.busy = true
	if v := stripANSI(m.View().Content); !strings.Contains(v, "working… · until: turn 2/3") {
		t.Errorf("busy status lacks the run:\n%s", v)
	}
	m.busy = false
	m = m.handleEvent(evMsg{"type": "notification", "kind": "until", "text": "until: stopped · turn cap 3 reached (turn 3 of 3)"})
	if v := stripANSI(m.View().Content); !strings.Contains(v, "⟳ until: stopped · turn cap 3 reached") {
		t.Errorf("stop line missing:\n%s", v)
	}
	m = m.handleEvent(evMsg{"type": "until"})
	if m.untilTail() != "" {
		t.Errorf("cleared: %q", m.untilTail())
	}
}

// The marker idle (between runs), running and paused on a question; parked,
// the question's status line keeps it.
func TestUntilMarker(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 160, 30)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	conds := []any{map[string]any{"kind": "plan"}, map[string]any{"kind": "turns", "n": float64(15)}}
	for _, c := range []struct {
		ev   evMsg
		want string
	}{
		{evMsg{"line": "until: plan · turns 15"}, "until: plan · turns 15"},
		{evMsg{"running": true, "turn": float64(3), "line": "until: turn 3/15 · plan 4 open, 2 ready"}, "until: turn 3/15 · plan 4 open, 2 ready"},
		{evMsg{"running": true, "paused": true, "turn": float64(3), "line": "until: paused (question) · turn 3/15"}, "until: paused (question) · turn 3/15"},
	} {
		c.ev["type"], c.ev["conds"], c.ev["cap"] = "until", conds, float64(15)
		m = m.handleEvent(c.ev)
		if v := stripANSI(m.View().Content); !strings.Contains(v, "ready · "+c.want) {
			t.Errorf("status lacks %q:\n%s", c.want, v)
		}
	}
	m.askParked = true
	m.focus = focusActivity
	if v := stripANSI(m.View().Content); !strings.Contains(v, "a question is waiting · until: paused (question)") {
		t.Errorf("parked question lacks the marker:\n%s", v)
	}
}
