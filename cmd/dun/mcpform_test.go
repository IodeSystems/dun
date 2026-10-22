package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/iodesystems/dun"
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

// The last row is the placement: direct preselected, space switches it to the
// shell, the panel says what the shell costs, and enter sends via:"shell".
func TestMCPForm_PlacementDirectByDefault(t *testing.T) {
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	m.openMCPForm()
	m.mcpFields[0].SetValue("corp-docs")
	m.mcpFields[1].SetValue("corp-mcp --stdio")
	var cur tea.Model = m
	for i := 0; i < 3; i++ {
		cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	tm := cur.(tuiModel)
	if tm.mcpStep != mcpViaStep || tm.mcpShell || !strings.Contains(stripANSI(tm.mcpFormPanel()), "(•) direct") {
		t.Fatalf("step %d shell %v:\n%s", tm.mcpStep, tm.mcpShell, stripANSI(tm.mcpFormPanel()))
	}
	cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	tm = cur.(tuiModel)
	if v := stripANSI(tm.mcpFormPanel()); !tm.mcpShell || !strings.Contains(v, "(•) in the shell") || !strings.Contains(v, "no docs indexing") {
		t.Fatalf("space must switch to the shell and say what it loses:\n%s", v)
	}
	cur.Update(tea.KeyMsg{Type: tea.KeyEnter})
	var got map[string]string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("sent %q: %v", buf.String(), err)
	}
	if got["id"] != "mcp" || !strings.HasPrefix(got["action"], "add ") {
		t.Fatalf("sent %v", got)
	}
	var spec dun.ServerSpec
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got["action"], "add ")), &spec); err != nil || spec.Via != dun.ViaShell || spec.Command != "corp-mcp" {
		t.Errorf("spec %+v %v", spec, err)
	}
}

// For a URL with no bearer the form goes on to an oauth login; an empty
// client id skips the rest of it, and a command never sees it.
func TestMCPForm_OAuthRows(t *testing.T) {
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	m.openMCPForm()
	m.mcpFields[mcpID].SetValue("snow")
	m.mcpFields[mcpServer].SetValue("https://acct.snowflakecomputing.com/mcp")
	var cur tea.Model = m
	for i := 0; i < 3; i++ {
		cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	tm := cur.(tuiModel)
	if tm.mcpStep != mcpClient || !strings.Contains(stripANSI(tm.mcpFormPanel()), "oauth login (optional)") {
		t.Fatalf("step %d:\n%s", tm.mcpStep, stripANSI(tm.mcpFormPanel()))
	}
	cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeyTab}) // empty client id: straight to the placement
	if tm = cur.(tuiModel); tm.mcpStep != mcpViaStep {
		t.Fatalf("an empty client id must skip the oauth rows: step %d", tm.mcpStep)
	}
	cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	tm = cur.(tuiModel)
	tm.mcpFields[mcpClient].SetValue("c1")
	tm.mcpFields[mcpScopes].SetValue("openid, email")
	tm.mcpFields[mcpSend].SetValue("jwt")
	cur = tm
	for i := 0; i < 4; i++ {
		cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if tm = cur.(tuiModel); tm.mcpStep != mcpSend || !strings.Contains(tm.mcpErr, "access_token or id_token") {
		t.Fatalf("step %d err %q", tm.mcpStep, tm.mcpErr)
	}
	tm.mcpFields[mcpSend].SetValue("id_token")
	cur, _ = tm.Update(tea.KeyMsg{Type: tea.KeyTab})
	cur.Update(tea.KeyMsg{Type: tea.KeyEnter})
	var got map[string]string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("sent %q: %v", buf.String(), err)
	}
	var spec dun.ServerSpec
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got["action"], "add ")), &spec); err != nil || spec.OAuth == nil ||
		spec.OAuth.ClientID != "c1" || spec.OAuth.Send != "id_token" || strings.Join(spec.OAuth.Scopes, " ") != "openid email" {
		t.Errorf("spec %+v %+v %v", spec, spec.OAuth, err)
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
