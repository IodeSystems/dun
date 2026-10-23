package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func askingVtui(t *testing.T, opts ...any) *vtui {
	t.Helper()
	v := newVtui(80, 24)
	v.m.proc = &dunProc{stdin: &echoWC{}}
	v.event(map[string]any{"type": "ready", "tools": []any{"ask_user"}})
	v.send("start")
	v.m.busy = true // the ask is a tool call inside a running turn
	v.event(map[string]any{"type": "ask", "question": "Which one?", "options": opts})
	if !v.m.asking {
		t.Fatal("precondition: the picker should be open")
	}
	return v
}

// esc in the picker puts the question away: dun keeps running, nothing is
// answered, and the question's row says it is waiting.
func TestAsk_EscParksWithoutQuitting(t *testing.T) {
	v := askingVtui(t, "A", "B")
	v.m = key(v.m, kDown) // move off the first option, to see it kept
	v.m = key(v.m, kEsc)
	if v.m.quitting {
		t.Fatal("esc in the picker quit dun")
	}
	if v.m.asking || !v.m.askParked {
		t.Fatalf("asking=%v parked=%v", v.m.asking, v.m.askParked)
	}
	if strings.Contains(v.convo(), "› A") || strings.Contains(v.convo(), "› B") {
		t.Error("parking sent an answer")
	}
	scr := v.view()
	if !strings.Contains(scr, "Which one?") || !strings.Contains(scr, "unanswered — select it, enter to answer") {
		t.Errorf("the parked question is not marked:\n%s", scr)
	}
	if v.m.focus != focusInput {
		t.Error("the composer should have the keys back")
	}
}

// The parked question is selected and reopened with enter, where it was left,
// and answering it settles the row.
func TestAsk_ReopenAndAnswer(t *testing.T) {
	v := askingVtui(t, "A", "B")
	v.m = key(v.m, kDown)
	v.m = key(v.m, kEsc)

	v.m = key(v.m, kTab)
	for v.m.sel > 0 && !v.m.convo[v.m.sel].askOpen {
		v.m = key(v.m, kUp)
	}
	if !v.m.convo[v.m.sel].askOpen {
		t.Fatal("could not select the question")
	}
	v.m = key(v.m, kEnter)
	if !v.m.asking || v.m.askParked || v.m.askSel != 1 {
		t.Fatalf("not reopened where it was left: asking=%v parked=%v sel=%d", v.m.asking, v.m.askParked, v.m.askSel)
	}
	v.m = key(v.m, kEnter) // pick B
	if v.m.asking || v.m.askParked {
		t.Fatalf("answering did not close the question: asking=%v parked=%v", v.m.asking, v.m.askParked)
	}
	if !strings.Contains(v.convo(), "› B") {
		t.Errorf("answer not in the conversation:\n%s", v.convo())
	}
	if strings.Contains(v.view(), "unanswered") {
		t.Error("an answered question is still marked unanswered")
	}
}

// A free-text question parks and comes back as its text field. A message sent
// while it is parked goes out as a message, and the question stays open.
func TestAsk_FreeTextParksAndMessagesStillSend(t *testing.T) {
	v := askingVtui(t)
	if !v.m.customAnswer {
		t.Fatal("precondition: a free-text question opens its text field")
	}
	v.m = key(v.m, kEsc)
	if v.m.quitting || !v.m.askParked {
		t.Fatalf("free-text esc: quitting=%v parked=%v", v.m.quitting, v.m.askParked)
	}
	v.m = typeStr(v.m, "meanwhile")
	v.m = key(v.m, kEnter)
	if !strings.Contains(v.convo(), "meanwhile") || !v.m.askParked {
		t.Fatalf("message while parked: parked=%v\n%s", v.m.askParked, v.convo())
	}
	v.m = key(v.m, kTab)
	for v.m.sel > 0 && !v.m.convo[v.m.sel].askOpen {
		v.m = key(v.m, kUp)
	}
	v.m = key(v.m, kEnter)
	if !v.m.asking || !v.m.customAnswer {
		t.Fatalf("free-text question did not reopen as its field: asking=%v custom=%v", v.m.asking, v.m.customAnswer)
	}
}

// esc inside the custom row goes back to the options, not out of the question.
func TestAsk_EscFromCustomRowReturnsToOptions(t *testing.T) {
	v := askingVtui(t, "A")
	v.m = key(v.m, kDown) // the custom row
	v.m = key(v.m, kEnter)
	if !v.m.customAnswer {
		t.Fatal("precondition: custom field open")
	}
	v.m = key(v.m, kEsc)
	if !v.m.asking || v.m.customAnswer || v.m.askParked {
		t.Errorf("asking=%v custom=%v parked=%v", v.m.asking, v.m.customAnswer, v.m.askParked)
	}
}

// esc with nothing to leave says how to exit, in the status line, and quits
// nothing; ctrl+c still quits.
func TestEsc_ShowsHowToExit(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.m = key(v.m, kEsc)
	if v.m.quitting {
		t.Fatal("esc quit dun")
	}
	if !strings.Contains(v.view(), "to exit: ctrl+c or /exit") {
		t.Errorf("no exit hint:\n%s", v.view())
	}
	v.m.flashUntil = v.m.flashUntil.Add(-flashFor) // as if it expired
	if strings.Contains(v.view(), "to exit:") {
		t.Error("the hint did not go away")
	}
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !v.m.quitting {
		t.Error("ctrl+c should still quit")
	}
}

// The question panel going away must not move the conversation: not when the
// panel closes, and not when the answer and the reply arrive after it. It used
// to grow the pane, scroll every row down, then scroll them back (the bounce).
func TestAsk_ClosingDoesNotMoveTheConversation(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
		back int // rows scrolled back first
	}{{"answer", kEnter, 0}, {"park", kEsc, 0}, {"scrolled back", kEnter, 3}} {
		t.Run(tc.name, func(t *testing.T) {
			v := tallConvo(t)
			v.m.proc = &dunProc{stdin: &echoWC{}}
			v.m.busy = true
			v.event(map[string]any{"type": "ask", "question": "Which?", "options": []any{"A", "B", "C"}})
			for i := 0; i < tc.back; i++ {
				v.m = key(v.m, kUp) // on the first option: scrolls the conversation
			}
			top := func() string { return v.m.vp.lines[v.m.vp.YOffset] }
			yoff, row := v.m.vp.YOffset, top()

			nm, _ := v.m.Update(tc.key) // no echo yet: the engine answers later
			v.m = nm.(tuiModel)
			if v.m.asking {
				t.Fatal("the panel should be closed")
			}
			if v.m.vp.YOffset != yoff || top() != row {
				t.Fatalf("closing moved the conversation: yoff %d -> %d, top %q -> %q", yoff, v.m.vp.YOffset, row, top())
			}
			// Answered here, or (parked) on another client.
			v.event(map[string]any{"type": "answered", "value": "A"})
			v.event(map[string]any{"type": "token", "text": "ok"})
			if v.m.vp.YOffset != yoff || top() != row {
				t.Fatalf("the answer moved the conversation: yoff %d -> %d, top %q -> %q", yoff, v.m.vp.YOffset, row, top())
			}
			// Enough new rows end the hold, and the pane follows the bottom again.
			for i := 0; i < 10; i++ {
				v.event(map[string]any{"type": "token", "text": "\nmore"})
			}
			v.event(map[string]any{"type": "done"})
			if v.m.convoHold != 0 {
				t.Errorf("hold still on after the content passed it: %d", v.m.convoHold)
			}
			if tc.back == 0 && !v.m.vp.AtBottom() {
				t.Error("pinned pane should follow the new rows")
			}
		})
	}
}

// draftVtui has "half a thought" in the composer, cursor after "half", when
// a question arrives.
func draftVtui(t *testing.T, ask map[string]any) *vtui {
	t.Helper()
	v := newVtui(80, 24)
	v.m.proc = &dunProc{stdin: &echoWC{}}
	v.event(map[string]any{"type": "ready", "tools": []any{"ask_user"}})
	v.send("start")
	v.m.busy = true
	v.m = typeStr(v.m, "half a thought")
	v.m.input.cursor = 4
	ask["type"] = "ask"
	v.event(ask)
	if !v.m.asking {
		t.Fatal("precondition: the picker should be open")
	}
	return v
}

// A draft in the composer when a question arrives comes back, with its cursor,
// however the question ends.
func TestAsk_DraftComesBack(t *testing.T) {
	enter := func(v *vtui) { v.m = key(v.m, kEnter) }
	for name, end := range map[string]func(v *vtui){
		"option": enter,
		"custom answer": func(v *vtui) {
			for range 3 {
				v.m = key(v.m, kDown)
			}
			v.m = typeStr(v.m, "mine")
			enter(v)
		},
		"detail": func(v *vtui) {
			v.m = key(v.m, kN)
			v.m = typeStr(v.m, "because")
			enter(v) // saves the detail
			enter(v) // sends the option with it
		},
		"multi": func(v *vtui) {
			v.m.askMulti, v.m.askChecked = true, make([]bool, len(v.m.askOptions))
			enter(v) // check A
			for range 3 {
				v.m = key(v.m, kDown) // to the done row
			}
			enter(v)
		},
		"park":           func(v *vtui) { v.m = key(v.m, kEsc) },
		"interrupted":    func(v *vtui) { v.event(map[string]any{"type": "done", "reason": "interrupted"}) },
		"turn ended":     func(v *vtui) { v.event(map[string]any{"type": "done"}) },
		"engine stopped": func(v *vtui) { v.event(map[string]any{"type": "engine", "state": "stopped"}) },
	} {
		t.Run(name, func(t *testing.T) {
			v := draftVtui(t, map[string]any{"question": "Which?", "options": []any{"A", "B"}})
			end(v)
			if v.m.asking {
				t.Fatal("the question is still open")
			}
			if got := v.m.input.Value(); got != "half a thought" || v.m.input.cursor != 4 {
				t.Errorf("composer = %q cursor %d, want the draft back with cursor 4", got, v.m.input.cursor)
			}
		})
	}
	// Parked, the composer is the draft again; what is typed then is the
	// draft the next close gives back.
	v := draftVtui(t, map[string]any{"question": "Which?", "options": []any{"A", "B"}})
	v.m = key(v.m, kEsc)
	v.m = typeStr(v.m, " more")
	v.m = key(v.m, kTab)
	for v.m.sel > 0 && !v.m.convo[v.m.sel].askOpen {
		v.m = key(v.m, kUp)
	}
	v.m = key(v.m, kEnter) // reopen
	if !v.m.asking {
		t.Fatal("did not reopen")
	}
	v.m = key(v.m, kEnter)
	if got := v.m.input.Value(); got != "half more a thought" {
		t.Errorf("after reopen and answer: composer = %q", got)
	}
}

// tab or → on an option opens it in the custom field to edit, cursor at the
// end; enter sends the edited text, esc goes back to the options unsent.
func TestAsk_TabEditsTheOption(t *testing.T) {
	kRight := tea.KeyMsg{Type: tea.KeyRight}
	for name, k := range map[string]tea.KeyMsg{"tab": kTab, "right": kRight} {
		t.Run(name, func(t *testing.T) {
			v := askingVtui(t, "A", "B", "C")
			v.m = key(v.m, kDown) // B
			v.m = key(v.m, k)
			if !v.m.customAnswer || v.m.input.Value() != "B" || v.m.input.cursor != 1 {
				t.Fatalf("custom=%v composer=%q cursor=%d", v.m.customAnswer, v.m.input.Value(), v.m.input.cursor)
			}
			if !strings.Contains(v.view(), "esc back to the options") {
				t.Errorf("no editing hint:\n%s", v.view())
			}
			v.m = typeStr(v.m, ", but blue")
			v.m = key(v.m, kEnter)
			if v.m.asking || !strings.Contains(v.convo(), "› B, but blue") {
				t.Errorf("asking=%v convo:\n%s", v.m.asking, v.convo())
			}
		})
	}
	t.Run("esc", func(t *testing.T) {
		v := askingVtui(t, "A", "B")
		v.m = key(v.m, kDown)
		v.m = key(v.m, kRight)
		v.m = key(v.m, kEsc)
		if !v.m.asking || v.m.customAnswer || v.m.askSel != 1 {
			t.Errorf("asking=%v custom=%v sel=%d", v.m.asking, v.m.customAnswer, v.m.askSel)
		}
		if strings.Contains(v.convo(), "› B") {
			t.Error("esc sent the answer")
		}
	})
	t.Run("with a detail", func(t *testing.T) {
		v := askingVtui(t, "A", "B")
		v.m = key(v.m, kN)
		v.m = typeStr(v.m, "soon")
		v.m = key(v.m, kEnter)
		v.m = key(v.m, kTab)
		if v.m.input.Value() != "A — soon" {
			t.Errorf("composer = %q, want what enter would send", v.m.input.Value())
		}
	})
	t.Run("multi", func(t *testing.T) {
		v := askingVtui(t, "A", "B", "C")
		v.m.askMulti, v.m.askChecked = true, make([]bool, 3)
		v.m = key(v.m, kEnter) // check A
		v.m = key(v.m, kDown)
		v.m = key(v.m, kDown) // highlight C
		v.m = key(v.m, kTab)
		if v.m.input.Value() != "A, C" {
			t.Errorf("composer = %q, want the checked set with the highlighted one", v.m.input.Value())
		}
	})
	t.Run("custom row", func(t *testing.T) {
		v := askingVtui(t, "A")
		v.m = key(v.m, kDown) // the custom row
		v.m = key(v.m, kTab)
		if v.m.customAnswer {
			t.Error("tab on the custom row has no option to edit")
		}
	})
}
