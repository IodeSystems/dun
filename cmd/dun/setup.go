package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The `dun --setup` wizard as a small Bubble Tea program (consistent with the
// rest of dun): URL → key (masked) → model. The model step fetches the
// endpoint's /v1/models and lets you pick from a list (↑/↓, enter), with a
// "type a name" row for anything not listed; if the fetch fails you just type
// it. enter advances, esc/ctrl+c cancels without saving.

type setupStep int

const (
	stepURL setupStep = iota
	stepKey
	stepModel
)

type modelsMsg []string

type setupModel struct {
	step        setupStep
	url, key    textinput.Model
	custom      textinput.Model // "type a model name" field
	cur         dunConfig
	models      []string
	fetching    bool
	sel         int // model list cursor; == len(models) is the "type it" row
	spin        spinner.Model
	chosenModel string
	saved       bool
}

func newSetupModel(cur dunConfig) setupModel {
	url := textinput.New()
	url.SetValue(firstNonEmpty(cur.URL, defaultURL))
	url.Focus()
	url.Width = 48

	key := textinput.New()
	key.SetValue(cur.Key)
	key.EchoMode = textinput.EchoPassword
	key.EchoCharacter = '•'
	key.Width = 48

	custom := textinput.New()
	custom.SetValue(firstNonEmpty(cur.Model, defaultModel))
	custom.Width = 48

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return setupModel{step: stepURL, url: url, key: key, custom: custom, cur: cur, spin: sp}
}

func (m setupModel) Init() tea.Cmd { return textinput.Blink }

func (m setupModel) fetchCmd() tea.Cmd {
	url, key := m.url.Value(), m.key.Value()
	return func() tea.Msg { return modelsMsg(fetchModels(url, key)) }
}

// customRow reports whether the model step's cursor is on the "type it" row.
func (m setupModel) customRow() bool {
	return len(m.models) == 0 || m.sel >= len(m.models)
}

func (m setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit // saved stays false
		case "enter":
			return m.advance()
		case "up":
			if m.step == stepModel && !m.fetching && m.sel > 0 {
				m.sel--
				m.syncCustomFocus()
			}
			return m, nil
		case "down":
			if m.step == stepModel && !m.fetching && m.sel < len(m.models) {
				m.sel++
				m.syncCustomFocus()
			}
			return m, nil
		}
		// Route typing to the active field.
		var cmd tea.Cmd
		switch {
		case m.step == stepURL:
			m.url, cmd = m.url.Update(msg)
		case m.step == stepKey:
			m.key, cmd = m.key.Update(msg)
		case m.step == stepModel && m.customRow():
			m.custom, cmd = m.custom.Update(msg)
		}
		return m, cmd

	case modelsMsg:
		m.models = []string(msg)
		m.fetching = false
		m.sel = 0
		want := firstNonEmpty(m.cur.Model, defaultModel)
		for i, id := range m.models {
			if id == want {
				m.sel = i
			}
		}
		m.syncCustomFocus()
		return m, textinput.Blink

	case spinner.TickMsg:
		if m.fetching {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m setupModel) advance() (tea.Model, tea.Cmd) {
	switch m.step {
	case stepURL:
		m.step = stepKey
		m.url.Blur()
		m.key.Focus()
		return m, textinput.Blink
	case stepKey:
		m.step = stepModel
		m.key.Blur()
		m.fetching = true
		return m, tea.Batch(m.fetchCmd(), m.spin.Tick)
	default: // stepModel
		if m.fetching {
			return m, nil
		}
		if !m.customRow() {
			m.chosenModel = m.models[m.sel]
			m.saved = true
			return m, tea.Quit
		}
		if v := strings.TrimSpace(m.custom.Value()); v != "" {
			m.chosenModel = v
			m.saved = true
			return m, tea.Quit
		}
		return m, nil
	}
}

func (m *setupModel) syncCustomFocus() {
	if m.customRow() {
		m.custom.Focus()
	} else {
		m.custom.Blur()
	}
}

var (
	stSetupTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	stSetupLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	stSetupSel   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	stSetupTool  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
)

func (m setupModel) View() string {
	var b strings.Builder
	b.WriteString(stSetupTitle.Render("dun setup") + stSetupLabel.Render("  ·  enter: next · esc: cancel") + "\n\n")

	// URL + key rows show the value once past them; the active row shows the field.
	b.WriteString(field(m.step, stepURL, "URL", m.url.View(), firstNonEmpty(m.url.Value(), "—")))
	b.WriteString(field(m.step, stepKey, "key", m.key.View(), maskKey(m.key.Value())))

	b.WriteString(stSetupLabel.Render("  model  "))
	switch {
	case m.step != stepModel:
		b.WriteString(stSetupLabel.Render("…"))
	case m.fetching:
		b.WriteString(m.spin.View() + stSetupLabel.Render(" fetching models…"))
	default:
		b.WriteString(m.modelPicker())
	}
	b.WriteString("\n")
	return b.String()
}

func field(cur, at setupStep, label, active, done string) string {
	if cur == at {
		return stSetupSel.Render("  "+label+"  ") + active + "\n"
	}
	return stSetupLabel.Render("  "+label+"  ") + done + "\n"
}

func (m setupModel) modelPicker() string {
	var b strings.Builder
	if len(m.models) == 0 {
		return m.custom.View() + stSetupLabel.Render("  (endpoint had no model list — type it)")
	}
	b.WriteString("\n")
	for i, id := range m.models {
		if i == m.sel {
			b.WriteString("    " + stSetupSel.Render("➤ "+id) + "\n")
		} else {
			b.WriteString("      " + stSetupTool.Render(id) + "\n")
		}
	}
	cursor := "      "
	if m.customRow() {
		cursor = "    " + stSetupSel.Render("➤ ")
	}
	b.WriteString(cursor + stSetupLabel.Render("type a name: ") + m.custom.View())
	return b.String()
}

// runSetupTUI runs the wizard and saves on completion.
func runSetupTUI() error {
	final, err := tea.NewProgram(newSetupModel(loadConfig())).Run()
	if err != nil {
		return err
	}
	fm := final.(setupModel)
	if !fm.saved {
		fmt.Fprintln(os.Stderr, "setup cancelled — nothing changed")
		return nil
	}
	c := dunConfig{URL: fm.url.Value(), Model: fm.chosenModel, Key: fm.key.Value()}
	if err := saveConfig(c); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "saved → %s\n  url:   %s\n  model: %s\n  key:   %s\n",
		configPath(), c.URL, c.Model, maskKey(c.Key))
	return nil
}
