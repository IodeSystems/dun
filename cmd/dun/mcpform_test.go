package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestMCPFormSpec_URLOrCommand(t *testing.T) {
	s := mcpFormSpec(" corp ", "https://corp/mcp", " tok ")
	if s.ID != "corp" || s.URL != "https://corp/mcp" || s.Bearer != "tok" || s.Command != "" {
		t.Errorf("remote: %+v", s)
	}
	s = mcpFormSpec("db", "db-mcp --stdio --root /x", "tok")
	if s.Command != "db-mcp" || len(s.Args) != 3 || s.URL != "" || s.Bearer != "" {
		t.Errorf("a command line is a stdio server and takes no bearer: %+v", s)
	}
	for _, bad := range []string{"", "Corp", "-x", "a b", "a.b"} {
		if mcpIDOK(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The token never reaches the screen or the slash history.
func TestMCPForm_MasksTheToken(t *testing.T) {
	m := newTUIModel(nil, ".")
	m.openMCPForm()
	m.mcpStep = 2
	m.mcpFields[2].SetValue("sk-very-secret")
	if v := stripANSI(m.mcpFormPanel()); strings.Contains(v, "sk-very-secret") {
		t.Errorf("token shown:\n%s", v)
	}
}

// Each MCP prompt is a slash command; its arguments are positional, the last
// taking the rest of the line, and a missing required one is refused.
func TestCatalog_PromptsBecomeSlashCommands(t *testing.T) {
	m := newTUIModel(nil, ".")
	m.noteCatalog(evMsg{"prompts": []any{map[string]any{"server": "corp", "name": "review",
		"arguments": []any{map[string]any{"name": "path", "required": true}, map[string]any{"name": "focus"}}}},
		"skills": []any{map[string]any{"name": "deploy", "description": "ship it", "source": "user"}}})
	if !m.commandNamed("corp:review") || len(m.allSlash()) != len(slashCommands)+1 {
		t.Fatal("the prompt is not a command")
	}
	var c slashCmd
	for _, x := range m.allSlash() {
		if x.name == "corp:review" {
			c = x
		}
	}
	if c.args != "<path> [focus]" {
		t.Errorf("args %q", c.args)
	}
	c.run(&m, nil)
	if v := stripANSI(m.convo[len(m.convo)-1].collapsed); !strings.Contains(v, "needs path") {
		t.Errorf("missing required argument: %q", v)
	}
	if len(m.skills) != 1 || m.skills[0].Name != "deploy" {
		t.Errorf("skills %+v", m.skills)
	}
}

// A word that is also a key name, arriving as one burst, is text.
func TestBurstOfRunes_KeyNamedWordIsTyped(t *testing.T) {
	m := newTUIModel(nil, ".")
	m.input.Focus()
	var cur tea.Model = m
	for _, w := range []string{"a", " ", "up", " ", "down", " ", "b"} {
		k := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(w)}
		if w == " " {
			k = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
		}
		cur, _ = cur.Update(k)
	}
	tm := cur.(tuiModel)
	if got := tm.input.Value(); got != "a up down b" {
		t.Errorf("input = %q", got)
	}
}
