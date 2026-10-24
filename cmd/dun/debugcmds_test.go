package main

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The debug commands are out of /help and the palette, counted on /help's
// last row; /help --debug lists them; typed in full one shows in the
// palette and runs, a prefix of one is not taken for it.
func TestDebugCommands_HiddenButTypeable(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	for name := range debugCommands {
		if !m.commandNamed(name) {
			t.Errorf("/%s is not a command", name)
		}
	}

	m.showHelp(false)
	help := stripANSI(m.convo[len(m.convo)-1].collapsed)
	for name := range debugCommands {
		if strings.Contains(help, "/"+name+" ") || strings.HasSuffix(help, "/"+name) {
			t.Errorf("/help lists /%s:\n%s", name, help)
		}
	}
	foot := fmt.Sprintf("%d debug commands hidden — /help --debug shows them", len(debugCommands))
	if !strings.HasSuffix(help, foot) || !strings.Contains(help, "/login") {
		t.Errorf("/help:\n%s", help)
	}
	m.showHelp(true)
	all := stripANSI(m.convo[len(m.convo)-1].collapsed)
	if !strings.Contains(all, "/scrolldebug") || strings.Contains(all, "hidden") {
		t.Errorf("/help --debug:\n%s", all)
	}

	m = typeStr(m, "/scr")
	if ms := m.paletteMatches(); len(ms) != 0 {
		t.Errorf("/scr shows %v", ms)
	}
	m = typeStr(m, "olldebug")
	if ms := m.paletteMatches(); len(ms) != 1 || ms[0].name != "scrolldebug" {
		t.Errorf("/scrolldebug typed in full: %v", ms)
	}
	n := len(m.convo)
	m = key(m, kEnter)
	if len(m.convo) <= n {
		t.Errorf("/scrolldebug did not run:\n%s", convoText(m))
	}
}
