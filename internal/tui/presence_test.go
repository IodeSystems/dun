package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// A key tells the engine the person is here, at most once per presencePing; a
// busy turn sends nothing.
func TestPresence_ThrottledPing(t *testing.T) {
	rec := &recWC{}
	m := newTUIModel(&dunProc{stdin: rec}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m.liveTurnDone = true
	press := func() {
		nm, _ := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
		m = nm.(tuiModel)
	}
	press()
	press()
	if n := strings.Count(rec.String(), `"presence"`); n != 1 {
		t.Fatalf("%d pings for two keys in a row, want 1: %s", n, rec.String())
	}
	m.lastPresence = time.Now().Add(-2 * presencePing)
	press()
	if n := strings.Count(rec.String(), `"presence"`); n != 2 {
		t.Errorf("%d pings after the throttle, want 2", n)
	}
	m.lastPresence = time.Time{}
	m.busy = true
	before := rec.String()
	press()
	if rec.String() != before {
		t.Error("pinged during a turn")
	}
}
