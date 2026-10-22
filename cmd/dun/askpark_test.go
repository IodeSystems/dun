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
