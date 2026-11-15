package tui

import (
	"fmt"
	"strings"
	"testing"
)

func items(kv ...string) []any {
	var out []any
	for i := 0; i+1 < len(kv); i += 2 {
		it := map[string]any{"kind": kv[i], "content": kv[i+1]}
		if kv[i] == "user" {
			it["id"] = fmt.Sprintf("u%d", i)
		}
		out = append(out, it)
	}
	return out
}

// A reconnect by snapshot rebuilds the conversation: what was said while
// detached shows, nothing shows twice, and the draft and the bottom pin stay.
func TestReconnectSnapshot_ShowsWhatWasMissed(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 100, 30)
	m = m.handleEvent(evMsg{"type": "snapshot", "items": items("user", "first question", "assistant", "first answer")})
	m.input.SetValue("half typed")
	m.skipHistory = true // the connection dropped: a reconnect
	m.append(StErr.Render("lost the session service — reconnecting"))
	snap := evMsg{"type": "snapshot", "items": items("user", "first question", "assistant", "first answer",
		"user", "asked from the phone", "assistant", "answered while away"),
		"state": []any{map[string]any{"type": "turn_state", "phase": "idle", "queued": []any{}, "replayed": true}}}
	m = m.handleEvent(snap)
	text := stripANSI(convoText(m))
	for _, want := range []string{"first question", "first answer", "asked from the phone", "answered while away"} {
		if c := strings.Count(text, want); c != 1 {
			t.Errorf("%q shown %d times:\n%s", want, c, text)
		}
	}
	if strings.Contains(text, "resumed") {
		t.Errorf("a reconnect is not a resume:\n%s", text)
	}
	if m.input.Value() != "half typed" {
		t.Errorf("draft lost: %q", m.input.Value())
	}
	if !m.scrollPinned {
		t.Error("a reader at the bottom is no longer at the bottom")
	}
	// The same snapshot again changes nothing.
	n := len(m.convo)
	m = m.handleEvent(snap)
	if len(m.convo) != n {
		t.Errorf("a second snapshot added rows: %d → %d", n, len(m.convo))
	}
}

// A reader scrolled up keeps their place; an open question keeps its row
// and its picker.
func TestReconnectSnapshot_KeepsScrollAndQuestion(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 100, 12)
	var kv []string
	for i := range 30 {
		kv = append(kv, "user", fmt.Sprintf("question %d", i), "assistant", fmt.Sprintf("answer %d", i))
	}
	m = m.handleEvent(evMsg{"type": "snapshot", "items": items(kv...)})
	m = m.handleEvent(evMsg{"type": "ask", "ask_id": "k1", "question": "which one?", "options": []any{"a", "b"}})
	m.askSel = 1
	m.vp.SetYOffset(5)
	m.updateScrollPin()
	if m.scrollPinned {
		t.Fatal("setup: not scrolled up")
	}
	m.skipHistory = true
	kv = append(kv, "assistant", "while away")
	m = m.handleEvent(evMsg{"type": "snapshot", "items": items(kv...),
		"state": []any{map[string]any{"type": "turn_state", "phase": "waiting_answer", "queued": []any{}, "replayed": true,
			"ask": map[string]any{"ask_id": "k1", "question": "which one?", "options": []any{"a", "b"}}}}})
	if m.vp.YOffset != 5 || m.scrollPinned {
		t.Errorf("scroll moved: offset %d pinned %v", m.vp.YOffset, m.scrollPinned)
	}
	text := stripANSI(convoText(m))
	if c := strings.Count(text, "which one?"); c != 1 || !m.asking || m.askSel != 1 {
		t.Errorf("question rows %d, asking %v, sel %d:\n%s", c, m.asking, m.askSel, text)
	}
	if !strings.Contains(text, "while away") {
		t.Error("missed reply not shown")
	}
}
