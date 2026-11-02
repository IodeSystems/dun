package tui

import (
	"bytes"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/enginecmd"
)

// mcpshell not installed (a binary-only install) is a setup step, not a
// failure: the splash says "not installed" in the secondary colour, never
// the failure red, and the one line under it says what is lost and where
// the install step is.
func TestSplash_MissingShellIsQuiet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DUN_HOME", t.TempDir())
	var sent bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&sent}}, t.TempDir())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "session", "id": "s1"})
	err := (&dun.MissingError{Command: "mcpshell"}).Error()
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"read"},
		"hint":    enginecmd.ServerHint([]dun.ServerState{{ID: dun.ServerShell, Err: err, Missing: true}}),
		"servers": []any{map[string]any{"id": "shell", "err": err, "missing": true}}})
	m = m.handleEvent(evMsg{"type": "settings", "model": "", "url": ""})
	s := stripANSI(convoText(m))
	if !strings.Contains(s, "shell not installed") || strings.Contains(s, "did not start") {
		t.Errorf("splash:\n%s", s)
	}
	if !strings.Contains(s, "shell off: mcpshell is not installed: no eval tool") || !strings.Contains(s, "dun doctor") {
		t.Errorf("no line naming what is lost and the fix:\n%s", s)
	}
	red := StErr.Render("x")
	if on, _, _ := strings.Cut(red, "x"); on != "" && strings.Contains(m.convo[0].collapsed, on) {
		t.Errorf("a missing tool is in the failure colour: %q", m.convo[0].collapsed)
	}
}
