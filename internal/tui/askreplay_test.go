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

// A restored question (dund restart) is drawn at its transcript position —
// after the user message that started the turn — not at the top of the
// replayed history. dund appends it at the end of the log (showRestored), so
// its position is fixed by the transcript; the TUI must not float it above
// the first message. Pinned by askEntry: the question is the row after the
// user row, at index 1, not index 0.
func TestAskReplay_RestoredQuestionAtTranscriptPosition(t *testing.T) {
	ask := map[string]any{"ask_id": "k1", "call_id": "c1", "question": "which one?", "options": []any{"red", "blue"}}
	snap := evMsg{"type": "snapshot", "items": []any{
		map[string]any{"kind": "user", "id": "u1", "content": "go"},
		map[string]any{"kind": "tool_call", "tool": "ask_user", "call_id": "c1", "args": ask},
	}, "state": []any{map[string]any{"type": "turn_state", "phase": "waiting_answer", "queued": []any{}, "ask": ask, "replayed": true}}}

	// Fresh client: the snapshot replays the transcript in order, so the
	// question lands after the user row.
	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 100, 30)
	m = m.handleEvent(snap)
	if !m.asking || m.askEntry() != 1 {
		t.Fatalf("fresh: asking %v question at %d (want 1, after the user row):\n%s", m.asking, m.askEntry(), convoText(m))
	}
	// A reconnect past the replay window rebuilds from the snapshot: the row
	// is still at its transcript position, not floated to the top.
	m.skipHistory = true
	m = m.handleEvent(snap)
	if !m.asking || m.askEntry() != 1 {
		t.Fatalf("reconnect: asking %v question at %d (want 1, after the user row):\n%s", m.asking, m.askEntry(), convoText(m))
	}
}

// A restored question's row settles when the question is dismissed: the
// interruption ends the shown turn (done, reason interrupted) and the turn
// goes idle, so no open question remains. Pinned by askEntry: -1 after the
// dismissal, not the stale open row.
func TestAskReplay_DismissedQuestionSettles(t *testing.T) {
	ask := map[string]any{"ask_id": "k1", "call_id": "c1", "question": "which one?", "options": []any{"red", "blue"}}
	snap := evMsg{"type": "snapshot", "items": []any{
		map[string]any{"kind": "user", "id": "u1", "content": "go"},
		map[string]any{"kind": "tool_call", "tool": "ask_user", "call_id": "c1", "args": ask},
	}, "state": []any{map[string]any{"type": "turn_state", "phase": "waiting_answer", "queued": []any{}, "ask": ask, "replayed": true}}}

	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 100, 30)
	m = m.handleEvent(snap)
	if !m.asking || m.askEntry() < 0 {
		t.Fatalf("setup: question not open:\n%s", convoText(m))
	}
	// The dismissal: dund ends the shown turn with an interrupted done and an
	// idle turn (restoredReply's interrupt path), then a quiet notice.
	m = m.handleEvent(evMsg{"type": "done", "reason": "interrupted", "detail": "question dismissed"})
	m = m.handleEvent(evMsg{"type": "turn_state", "phase": "idle", "queued": []any{}})
	if m.asking {
		t.Errorf("still asking after dismissal:\n%s", convoText(m))
	}
	if m.askEntry() >= 0 {
		t.Errorf("the question row is still open after dismissal (want it settled, no open question):\n%s", convoText(m))
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
	// A restored question's answer is the wake message dund sends next; the
	// Answered event alone draws no "› value" row.
	m = m.handleEvent(evMsg{"type": "answered", "ask_id": "k1", "value": "red"})
	m = m.handleEvent(evMsg{"type": "user", "content": "[automatic] Answer to your question \"which one?\": red"})
	if m.asking || !strings.Contains(stripANSI(convoText(m)), "red") {
		t.Errorf("answer:\n%s", convoText(m))
	}
}

// A RESTORED question's answer is one row: the wake message (the user
// message dund sends with the answer) IS the answer, so the TUI must not
// draw a second "› value" row from the Answered event. A live question
// (TestAskReplay_SameRowsAsLive) keeps its "› value" row: there no wake
// message follows.
func TestAskReplay_AnswerIsOneRow(t *testing.T) {
	ask := map[string]any{"ask_id": "k1", "call_id": "c1", "question": "which one?", "options": []any{"red", "blue"}}
	snap := evMsg{"type": "snapshot", "items": []any{
		map[string]any{"kind": "user", "id": "u1", "content": "go"},
		map[string]any{"kind": "tool_call", "tool": "ask_user", "call_id": "c1", "args": ask},
	}, "state": []any{map[string]any{"type": "turn_state", "phase": "waiting_answer", "queued": []any{}, "ask": ask, "replayed": true}}}

	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 100, 30)
	m = m.handleEvent(snap)
	if !m.asking {
		t.Fatalf("setup: question not open:\n%s", convoText(m))
	}
	// The answer: dund sends the Answered event, then the wake user message.
	m = m.handleEvent(evMsg{"type": "answered", "ask_id": "k1", "value": "green"})
	m = m.handleEvent(evMsg{"type": "user", "content": "[automatic] Answer to your question \"which one?\": green"})
	text := stripANSI(convoText(m))
	if c := strings.Count(text, "green"); c != 1 {
		t.Errorf("the answer is in %d rows (want 1, the wake line):\n%s", c, text)
	}
	if !strings.Contains(text, "↻ resumed") {
		t.Errorf("the wake line is not drawn as the resume row:\n%s", text)
	}
	if strings.Contains(text, "› green") {
		t.Errorf("a second answer row:\n%s", text)
	}
}
