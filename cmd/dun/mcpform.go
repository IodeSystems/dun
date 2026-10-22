package main

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/iodesystems/dun"
)

// /mcp add — a form for a new MCP server: an id, then a URL (a remote server
// over Streamable HTTP) or a command line (a local one over stdio), then an
// optional bearer token, then — for a URL with no bearer — an optional OAuth
// login (client id, scopes, issuer, which token to send), then its
// placement: direct (the default) or in the shell (dun.ViaShell). The engine
// saves it to $DUN_HOME/mcp.json, there in every workspace, and starts it;
// the result comes back as a server event.
//
// The OAuth rows are the common ones. The rest of the oauth block
// (client_secret, audience, resource, redirect_port, auth_params) is set in
// mcp.json by hand.
//
// The token is typed here rather than given as a /mcp add argument: a slash
// command's arguments are in the input history and the scrollback.

// The form's rows. The oauth rows are shown only for a URL with no bearer.
const (
	mcpID = iota
	mcpServer
	mcpBearer
	mcpClient
	mcpScopes
	mcpIssuer
	mcpSend
	mcpViaStep // the last row: direct or in the shell
)

func (m *tuiModel) openMCPForm() tea.Cmd {
	mk := func(placeholder string) textinput.Model {
		t := textinput.New()
		t.Placeholder = placeholder
		t.Width = 56
		return t
	}
	m.mcpFields = [mcpViaStep]textinput.Model{
		mk("e.g. corp-docs"),
		mk("https://host/mcp, or a command: my-mcp --stdio"),
		mk("bearer token (empty for none; $NAME reads an env var)"),
		mk("oauth client id (empty: no login)"),
		mk("scopes, space-separated (empty: what the server asks for)"),
		mk("issuer (empty: the server names it)"),
		mk("access_token (default) or id_token"),
	}
	m.mcpFields[mcpBearer].EchoMode = textinput.EchoPassword
	m.mcpFields[mcpBearer].EchoCharacter = '•'
	m.mcpFields[mcpID].Focus()
	m.mcpStep, m.mcpErr, m.mcpForm, m.mcpShell = 0, "", true, false
	m.input.Blur()
	m.refresh()
	return textinput.Blink
}

func (m *tuiModel) closeMCPForm() {
	m.mcpForm = false
	m.input.Focus()
	m.refresh()
}

// mcpOAuthRows reports whether the oauth rows apply: a URL with no bearer.
func (m *tuiModel) mcpOAuthRows() bool {
	t := strings.TrimSpace(m.mcpFields[mcpServer].Value())
	return (strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://")) &&
		strings.TrimSpace(m.mcpFields[mcpBearer].Value()) == ""
}

// mcpNext is the row after step: the oauth rows are skipped when they do not
// apply, and after an empty client id.
func (m *tuiModel) mcpNext(step int) int {
	next := step + 1
	if next >= mcpClient && next < mcpViaStep &&
		(!m.mcpOAuthRows() || strings.TrimSpace(m.mcpFields[mcpClient].Value()) == "" && step >= mcpClient) {
		return mcpViaStep
	}
	return next
}

// mcpPrev is the row before step, skipping oauth rows that do not apply.
func (m *tuiModel) mcpPrev(step int) int {
	prev := step - 1
	if step == mcpViaStep && !m.mcpOAuthRows() {
		return mcpBearer
	}
	if step == mcpViaStep && strings.TrimSpace(m.mcpFields[mcpClient].Value()) == "" {
		return mcpClient
	}
	return prev
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
		switch {
		case m.mcpStep == mcpID && !mcpIDOK(v):
			m.mcpErr = "an id is lowercase letters, digits, - and _ — it prefixes the server's tools"
		case m.mcpStep == mcpServer && v == "":
			m.mcpErr = "a URL or a command"
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
			spec := mcpFormSpec(m.mcpFields[mcpID].Value(), m.mcpFields[mcpServer].Value(), m.mcpFields[mcpBearer].Value())
			if m.mcpOAuthRows() {
				spec.OAuth = mcpFormOAuth(m.mcpFields[mcpClient].Value(), m.mcpFields[mcpScopes].Value(),
					m.mcpFields[mcpIssuer].Value(), m.mcpFields[mcpSend].Value())
			}
			if m.mcpShell {
				if spec.OAuth != nil {
					m.mcpErr = "an oauth server cannot run in the shell — dun holds its token"
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
		return m, nil // the placement row takes no text
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
// remote server, anything else a command line split on spaces.
func mcpFormSpec(id, target, bearer string) dun.ServerSpec {
	spec := dun.ServerSpec{ID: strings.TrimSpace(id), Bearer: strings.TrimSpace(bearer)}
	target = strings.TrimSpace(target)
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		spec.URL = target
	} else if f := strings.Fields(target); len(f) > 0 {
		spec.Command, spec.Args = f[0], f[1:]
		spec.Bearer = "" // a stdio server takes its credentials in its env or args
	}
	return spec
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
	labels := [mcpViaStep]string{"id    ", "server", "bearer", "client", "scopes", "issuer", "send  "}
	rows := []string{stHeader.Render("add MCP server") + stDim.Render("  enter next · shift+tab back · esc cancel")}
	oauth := m.mcpOAuthRows()
	for i, f := range m.mcpFields {
		if i == mcpClient && oauth {
			rows = append(rows, "  "+stDim.Render("oauth login (optional): a client registered with the identity provider, redirect URI"),
				"  "+stDim.Render("http://127.0.0.1:"+strconv.Itoa(dun.DefaultOAuthRedirectPort)+"/oauth/callback; secret, audience, resource and the rest go in mcp.json"))
		}
		if i >= mcpClient && !oauth {
			break
		}
		label := stDim.Render("  " + labels[i] + "  ")
		if i == m.mcpStep {
			label = stSel.Render("▎ " + labels[i] + "  ")
		}
		rows = append(rows, label+f.View())
	}
	label := stDim.Render("  place   ")
	if m.mcpStep == mcpViaStep {
		label = stSel.Render("▎ place   ")
	}
	direct, shell := "( ) direct", "( ) in the shell"
	if m.mcpShell {
		shell = "(•) in the shell"
	} else {
		direct = "(•) direct"
	}
	rows = append(rows, label+direct+"   "+shell+stDim.Render("   space switches"))
	if m.mcpShell {
		rows = append(rows, "  "+stDim.Render("in the shell: one eval namespace, no tool schemas in every request; but no docs indexing,"),
			"  "+stDim.Render("no prompts or skills, no tool deny-list, and its calls show as eval"))
	} else {
		rows = append(rows, "  "+stDim.Render("direct: each tool's schema rides every request; docs, prompts, skills and renderers work"))
	}
	if m.mcpErr != "" {
		rows = append(rows, "  "+stErr.Render(m.mcpErr))
	}
	rows = append(rows, "  "+stDim.Render("saved to "+dun.UserServersFile()+" (0600), started now and every session; the same id replaces"))
	return strings.Join(rows, "\n")
}
