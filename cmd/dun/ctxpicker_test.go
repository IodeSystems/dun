package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// In /context config's picker, enter on a flag fills it in and waits for
// its value; it does not run the flag alone (USER 2026-10-27).
func TestContextPicker_EnterOnAFlagWaitsForItsValue(t *testing.T) {
	rec := &recWC{}
	m := newTUIModel(&dunProc{stdin: rec}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m = typeStr(m, "/context config")
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // the picker
	m = nm.(tuiModel)
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // on --max
	m = nm.(tuiModel)
	if strings.Contains(rec.String(), "ctxconfig") {
		t.Fatalf("ran without a value: %s", rec.String())
	}
	if got := m.input.Value(); got != "/context config --max " {
		t.Fatalf("composer: %q", got)
	}
	m = typeStr(m, "512k")
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !strings.Contains(rec.String(), `"action":"--max 512k"`) || strings.Count(rec.String(), "ctxconfig") != 1 {
		t.Errorf("sent: %s", rec.String())
	}
}
