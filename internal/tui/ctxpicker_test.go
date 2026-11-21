package tui

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
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !strings.Contains(rec.String(), `"action":"--max 512k"`) || strings.Count(rec.String(), "ctxconfig") != 1 {
		t.Errorf("sent: %s", rec.String())
	}
}

// /context config loops: the picker offers it after the flags, lists the
// modes under it, and a run sends the loops control with what follows.
func TestContextConfigLoops(t *testing.T) {
	rec := &recWC{}
	m := newTUIModel(&dunProc{stdin: rec}, "/ws")
	labels := func(arg string) string {
		var names []string
		if l := contextChoices(&m, arg); l != nil {
			for _, c := range l.choices {
				names = append(names, c.label)
			}
		}
		return strings.Join(names, " ")
	}
	if got := labels("config "); !strings.HasSuffix(got, " loops") {
		t.Errorf("config picker: %q", got)
	}
	if got := labels("config loops "); got != "off count cut tolerance tools" {
		t.Errorf("loops picker: %q", got)
	}
	if got := labels("config loops to"); got != "tolerance tools" {
		t.Errorf("loops picker, to: %q", got)
	}
	m.runSlash("/context config loops cut")
	if !strings.Contains(rec.String(), `"loops"`) || !strings.Contains(rec.String(), `"action":"cut"`) {
		t.Errorf("sent: %s", rec.String())
	}
	if strings.Contains(rec.String(), "ctxconfig") {
		t.Errorf("loops went as a context flag: %s", rec.String())
	}
}
