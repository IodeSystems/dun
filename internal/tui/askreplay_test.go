package tui

import (
	"strings"
	"testing"
)

// A replayed ask_user is drawn as live: its question row, then the answer
// as the person's row; no run row.
func TestAskReplay_SameRowsAsLive(t *testing.T) {
	live := newTUIModel(&dunProc{}, "/ws")
	live = resized(live, 100, 30)
	live = live.handleEvent(evMsg{"type": "ask", "ask_id": "k1", "call_id": "c1", "question": "which one?", "options": []any{"red", "blue"}})
	live = live.handleEvent(evMsg{"type": "answered", "ask_id": "k1", "value": "blue"})

	rep := newTUIModel(&dunProc{}, "/ws")
	rep = resized(rep, 100, 30)
	rep.quietReplay = true
	rep.replay([]any{
		map[string]any{"kind": "tool_call", "tool": "ask_user", "call_id": "c1",
			"args": map[string]any{"question": "which one?", "options": []any{"red", "blue"}}},
		map[string]any{"kind": "tool_result", "tool": "ask_user", "call_id": "c1", "content": "blue"},
	})
	if got, want := stripANSI(convoText(rep)), stripANSI(convoText(live)); got != want {
		t.Errorf("replay:\n%s\nlive:\n%s", got, want)
	}
	if strings.Contains(stripANSI(convoText(rep)), "ask_user") {
		t.Errorf("a run row:\n%s", convoText(rep))
	}
}

// A refused ask_user was never shown live, so replay shows nothing for it;
// an interrupted one keeps its question and has no answer.
func TestAskReplay_RefusedAndUnanswered(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 100, 30)
	m.quietReplay = true
	m.replay([]any{
		map[string]any{"kind": "tool_call", "tool": "ask_user", "call_id": "c1", "args": map[string]any{"question": "bad?"}},
		map[string]any{"kind": "tool_result", "tool": "ask_user", "call_id": "c1", "content": "ERROR: ask_user needs at least two concrete options"},
		map[string]any{"kind": "tool_call", "tool": "ask_user", "call_id": "c2", "args": map[string]any{"question": "cut?"}},
		map[string]any{"kind": "tool_result", "tool": "ask_user", "call_id": "c2", "content": "ERROR: could not get an answer: interrupted"},
	})
	text := stripANSI(convoText(m))
	if strings.Contains(text, "bad?") || strings.Contains(text, "ERROR") || !strings.Contains(text, "cut?") {
		t.Errorf("got:\n%s", text)
	}
}

// A snapshot with the question still open: history draws its row, the
// state opens it, and it is one row.
func TestAskReplay_OpenQuestionIsOneRow(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 100, 30)
	ask := map[string]any{"ask_id": "k1", "call_id": "c1", "question": "which one?", "options": []any{"red", "blue"}}
	snap := evMsg{"type": "snapshot", "items": []any{
		map[string]any{"kind": "user", "id": "u1", "content": "go"},
		map[string]any{"kind": "tool_call", "tool": "ask_user", "call_id": "c1", "args": ask},
	}, "state": []any{map[string]any{"type": "turn_state", "phase": "waiting_answer", "queued": []any{}, "ask": ask, "replayed": true}}}
	m = m.handleEvent(snap)
	if c := strings.Count(stripANSI(convoText(m)), "which one?"); c != 1 || !m.asking || m.askEntry() < 0 {
		t.Fatalf("rows %d asking %v open %d:\n%s", c, m.asking, m.askEntry(), convoText(m))
	}
	// A reconnect by the same snapshot: still one row, still open.
	m.skipHistory = true
	m = m.handleEvent(snap)
	if c := strings.Count(stripANSI(convoText(m)), "which one?"); c != 1 || !m.asking || m.askEntry() < 0 {
		t.Fatalf("after reconnect: rows %d asking %v:\n%s", c, m.asking, convoText(m))
	}
	m = m.handleEvent(evMsg{"type": "answered", "ask_id": "k1", "value": "red"})
	if m.asking || !strings.Contains(stripANSI(convoText(m)), "red") {
		t.Errorf("answer:\n%s", convoText(m))
	}
}
