package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The text row is in the option list (USER 2026-11-10: "why not type
// inline?"). Typing on any row is an answer in the person's own words: it
// moves to the text row and types there, with no extra step.
func TestAskInline_TypingOnAnOptionJumpsToTheTextRow(t *testing.T) {
	v := askingVtui(t, "red", "green")
	if v.m.askSel != 0 {
		t.Fatalf("precondition: on the first option, sel=%d", v.m.askSel)
	}
	v.m = typeStr(v.m, "blue")
	if !v.m.onAskText() || v.m.askField.Value() != "blue" {
		t.Fatalf("on text=%v text=%q", v.m.onAskText(), v.m.askField.Value())
	}
	// The words are on the row in the list; nothing opened below it.
	panel := strings.Split(stripANSI(v.m.askPanel()), "\n")
	if len(panel) != 3 || !strings.Contains(panel[2], "› blue") {
		t.Fatalf("panel:\n%s", strings.Join(panel, "\n"))
	}
	v.m = key(v.m, kEnter)
	if v.m.asking || !strings.Contains(v.convo(), "› blue") {
		t.Errorf("asking=%v convo:\n%s", v.m.asking, v.convo())
	}
}

// A paste is typing: from an option row it lands in the text row too.
func TestAskInline_PasteOnAnOptionLandsInTheTextRow(t *testing.T) {
	v := askingVtui(t, "A", "B")
	nm, _ := v.m.Update(tea.PasteMsg{Content: "pasted words"})
	v.m = nm.(tuiModel)
	if !v.m.onAskText() || v.m.askField.Value() != "pasted words" {
		t.Errorf("on text=%v text=%q", v.m.onAskText(), v.m.askField.Value())
	}
}

// Every printable key types, on any row: "no" typed on an option row lands in
// the text row and is sent. A detail is ctrl+d, not a letter. Space is the
// one printable hotkey: it toggles a multi-select option row.
func TestAskInline_EveryLetterTypes(t *testing.T) {
	t.Run("no from an option row", func(t *testing.T) {
		v := askingVtui(t, "A", "B")
		v.m = typeStr(v.m, "no")
		if v.m.noting || !v.m.onAskText() || v.m.askField.Value() != "no" {
			t.Fatalf("noting=%v on text=%v text=%q", v.m.noting, v.m.onAskText(), v.m.askField.Value())
		}
		v.m = key(v.m, kEnter)
		if v.m.asking || !strings.Contains(v.convo(), "› no") {
			t.Errorf("asking=%v convo:\n%s", v.m.asking, v.convo())
		}
	})
	t.Run("ctrl+d on an option starts a detail", func(t *testing.T) {
		v := askingVtui(t, "A", "B")
		v.m = key(v.m, kCtrlD)
		if !v.m.noting || v.m.onAskText() || v.m.askField.Value() != "" {
			t.Fatalf("noting=%v on text=%v text=%q", v.m.noting, v.m.onAskText(), v.m.askField.Value())
		}
		if !strings.Contains(v.view(), "type a detail") {
			t.Errorf("no detail hint:\n%s", v.view())
		}
		// The detail's own letters type into the detail, not the text row.
		v.m = typeStr(v.m, "now")
		if v.m.input.Value() != "now" || v.m.askField.Value() != "" {
			t.Errorf("detail=%q text=%q", v.m.input.Value(), v.m.askField.Value())
		}
	})
	t.Run("the hint names ctrl+d", func(t *testing.T) {
		v := askingVtui(t, "A", "B")
		if !strings.Contains(v.view(), "ctrl+d detail") {
			t.Errorf("hint:\n%s", v.view())
		}
	})
	t.Run("ctrl+d on the text row deletes, no detail", func(t *testing.T) {
		v := askingVtui(t, "A", "B")
		v.m = typeStr(v.m, "xy")
		v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyLeft})
		v.m = key(v.m, kCtrlD)
		if v.m.noting || v.m.askField.Value() != "x" {
			t.Errorf("noting=%v text=%q", v.m.noting, v.m.askField.Value())
		}
	})
	t.Run("space on a multi option toggles; on the text row it types", func(t *testing.T) {
		v := askingVtui(t, "A", "B")
		v.m.askMulti, v.m.askChecked = true, make([]bool, 2)
		v.m = key(v.m, runesKey([]rune(" ")))
		if !v.m.askChecked[0] || v.m.onAskText() || v.m.askField.Value() != "" {
			t.Fatalf("checked=%v on text=%v text=%q", v.m.askChecked, v.m.onAskText(), v.m.askField.Value())
		}
		v.m = typeStr(v.m, "a b")
		if v.m.askField.Value() != "a b" {
			t.Errorf("space on the text row should type: %q", v.m.askField.Value())
		}
	})
}

// Esc twice from an option row with words typed: the first clears them, the
// second parks the question.
func TestAskInline_EscTwiceFromAnOptionRow(t *testing.T) {
	v := askingVtui(t, "A", "B")
	v.m = typeStr(v.m, "draft")
	v.m = key(v.m, kUp)
	v.m = key(v.m, kUp) // back on A, the words still in the row
	if v.m.askSel != 0 || v.m.askField.Value() != "draft" {
		t.Fatalf("sel=%d text=%q", v.m.askSel, v.m.askField.Value())
	}
	v.m = key(v.m, kEsc)
	if !v.m.asking || v.m.askField.Value() != "" || v.m.askSel != 0 {
		t.Fatalf("first esc: asking=%v text=%q sel=%d", v.m.asking, v.m.askField.Value(), v.m.askSel)
	}
	v.m = key(v.m, kEsc)
	if v.m.asking || !v.m.askParked {
		t.Errorf("second esc: asking=%v parked=%v", v.m.asking, v.m.askParked)
	}
}

// Multi-select: words in the text row are one more selected entry. The done
// row counts and sends them even when enter was never pressed on the row.
func TestAskInline_MultiTextIsOneMoreEntry(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &echoWC{}}, "/ws")
	m = resized(m, 80, 24)
	m = m.handleEvent(evMsg{"type": "ask", "question": "Fix which?", "options": []any{"lint", "tests"}, "multi": true})
	m = key(m, kEnter) // check lint
	m = typeStr(m, "bench")
	if !m.onAskText() {
		t.Fatalf("typing should be on the text row, sel=%d", m.askSel)
	}
	if !strings.Contains(stripANSI(m.askPanel()), "submit 2 selected") {
		t.Errorf("done should count the typed entry:\n%s", stripANSI(m.askPanel()))
	}
	m = key(m, kDown) // to done
	m = key(m, kEnter)
	if m.asking || !strings.Contains(convoText(m), "lint, bench") {
		t.Errorf("asking=%v convo:\n%s", m.asking, convoText(m))
	}
}

// A question whose answer must be one of its options has no text row:
// typing and pasting do nothing, the hotkeys that change the answer are off,
// and the multi done row follows the options.
func TestAskInline_NoCustomHasNoTextRow(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &echoWC{}}, "/ws")
	m = resized(m, 80, 24)
	m = m.handleEvent(evMsg{"type": "ask", "question": "Commit?", "options": []any{"commit", "cancel"}, "no_custom": true})
	panel := stripANSI(m.askPanel())
	if strings.Contains(panel, "type an answer") || len(strings.Split(panel, "\n")) != 2 {
		t.Fatalf("no text row expected:\n%s", panel)
	}
	if strings.Contains(stripANSI(m.View().Content), "edit") {
		t.Errorf("hint offers editing:\n%s", stripANSI(m.View().Content))
	}
	m = typeStr(m, "maybe")
	m = key(m, kCtrlD)
	m = key(m, kTab)
	nm, _ := m.Update(tea.PasteMsg{Content: "pasted"})
	m = nm.(tuiModel)
	if m.askSel != 0 || m.noting || m.askField.Value() != "" {
		t.Fatalf("sel=%d noting=%v text=%q", m.askSel, m.noting, m.askField.Value())
	}
	m = key(m, kDown)
	m = key(m, kDown) // stops at the last option
	if m.askSel != 1 {
		t.Fatalf("sel=%d, want the last option", m.askSel)
	}
	m = key(m, kEnter)
	if m.asking || !strings.Contains(convoText(m), "cancel") {
		t.Errorf("asking=%v convo:\n%s", m.asking, convoText(m))
	}

	// Multi: the done row is right after the options.
	m = newTUIModel(&dunProc{stdin: &echoWC{}}, "/ws")
	m = resized(m, 80, 24)
	m = m.handleEvent(evMsg{"type": "ask", "question": "Which?", "options": []any{"a", "b"}, "multi": true, "no_custom": true})
	m = key(m, kEnter) // check a
	m = key(m, kDown)
	m = key(m, kDown) // done
	m = key(m, kEnter)
	if m.asking || !strings.Contains(convoText(m), "a") {
		t.Errorf("multi: asking=%v sel=%d convo:\n%s", m.asking, m.askSel, convoText(m))
	}

	// no_custom without options would leave nothing to answer with: ignored.
	m = newTUIModel(&dunProc{stdin: &echoWC{}}, "/ws")
	m = m.handleEvent(evMsg{"type": "ask", "question": "Name?", "no_custom": true})
	if !m.onAskText() {
		t.Errorf("a question with no options keeps its text row")
	}
}
