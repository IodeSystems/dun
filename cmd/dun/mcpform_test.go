package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun"
)

func TestMCPFormSpec_URLOrCommand(t *testing.T) {
	s, _ := mcpFormSpec(" corp ", "https://corp/mcp", " tok ")
	if s.ID != "corp" || s.URL != "https://corp/mcp" || s.Bearer != "tok" || s.Command != "" {
		t.Errorf("remote: %+v", s)
	}
	s, _ = mcpFormSpec("db", `db-mcp --stdio --root "/my dir"`, "tok")
	if s.Command != "db-mcp" || len(s.Args) != 3 || s.Args[2] != "/my dir" || s.URL != "" || s.Bearer != "" {
		t.Errorf("a command line is a stdio server and takes no bearer: %+v", s)
	}
	if _, err := mcpFormSpec("db", `db-mcp "open`, ""); err == nil {
		t.Error("an unbalanced quote must be refused")
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
		cur, _ = cur.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	tm := cur.(tuiModel)
	if tm.mcpStep != mcpViaStep || tm.mcpShell || !strings.Contains(stripANSI(tm.mcpFormPanel()), "(•) standalone") {
		t.Fatalf("step %d shell %v:\n%s", tm.mcpStep, tm.mcpShell, stripANSI(tm.mcpFormPanel()))
	}
	cur, _ = cur.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	tm = cur.(tuiModel)
	if v := stripANSI(tm.mcpFormPanel()); !tm.mcpShell || !strings.Contains(v, "(•) scripted") || !strings.Contains(v, "no docs indexing") {
		t.Fatalf("space must switch to the shell and say what it loses:\n%s", v)
	}
	cur.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
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
		cur, _ = cur.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	tm := cur.(tuiModel)
	if tm.mcpStep != mcpClient || !strings.Contains(stripANSI(tm.mcpFormPanel()), "oauth login (optional)") {
		t.Fatalf("step %d:\n%s", tm.mcpStep, stripANSI(tm.mcpFormPanel()))
	}
	cur, _ = cur.Update(tea.KeyPressMsg{Code: tea.KeyTab}) // empty client id: straight to the placement
	if tm = cur.(tuiModel); tm.mcpStep != mcpViaStep {
		t.Fatalf("an empty client id must skip the oauth rows: step %d", tm.mcpStep)
	}
	cur, _ = cur.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	tm = cur.(tuiModel)
	tm.mcpFields[mcpClient].SetValue("c1")
	tm.mcpFields[mcpScopes].SetValue("openid, email")
	tm.mcpFields[mcpSend].SetValue("jwt")
	cur = tm
	for i := 0; i < 4; i++ {
		cur, _ = cur.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	if tm = cur.(tuiModel); tm.mcpStep != mcpSend || !strings.Contains(tm.mcpErr, "access_token or id_token") {
		t.Fatalf("step %d err %q", tm.mcpStep, tm.mcpErr)
	}
	tm.mcpFields[mcpSend].SetValue("id_token")
	cur, _ = tm.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	cur.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
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

// A word that is also a key name, arriving as one burst (typed fast, sent
// over ssh, tmux, the web terminal), is text: the decoder hands it over a
// grapheme at a time.
func TestBurstOfRunes_KeyNamedWordIsTyped(t *testing.T) {
	m := newTUIModel(nil, ".")
	m.input.Focus()
	var cur tea.Model = m
	for _, k := range decodeKeys(t, "a up down b") {
		cur, _ = cur.Update(k)
	}
	tm := cur.(tuiModel)
	if got := tm.input.Value(); got != "a up down b" {
		t.Errorf("input = %q", got)
	}
}

// Shell-style splitting: quotes and backslashes, nothing expanded.
func TestSplitShell(t *testing.T) {
	for in, want := range map[string][]string{
		`npx @playwright/mcp@latest --headless`: {"npx", "@playwright/mcp@latest", "--headless"},
		`a 'b c' "d \"e\"" f\ g`:                {"a", "b c", `d "e"`, "f g"},
		`x '' "$HOME" '$HOME'`:                  {"x", "", "$HOME", "$HOME"},
		`  `:                                    nil,
	} {
		got, err := splitShell(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`a 'b`, `a "b`, `a\`} {
		if _, err := splitShell(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if w := []string{"npx", "a b", "it's"}; !slices.Equal(must(splitShell(shellJoin(w))), w) {
		t.Errorf("join round trip: %q", shellJoin(w))
	}
}

func must[T any](v T, _ error) T { return v }

// A command hides the bearer and oauth rows and asks for env (masked); a
// URL hides env. The server row says which it is.
func TestMCPForm_CommandRowsAndEnv(t *testing.T) {
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	m.openMCPForm()
	m.mcpFields[mcpID].SetValue("db")
	m.mcpFields[mcpServer].SetValue("db-mcp --stdio")
	var cur tea.Model = m
	cur, _ = cur.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	cur, _ = cur.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	tm := cur.(tuiModel)
	v := stripANSI(tm.mcpFormPanel())
	if tm.mcpStep != mcpEnv || !strings.Contains(v, "local command (stdio)") || strings.Contains(v, "bearer") || strings.Contains(v, "oauth") {
		t.Fatalf("step %d:\n%s", tm.mcpStep, v)
	}
	tm.mcpFields[mcpEnv].SetValue(`TOKEN=s3cret DSN=$DB_DSN`)
	if v := stripANSI(tm.mcpFormPanel()); strings.Contains(v, "s3cret") {
		t.Errorf("the env row must be masked:\n%s", v)
	}
	tm.mcpFields[mcpEnv].SetValue(`TOKEN='open`)
	cur, _ = tm.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if tm = cur.(tuiModel); tm.mcpStep != mcpEnv || !strings.Contains(tm.mcpErr, "unbalanced") {
		t.Fatalf("a bad env must stop the form: %d %q", tm.mcpStep, tm.mcpErr)
	}
	tm.mcpFields[mcpEnv].SetValue(`TOKEN=s3cret DSN=$DB_DSN`)
	cur, _ = tm.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	cur, _ = cur.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	var got map[string]string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("sent %q: %v", buf.String(), err)
	}
	var spec dun.ServerSpec
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got["action"], "add ")), &spec); err != nil ||
		!slices.Equal(spec.Env, []string{"TOKEN=s3cret", "DSN=$DB_DSN"}) || spec.Command != "db-mcp" {
		t.Errorf("spec %+v %v", spec, err)
	}
	if strings.Contains(convoText(cur.(tuiModel)), "s3cret") {
		t.Error("the secret reached the scrollback")
	}

	// A URL: no env row, says remote.
	m = newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	m.openMCPForm()
	m.mcpFields[mcpServer].SetValue("https://x/mcp")
	if v := stripANSI(m.mcpFormPanel()); !strings.Contains(v, "remote (HTTP)") || strings.Contains(v, "  env  ") {
		t.Errorf("url:\n%s", v)
	}
}

// A preset fills the id and server rows; nothing is sent until enter.
func TestMCPForm_Preset(t *testing.T) {
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	m.openMCPForm("playwright")
	if m.mcpFields[mcpID].Value() != "playwright" || m.mcpFields[mcpServer].Value() != "npx @playwright/mcp@latest --headless --isolated" || m.mcpStep != mcpServer {
		t.Fatalf("preset: %q %q step %d", m.mcpFields[mcpID].Value(), m.mcpFields[mcpServer].Value(), m.mcpStep)
	}
	if buf.Len() != 0 {
		t.Error("a preset must not add anything by itself")
	}
	m.openMCPForm("nope")
	if !strings.Contains(m.mcpErr, "no preset") {
		t.Errorf("unknown preset: %q", m.mcpErr)
	}
	// Typing a preset's id fills an empty server row.
	m.openMCPForm()
	m.mcpFields[mcpID].SetValue("figma")
	cur, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if tm := cur.(tuiModel); tm.mcpFields[mcpServer].Value() != "http://127.0.0.1:3845/mcp" {
		t.Errorf("typed preset id: %q", tm.mcpFields[mcpServer].Value())
	}
}
