package tui

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
	m = m.feed(evMsg{"type": "settings", "model": "deepseek/x", "url": "https://or/v1", "effort": "high"})
	m.noteServers(evMsg{"servers": []any{
		map[string]any{"id": "vay", "auto": true, "via": "shell", "hosted": true, "tools": float64(112)},
		map[string]any{"id": "notes", "auto": true, "via": "shell"},
	}})
	var rows []string
	for _, s := range m.statusSections(120) {
		rows = append(rows, s.label+" "+ansi.Strip(s.value))
	}
	all := strings.Join(rows, "\n") + "\n" + ansi.Strip(m.statusDetail(120))
	for _, want := range []string{"effort high · deep thinking", "scripted in the shell · 112 tools", "scripted · not hosted"} {
		if !strings.Contains(all, want) {
			t.Errorf("lacks %q:\n%s", want, all)
		}
	}
}

// /status says what effort default does on this server when the engine
// learned it (the settings event's effortWords).
func TestStatus_EffortDefaultFromTheServer(t *testing.T) {
	m := buildModel(2, 2)
	m = m.feed(evMsg{"type": "settings", "model": "chat", "url": "https://llm/v1", "effort": "default",
		"effortWords": "nothing sent: the server thinks at medium"})
	m.statusSlash()
	if s := stripANSI(m.convo[len(m.convo)-1].collapsed); !strings.Contains(s, "effort      default · nothing sent: the server thinks at medium · /effort") {
		t.Errorf("/status:\n%s", s)
	}
}

// /status keeps the saved level and says what the model is sent when the
// engine's role table has it differ; the roles window shows the table as
// sent (USER 2026-11-18).
func TestStatus_SavedAndSentEffort(t *testing.T) {
	m := buildModel(2, 2)
	m = m.feed(evMsg{"type": "settings", "model": "g", "url": "https://llm/v1", "effort": "none", "tiers": []any{
		map[string]any{"name": "fast"},
		map[string]any{"name": "main", "role": "main", "source": "session", "wanted": "none", "effective": "minimal",
			"reason": "minimal (none not supported: reasoning.thinking_only=true)", "effortSource": "/effort"},
		map[string]any{"name": "fold", "role": "fold", "model": "small", "provider": "gw", "source": "side_calls fast · config", "wanted": "lowest", "effective": "none", "effortSource": "default"},
	}})
	m.statusSlash()
	if s := stripANSI(m.convo[len(m.convo)-1].collapsed); !strings.Contains(s, "effort      none (saved) · sent as minimal: minimal (none not supported") {
		t.Errorf("/status:\n%s", s)
	}
	if len(m.engineTiers) != 1 {
		t.Errorf("tier rows %+v", m.engineTiers)
	}
	all := stripANSI(strings.Join(m.roleTable(), "\n"))
	for _, want := range []string{"main         g", "fold         small on gw", "lowest   none", "side_calls fast · config / default"} {
		if !strings.Contains(all, want) {
			t.Errorf("role table lacks %q:\n%s", want, all)
		}
	}
}
