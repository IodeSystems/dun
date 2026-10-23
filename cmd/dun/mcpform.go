package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/iodesystems/dun"
)

// /mcp add [preset] — a form for a new MCP server: an id, then where it is —
// a URL (a remote server over Streamable HTTP) or a command line (a local
// one over stdio, split with shell-style quoting) — then, for a URL, an
// optional bearer token and, with no bearer, an optional OAuth login (client
// id, scopes, issuer, which token to send); for a command, optional
// environment variables. Last, its mode: standalone (the default) or
// scripted (hosted in mcpshell, dun.ViaShell). The engine saves it to
// $DUN_HOME/mcp.json, there in every workspace, and starts it; the result
// comes back as a server event.
//
// A preset (dun.ServerPresets) fills the id and the server row; typing a
// preset's id and leaving the row does the same when the server row is
// empty. Nothing is added until enter on the last row.
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
	mcpViaStep // the last row: standalone or scripted
)

func (m *tuiModel) openMCPForm(preset ...string) tea.Cmd {
	mk := func(placeholder string) textinput.Model {
		t := textinput.New()
		t.Placeholder = placeholder
		t.Width = 56
		return t
	}
	m.mcpFields = [mcpViaStep]textinput.Model{
		mk("e.g. corp-docs, or a preset: " + presetIDs()),
		mk("URL (https://…) or command (npx some-mcp --stdio)"),
		mk("bearer token (empty for none; $NAME reads an env var)"),
		mk("NAME=value NAME2=$VAR (empty for none; $VAR is read at start)"),
		mk("oauth client id (empty: no login)"),
		mk("scopes, space-separated (empty: what the server asks for)"),
		mk("issuer (empty: the server names it)"),
		mk("access_token (default) or id_token"),
	}
	for _, i := range []int{mcpBearer, mcpEnv} {
		m.mcpFields[i].EchoMode = textinput.EchoPassword
		m.mcpFields[i].EchoCharacter = '•'
	}
	m.mcpStep, m.mcpErr, m.mcpForm, m.mcpShell = 0, "", true, false
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

// mcpNext is the next row that applies after step.
func (m *tuiModel) mcpNext(step int) int {
	for next := step + 1; next < mcpViaStep; next++ {
		if m.mcpRowShown(next) {
			return next
		}
	}
	return mcpViaStep
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

func (m tuiModel) updateMCPForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.closeMCPForm()
		return m, nil
	case "shift+tab", "up":
		if m.mcpStep > 0 {
			if m.mcpStep < len(m.mcpFields) {
				m.mcpFields[m.mcpStep].Blur()
			}
			m.mcpStep = m.mcpPrev(m.mcpStep)
			m.mcpFields[m.mcpStep].Focus()
		}
		m.refresh()
		return m, textinput.Blink
	case " ", "left", "right", "h", "l":
		if m.mcpStep == mcpViaStep {
			m.mcpShell = !m.mcpShell
			m.refresh()
			return m, nil
		}
	case "enter", "tab", "down":
		v := ""
		if m.mcpStep < len(m.mcpFields) {
			v = strings.TrimSpace(m.mcpFields[m.mcpStep].Value())
		}
		// A preset's id with the server row empty fills it in.
		if m.mcpStep == mcpID && strings.TrimSpace(m.mcpFields[mcpServer].Value()) == "" {
			m.fillPreset(v)
		}
		var cmdErr, envErr error
		if m.mcpStep == mcpServer && m.mcpIsCommand() {
			_, cmdErr = splitShell(v)
		}
		if m.mcpStep == mcpEnv {
			_, envErr = parseEnv(v)
		}
		switch {
		case m.mcpStep == mcpID && !mcpIDOK(v):
			m.mcpErr = "an id is lowercase letters, digits, - and _ — it prefixes the server's tools"
		case m.mcpStep == mcpServer && v == "":
			m.mcpErr = "a URL (https://…) or a command (npx some-mcp --stdio)"
		case cmdErr != nil:
			m.mcpErr = "command: " + cmdErr.Error()
		case envErr != nil:
			m.mcpErr = "env: " + envErr.Error()
		case m.mcpStep == mcpSend && v != "" && v != dun.SendAccessToken && v != dun.SendIDToken:
			m.mcpErr = "send is access_token or id_token"
		case m.mcpStep < mcpViaStep:
			m.mcpErr = ""
			m.mcpFields[m.mcpStep].Blur()
			m.mcpStep = m.mcpNext(m.mcpStep)
			if m.mcpStep < len(m.mcpFields) {
				m.mcpFields[m.mcpStep].Focus()
			}
		case msg.String() == "enter":
			spec, err := mcpFormSpec(m.mcpFields[mcpID].Value(), m.mcpFields[mcpServer].Value(), m.mcpFields[mcpBearer].Value())
			if err != nil {
				m.mcpErr = "command: " + err.Error()
				break
			}
			if spec.Command != "" {
				if spec.Env, err = parseEnv(m.mcpFields[mcpEnv].Value()); err != nil {
					m.mcpErr = "env: " + err.Error()
					break
				}
			}
			if m.mcpOAuthRows() {
				spec.OAuth = mcpFormOAuth(m.mcpFields[mcpClient].Value(), m.mcpFields[mcpScopes].Value(),
					m.mcpFields[mcpIssuer].Value(), m.mcpFields[mcpSend].Value())
			}
			if m.mcpShell {
				if spec.OAuth != nil {
					m.mcpErr = "an oauth server cannot be scripted — dun holds its token"
					break
				}
				spec.Via = dun.ViaShell
			}
			b, _ := json.Marshal(spec)
			if !m.proc.controlCmd("mcp", "add "+string(b)) {
				m.mcpErr = "no engine right now — /reconnect first"
				break
			}
			m.closeMCPForm()
			m.append(stDim.Render("adding " + spec.ID + "…"))
			return m, nil
		}
		m.refresh()
		return m, textinput.Blink
	}
	if m.mcpStep >= len(m.mcpFields) {
		return m, nil // the mode row takes no text
	}
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
	labels := [mcpViaStep]string{"id    ", "server", "bearer", "env   ", "client", "scopes", "issuer", "send  "}
	rows := []string{stHeader.Render("add MCP server") + stDim.Render("  enter next · shift+tab back · esc cancel"),
		"  " + stDim.Render("presets (type one as the id, or /mcp add <preset>): "+presetIDs())}
	for i, f := range m.mcpFields {
		if !m.mcpRowShown(i) {
			continue
		}
		if i == mcpClient {
			rows = append(rows, "  "+stDim.Render("oauth login (optional): a client registered with the identity provider, redirect URI"),
				"  "+stDim.Render("http://127.0.0.1:"+strconv.Itoa(dun.DefaultOAuthRedirectPort)+"/oauth/callback; secret, audience, resource and the rest go in mcp.json"))
		}
		label := stDim.Render("  " + labels[i] + "  ")
		if i == m.mcpStep {
			label = stSel.Render("▎ " + labels[i] + "  ")
		}
		row := label + f.View()
		if i == mcpServer {
			switch {
			case m.mcpIsURL():
				row += stDim.Render("  remote (HTTP)")
			case m.mcpIsCommand():
				row += stDim.Render("  local command (stdio)")
			}
		}
		rows = append(rows, row)
	}
	label := stDim.Render("  mode    ")
	if m.mcpStep == mcpViaStep {
		label = stSel.Render("▎ mode    ")
	}
	direct, shell := "( ) standalone", "( ) scripted"
	if m.mcpShell {
		shell = "(•) scripted"
	} else {
		direct = "(•) standalone"
	}
	rows = append(rows, label+direct+"   "+shell+stDim.Render("   space switches"))
	if m.mcpShell {
		rows = append(rows, "  "+stDim.Render("scripted: hosted in mcpshell, the model calls it through eval — no tool schemas in every request;"),
			"  "+stDim.Render("but no docs indexing, prompts, skills or tool switches, and its calls show as eval"))
	} else {
		rows = append(rows, "  "+stDim.Render("standalone: its own tools, each schema in every request; docs, prompts, skills and renderers work"))
	}
	if m.mcpErr != "" {
		rows = append(rows, "  "+stErr.Render(m.mcpErr))
	}
	rows = append(rows, "  "+stDim.Render("saved to "+dun.UserServersFile()+" (0600), on globally and started now; the same id replaces"))
	return strings.Join(rows, "\n")
}
