package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun"
)

// /mcp add [preset] — a form for a new MCP server: a name (its id), then
// where it is — a URL (a remote server over Streamable HTTP) or a command
// line (a local one over stdio, split with shell-style quoting). Those two
// are all it needs, and enter on the server row (or any row after it) adds
// it. The optional rows follow, reached with tab: for a URL, a bearer token
// and, with no token, an OAuth login (client id, scopes, issuer, which token
// to send); for a command, environment variables. The engine saves it to
// $DUN_HOME/mcp.json, there in every workspace, and starts it standalone;
// the result comes back as a server event.
//
// Scripted (hosted in mcpshell) is not asked here: it is a later choice —
// /mcp mode <id> scripted, or m in /mcp — and a first server's user cannot
// answer it.
//
// A preset (dun.ServerPresets) fills the name and the server row; typing a
// preset's name and leaving the row does the same when the server row is
// empty.
//
// The OAuth rows are the common ones. The rest of the oauth block
// (client_secret, audience, resource, redirect_port, auth_params) is set in
// mcp.json by hand. A command has no working-directory row: ServerSpec has
// no such field.
//
// The token and the env are typed here rather than given as /mcp add
// arguments, and masked: a slash command's arguments are in the input
// history and the scrollback.

// The form's rows. Which apply depends on the server row (mcpRowShown).
const (
	mcpID = iota
	mcpServer
	mcpBearer
	mcpEnv
	mcpClient
	mcpScopes
	mcpIssuer
	mcpSend
	mcpRows // how many
)

func (m *tuiModel) openMCPForm(preset ...string) tea.Cmd {
	mk := func(placeholder string) textinput.Model {
		t := textinput.New()
		t.Placeholder = placeholder
		t.SetWidth(formFieldW(m.w))
		return t
	}
	// Each placeholder fits a phone's field (formFieldW at 49 columns): the
	// label says what the field is, the placeholder only what to put in it.
	m.mcpFields = [mcpRows]textinput.Model{
		mk("e.g. notes"),
		mk("command or address you were given"),
		mk("if you were given one"),
		mk("NAME=value, if it needs any"),
		mk("oauth client id (empty: none)"),
		mk("scopes (empty: what it asks for)"),
		mk("issuer (empty: the server's)"),
		mk("access_token or id_token"),
	}
	for _, i := range []int{mcpBearer, mcpEnv} {
		m.mcpFields[i].EchoMode = textinput.EchoPassword
		m.mcpFields[i].EchoCharacter = '•'
	}
	m.mcpStep, m.mcpErr, m.mcpForm = 0, "", true
	if len(preset) > 0 && preset[0] != "" {
		if !m.fillPreset(preset[0]) {
			m.mcpErr = "no preset " + strconv.Quote(preset[0]) + " — presets: " + presetIDs()
		} else {
			m.mcpStep = mcpServer
		}
	}
	m.mcpFields[m.mcpStep].Focus()
	m.input.Blur()
	m.refresh()
	return textinput.Blink
}

// presetIDs lists the presets for a hint.
func presetIDs() string {
	ids := make([]string, len(dun.ServerPresets))
	for i, p := range dun.ServerPresets {
		ids[i] = p.ID
	}
	return strings.Join(ids, ", ")
}

// fillPreset fills the id and server rows from a preset.
func (m *tuiModel) fillPreset(id string) bool {
	p, ok := dun.Preset(id)
	if !ok {
		return false
	}
	m.mcpFields[mcpID].SetValue(p.ID)
	target := p.URL
	if target == "" {
		target = shellJoin(append([]string{p.Command}, p.Args...))
	}
	m.mcpFields[mcpServer].SetValue(target)
	return true
}

func (m *tuiModel) closeMCPForm() {
	m.mcpForm = false
	m.input.Focus()
	m.refresh()
}

// mcpIsURL reports whether the server row is a URL; mcpIsCommand whether it
// is a command line.
func (m *tuiModel) mcpIsURL() bool {
	t := strings.TrimSpace(m.mcpFields[mcpServer].Value())
	return strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://")
}

func (m *tuiModel) mcpIsCommand() bool {
	return !m.mcpIsURL() && strings.TrimSpace(m.mcpFields[mcpServer].Value()) != ""
}

// mcpOAuthRows reports whether the oauth rows apply: a URL with no bearer.
func (m *tuiModel) mcpOAuthRows() bool {
	return m.mcpIsURL() && strings.TrimSpace(m.mcpFields[mcpBearer].Value()) == ""
}

// mcpRowShown reports whether a row applies to what the form holds now.
func (m *tuiModel) mcpRowShown(step int) bool {
	switch step {
	case mcpBearer:
		return m.mcpIsURL()
	case mcpEnv:
		return m.mcpIsCommand()
	case mcpClient:
		return m.mcpOAuthRows()
	case mcpScopes, mcpIssuer, mcpSend:
		return m.mcpOAuthRows() && strings.TrimSpace(m.mcpFields[mcpClient].Value()) != ""
	}
	return true
}

// mcpNext is the next row that applies after step, or step when none does.
func (m *tuiModel) mcpNext(step int) int {
	for next := step + 1; next < mcpRows; next++ {
		if m.mcpRowShown(next) {
			return next
		}
	}
	return step
}

// mcpPrev is the row before step that applies.
func (m *tuiModel) mcpPrev(step int) int {
	for prev := step - 1; prev > 0; prev-- {
		if m.mcpRowShown(prev) {
			return prev
		}
	}
	return 0
}

// mcpRowErr checks the row the cursor is leaving; "" when it is fine.
func (m *tuiModel) mcpRowErr() string {
	v := strings.TrimSpace(m.mcpFields[m.mcpStep].Value())
	switch m.mcpStep {
	case mcpID:
		if !mcpIDOK(v) {
			return "a name is lowercase letters, digits, - and _ — it prefixes the server's tools"
		}
	case mcpServer:
		if v == "" {
			return "a command (npx some-mcp --stdio) or a web address (https://…)"
		}
		if m.mcpIsCommand() {
			if _, err := splitShell(v); err != nil {
				return "command: " + err.Error()
			}
		}
	case mcpEnv:
		if _, err := parseEnv(v); err != nil {
			return "env: " + err.Error()
		}
	case mcpSend:
		if v != "" && v != dun.SendAccessToken && v != dun.SendIDToken {
			return "send is access_token or id_token"
		}
	}
	return ""
}

func (m tuiModel) updateMCPForm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.closeMCPForm()
		return m, nil
	case "shift+tab", "up":
		if m.mcpStep > 0 {
			m.mcpFields[m.mcpStep].Blur()
			m.mcpStep = m.mcpPrev(m.mcpStep)
			m.mcpFields[m.mcpStep].Focus()
		}
		m.refresh()
		return m, textinput.Blink
	case "enter", "tab", "down":
		// A preset's name with the server row empty fills it in.
		if m.mcpStep == mcpID && strings.TrimSpace(m.mcpFields[mcpServer].Value()) == "" {
			m.fillPreset(strings.TrimSpace(m.mcpFields[mcpID].Value()))
		}
		if m.mcpErr = m.mcpRowErr(); m.mcpErr != "" {
			m.refresh()
			return m, textinput.Blink
		}
		// enter adds once the name and the server are in; tab goes on to
		// the optional rows.
		if msg.String() == "enter" && m.mcpStep >= mcpServer {
			if m.addMCPServer() {
				return m, nil
			}
			m.refresh()
			return m, textinput.Blink
		}
		m.mcpFields[m.mcpStep].Blur()
		m.mcpStep = m.mcpNext(m.mcpStep)
		m.mcpFields[m.mcpStep].Focus()
		m.refresh()
		return m, textinput.Blink
	}
	return m.typeMCPField(msg)
}

// addMCPServer sends the form's server to the engine and closes the form;
// false, with mcpErr set, when it cannot.
func (m *tuiModel) addMCPServer() bool {
	spec, err := mcpFormSpec(m.mcpFields[mcpID].Value(), m.mcpFields[mcpServer].Value(), m.mcpFields[mcpBearer].Value())
	if err != nil {
		m.mcpErr = "command: " + err.Error()
		return false
	}
	if spec.Command != "" {
		if spec.Env, err = parseEnv(m.mcpFields[mcpEnv].Value()); err != nil {
			m.mcpErr = "env: " + err.Error()
			return false
		}
	}
	if m.mcpOAuthRows() {
		spec.OAuth = mcpFormOAuth(m.mcpFields[mcpClient].Value(), m.mcpFields[mcpScopes].Value(),
			m.mcpFields[mcpIssuer].Value(), m.mcpFields[mcpSend].Value())
	}
	b, _ := json.Marshal(spec)
	if !m.proc.controlCmd("mcp", "add "+string(b)) {
		m.mcpErr = "no engine right now — /reconnect first"
		return false
	}
	m.closeMCPForm()
	// The result replaces this line (noteMCPAdded): progress, then news,
	// in one place.
	m.appendNotice(convoEntry{collapsed: stDim.Render("adding " + spec.ID + "…"), adding: spec.ID})
	m.refresh()
	return true
}

// typeMCPField is a key or a paste in the add-server form's field.
func (m tuiModel) typeMCPField(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.mcpFields[m.mcpStep], cmd = m.mcpFields[m.mcpStep].Update(msg)
	m.refresh()
	return m, cmd
}

func mcpIDOK(id string) bool {
	if id == "" || len(id) > 32 || id[0] == '-' || id[0] == '_' {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// mcpFormSpec turns the form into a server spec: an http(s) address is a
// remote server, anything else a command line split with shell quoting.
func mcpFormSpec(id, target, bearer string) (dun.ServerSpec, error) {
	spec := dun.ServerSpec{ID: strings.TrimSpace(id), Bearer: strings.TrimSpace(bearer)}
	target = strings.TrimSpace(target)
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		spec.URL = target
		return spec, nil
	}
	f, err := splitShell(target)
	if err != nil {
		return spec, err
	}
	if len(f) > 0 {
		spec.Command, spec.Args = f[0], f[1:]
	}
	spec.Bearer = "" // a stdio server takes its credentials in its env or args
	return spec, nil
}

// splitShell is dun.SplitShell.
func splitShell(s string) ([]string, error) { return dun.SplitShell(s) }

// shellJoin is splitShell's inverse, quoting a word only when it needs it.
func shellJoin(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		if w != "" && !strings.ContainsAny(w, " \t\n'\"\\$`") {
			out[i] = w
			continue
		}
		out[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

// parseEnv reads the env row: NAME=value words, shell-quoted.
func parseEnv(s string) ([]string, error) {
	words, err := splitShell(s)
	if err != nil {
		return nil, err
	}
	for _, w := range words {
		k, _, ok := strings.Cut(w, "=")
		if !ok || !envName(k) {
			return nil, fmt.Errorf("%q is not NAME=value", w)
		}
	}
	if len(words) == 0 {
		return nil, nil
	}
	return words, nil
}

func envName(k string) bool {
	if k == "" || k[0] >= '0' && k[0] <= '9' {
		return false
	}
	for _, r := range k {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// mcpFormOAuth is the oauth block from the form's rows; nil without a client
// id. Scopes split on spaces or commas.
func mcpFormOAuth(clientID, scopes, issuer, send string) *dun.OAuthSpec {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return nil
	}
	o := &dun.OAuthSpec{ClientID: clientID, Issuer: strings.TrimSpace(issuer), Send: strings.TrimSpace(send)}
	o.Scopes = strings.FieldsFunc(scopes, func(r rune) bool { return r == ' ' || r == ',' })
	if len(o.Scopes) == 0 {
		o.Scopes = nil
	}
	return o
}

func (m tuiModel) mcpFormPanel() string {
	labels := [mcpRows]string{"name", "server", "token", "env", "client", "scopes", "issuer", "send"}
	rows := []string{stHeader.Render("add MCP server")}
	for i, f := range m.mcpFields {
		if !m.mcpRowShown(i) {
			continue
		}
		if i == mcpClient {
			rows = append(rows, "  "+stDim.Render("oauth login (optional): a client id registered with the identity provider, redirect URI"),
				"  "+stDim.Render("http://127.0.0.1:"+strconv.Itoa(dun.DefaultOAuthRedirectPort)+"/oauth/callback; secret, audience, resource and the rest go in mcp.json"))
		}
		row := formRow(labels[i], i == m.mcpStep, f.View())
		if i == mcpServer {
			switch {
			case m.mcpIsURL():
				row += stDim.Render("  a web address")
			case m.mcpIsCommand():
				row += stDim.Render("  runs as a command")
			}
		}
		rows = append(rows, row)
	}
	if m.mcpErr != "" {
		rows = append(rows, "  "+stErr.Render(m.mcpErr))
	}
	rows = formNotes(rows, "saved to "+tildePath(dun.UserServersFile())+" for every project", "starts now · "+sameNameNote)
	return strings.Join(rows, "\n")
}

// mcpHint is the form's status line: enter adds once the server row is
// reached, tab goes on to the optional rows when there are any.
func (m tuiModel) mcpHint() string {
	enter := "enter next"
	if m.mcpStep >= mcpServer {
		enter = "enter add"
	}
	var extra []string
	if m.mcpStep >= mcpServer && m.mcpNext(m.mcpStep) != m.mcpStep {
		extra = append(extra, "tab more options")
	}
	return formKeys("add server", enter, extra...)
}
