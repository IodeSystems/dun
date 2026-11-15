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

// A reconnect by snapshot, twice: the replayed state events add no line.
func TestSnapshot_ReplayedStateRepeatsNoNote(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 100, 30)
	snap := evMsg{"type": "snapshot", "items": []any{map[string]any{"kind": "user", "id": "u1", "content": "hi"}},
		"state": []any{
			map[string]any{"type": "server", "id": "rag", "action": "on", "message": "rag: on", "replayed": true},
			map[string]any{"type": "workspace", "branch": "dun/x", "replayed": true},
			map[string]any{"type": "turn_state", "phase": "idle", "queued": []any{}, "replayed": true},
		}}
	m = m.handleEvent(snap)
	m.skipHistory = true // a reconnect: the conversation is rebuilt from it
	m = m.handleEvent(snap)
	n := len(m.convo)
	m = m.handleEvent(snap)
	if len(m.convo) != n || strings.Contains(stripANSI(convoText(m)), "rag: on") {
		t.Errorf("notes repeated (%d → %d):\n%s", n, len(m.convo), convoText(m))
	}
	if m.branch != "dun/x" {
		t.Error("the state itself was not applied")
	}
}
