package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// /usage and /context open one screen with tabs, not a block in the
// conversation; tab moves between the tabs, a usage event redraws the open
// one, esc closes it.
func TestStatsScreen(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	m = nm.(tuiModel)
	before := len(m.convo)

	m.runSlash("/usage")
	if m.activeScreen() == nil || m.statsPaging() == nil {
		t.Fatal("/usage: no screen")
	}
	if len(m.convo) != before {
		t.Errorf("/usage added %d blocks to the conversation", len(m.convo)-before)
	}
	view := func() string { return ansi.Strip(m.drawView()) }
	if v := view(); !strings.Contains(v, "▸ Usage") || !strings.Contains(v, "tokens") || !strings.Contains(v, "cost & limits") {
		t.Fatalf("usage tab:\n%s", v)
	}
	if lines := strings.Split(view(), "\n"); len(lines) > 30 {
		t.Errorf("%d rows on a 30-row terminal", len(lines))
	}

	// Live: a usage event redraws the open tab.
	nm, _ = m.Update(evMsg{"type": "usage", "processed": 12345.0, "turns": 1.0})
	m = nm.(tuiModel)
	if v := view(); !strings.Contains(v, "12,345") {
		t.Errorf("not redrawn after a usage event:\n%s", v)
	}

	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = nm.(tuiModel)
	if v := view(); !strings.Contains(v, "▸ Spend") || !strings.Contains(v, "this machine") {
		t.Errorf("tab did not move to Spend:\n%s", v)
	}
	nm, _ = m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	m = nm.(tuiModel)
	if v := view(); !strings.Contains(v, "▸ Context") || !strings.Contains(v, "window") {
		t.Errorf("c did not move to Context:\n%s", v)
	}

	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = nm.(tuiModel)
	if m.activeScreen() != nil {
		t.Error("esc left the screen up")
	}

	m.runSlash("/context")
	if v := m.statsPaging(); v == nil || v.tab != statsContext {
		t.Fatal("/context: not on the Context tab")
	}
	if len(m.convo) != before {
		t.Errorf("/context added %d blocks to the conversation", len(m.convo)-before)
	}
}
