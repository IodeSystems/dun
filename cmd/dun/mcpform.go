package main

import (
	"encoding/json"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/iodesystems/dun"
)

// /mcp add — a form for a new MCP server: an id, then a URL (a remote server
// over Streamable HTTP) or a command line (a local one over stdio), then an
// optional bearer token. The engine saves it to $DUN_HOME/mcp.json, there in
// every workspace, and starts it; the result comes back as a server event.
//
// The token is typed here rather than given as a /mcp add argument: a slash
// command's arguments are in the input history and the scrollback.

func (m *tuiModel) openMCPForm() tea.Cmd {
	mk := func(placeholder string) textinput.Model {
		t := textinput.New()
		t.Placeholder = placeholder
		t.Width = 56
		return t
	}
	m.mcpFields = [3]textinput.Model{
		mk("e.g. corp-docs"),
		mk("https://host/mcp, or a command: my-mcp --stdio"),
		mk("bearer token (empty for none; $NAME reads an env var)"),
	}
	m.mcpFields[2].EchoMode = textinput.EchoPassword
	m.mcpFields[2].EchoCharacter = '•'
	m.mcpFields[0].Focus()
	m.mcpStep, m.mcpErr, m.mcpForm = 0, "", true
	m.input.Blur()
	m.refresh()
	return textinput.Blink
}

func (m *tuiModel) closeMCPForm() {
	m.mcpForm = false
	m.input.Focus()
	m.refresh()
}

func (m tuiModel) updateMCPForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.closeMCPForm()
		return m, nil
	case "shift+tab", "up":
		if m.mcpStep > 0 {
			m.mcpFields[m.mcpStep].Blur()
			m.mcpStep--
			m.mcpFields[m.mcpStep].Focus()
		}
		m.refresh()
		return m, textinput.Blink
	case "enter", "tab", "down":
		v := strings.TrimSpace(m.mcpFields[m.mcpStep].Value())
		switch {
		case m.mcpStep == 0 && !mcpIDOK(v):
			m.mcpErr = "an id is lowercase letters, digits, - and _ — it prefixes the server's tools"
		case m.mcpStep == 1 && v == "":
			m.mcpErr = "a URL or a command"
		case m.mcpStep < 2:
			m.mcpErr = ""
			m.mcpFields[m.mcpStep].Blur()
			m.mcpStep++
			m.mcpFields[m.mcpStep].Focus()
		case msg.String() == "enter":
			spec := mcpFormSpec(m.mcpFields[0].Value(), m.mcpFields[1].Value(), m.mcpFields[2].Value())
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

func (m tuiModel) mcpFormPanel() string {
	labels := [3]string{"id    ", "server", "bearer"}
	rows := []string{stHeader.Render("add MCP server") + stDim.Render("  enter next · shift+tab back · esc cancel")}
	for i, f := range m.mcpFields {
		label := stDim.Render("  " + labels[i] + "  ")
		if i == m.mcpStep {
			label = stSel.Render("▎ " + labels[i] + "  ")
		}
		rows = append(rows, label+f.View())
	}
	if m.mcpErr != "" {
		rows = append(rows, "  "+stErr.Render(m.mcpErr))
	}
	rows = append(rows, "  "+stDim.Render("saved to "+dun.UserServersFile()+" (0600), started now and every session; the same id replaces"))
	return strings.Join(rows, "\n")
}
