package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// /status: the effort in force, and a scripted server as scripted, not off
// (USER 2026-10-27).
func TestStatus_EffortAndScriptedServers(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "settings", "model": "deepseek/x", "url": "https://or/v1", "effort": "high"})
	m.noteServers(evMsg{"servers": []any{
		map[string]any{"id": "vay", "auto": true, "via": "shell", "hosted": true, "tools": float64(112)},
		map[string]any{"id": "notes", "auto": true, "via": "shell"},
	}})
	var rows []string
	for _, s := range m.statusSections(120) {
		rows = append(rows, s.label+" "+ansi.Strip(s.value))
	}
	all := strings.Join(rows, "\n") + "\n" + ansi.Strip(m.statusDetail(120))
	for _, want := range []string{"effort high · thinks as long as it needs", "scripted in the shell · 112 tools", "scripted · not hosted"} {
		if !strings.Contains(all, want) {
			t.Errorf("lacks %q:\n%s", want, all)
		}
	}
}

// /status says what effort default does on this server when the engine
// learned it (the settings event's effortWords).
func TestStatus_EffortDefaultFromTheServer(t *testing.T) {
	m := buildModel(2, 2)
	m = m.handleEvent(evMsg{"type": "settings", "model": "chat", "url": "https://llm/v1", "effort": "default",
		"effortWords": "nothing sent: the server thinks at medium"})
	m.statusSlash()
	if s := stripANSI(m.convo[len(m.convo)-1].collapsed); !strings.Contains(s, "effort      default · nothing sent: the server thinks at medium · /effort") {
		t.Errorf("/status:\n%s", s)
	}
}
