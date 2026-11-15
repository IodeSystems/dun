package tui

import (
	"strings"
	"testing"
)

// A client that arrives by snapshot (past the replay window) gets the turn
// from turn_state alone: busy, the open question, the queued messages.
func TestTurnState_SnapshotRestoresTheTurn(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 100, 30)
	m = m.handleEvent(evMsg{"type": "snapshot", "items": []any{map[string]any{"kind": "user", "id": "u1", "content": "go"}},
		"state": []any{map[string]any{"type": "turn_state", "phase": "waiting_answer", "since": 1000.0,
			"ask":    map[string]any{"question": "which one?", "options": []any{"a", "b"}},
			"queued": []any{map[string]any{"id": "q1", "text": "and also"}}}}})
	if !m.busy || !m.asking || m.queuedMsgs != 1 || m.queuedRow("q1") < 0 {
		t.Fatalf("busy=%v asking=%v queued=%d", m.busy, m.asking, m.queuedMsgs)
	}
	if !strings.Contains(stripANSI(convoText(m)), "which one?") {
		t.Errorf("the question is not shown:\n%s", convoText(m))
	}
	// The turn moves on: the question closes; the engine dies: the row says
	// its message was never delivered.
	m = m.handleEvent(evMsg{"type": "turn_state", "phase": "working", "queued": []any{map[string]any{"id": "q1", "text": "and also"}}})
	if m.asking {
		t.Error("a question the turn no longer waits on is still open")
	}
	m = m.handleEvent(evMsg{"type": "turn_state", "phase": "idle", "queued": []any{}})
	if m.busy || m.queuedRow("q1") < 0 || !strings.Contains(stripANSI(convoText(m)), "not delivered") {
		t.Errorf("lost message not said:\n%s", convoText(m))
	}
}
