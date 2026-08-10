package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// modelFetchMsg carries the list of model ids from the API.
type modelFetchMsg []string

// modelSlash implements /model: bare opens the picker, an argument switches
// straight to that model (session-only).
func modelSlash(m *tuiModel, args []string) tea.Cmd {
	if m.replaying {
		m.append(stDim.Render("this is a replay — there is no engine to resume with"))
		return nil
	}
	// /model <name>: switch immediately (session-only, no persist).
	if len(args) > 0 {
		return m.switchModel(args[0], false)
	}
	// Bare /model: open the picker.
	m.modelPicking, m.modelSel, m.modelPersist = true, 0, false
	m.modelFetching = true
	m.input.Blur()
	m.refresh()
	// Fetch models from the API.
	key := ""
	if m.keySet {
		// We need the actual key, not just a flag. Read from config.
		cfg := loadConfig()
		key = cfg.Key
	}
	return func() tea.Msg {
		models := fetchModels(m.url, key)
		return modelFetchMsg(models)
	}
}

// updateModelPicking owns the keys while the model list is open.
func (m tuiModel) updateModelPicking(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c", "q":
		m.modelPicking = false
		m.input.Focus()
		m.refresh()
		return m, nil
	case "up", "k":
		if m.modelSel > 0 {
			m.modelSel--
		}
		m.refresh()
		return m, nil
	case "down", "j":
		if m.modelSel < len(m.modelList)-1 {
			m.modelSel++
		}
		m.refresh()
		return m, nil
	case " ":
		// Toggle persist checkbox.
		m.modelPersist = !m.modelPersist
		m.refresh()
		return m, nil
	case "enter":
		if len(m.modelList) == 0 || m.modelSel < 0 || m.modelSel >= len(m.modelList) {
			m.modelPicking = false
			m.input.Focus()
			m.refresh()
			return m, nil
		}
		chosen := m.modelList[m.modelSel]
		m.modelPicking = false
		m.input.Focus()
		return m, m.switchModel(chosen, m.modelPersist)
	}
	return m, nil
}

// switchModel restarts the engine with a new model.
// If persist is true, the new model is saved to config.json.
func (m *tuiModel) switchModel(model string, persist bool) tea.Cmd {
	if model == "" {
		return nil
	}
	if model == m.model && !persist {
		m.append(stDim.Render("already using model " + model))
		m.refresh()
		return nil
	}
	// Save to config if persisting.
	if persist {
		cfg := loadConfig()
		cfg.Model = model
		if err := saveConfig(cfg); err != nil {
			m.append(stErr.Render("failed to save config: " + err.Error()))
			return nil
		}
	}

	desc := "this session"
	if persist {
		desc = "persistently"
	}
	m.append(stDim.Render(fmt.Sprintf("switching model to %s %s…", model, desc)))

	// Restart the engine with the new model.
	m.proc.close()
	m.convo = nil
	m.cur, m.pendingTool, m.pendingArgs = "", -1, nil
	m.sel, m.blockH = -1, nil
	m.busy, m.asking, m.starting = false, false, true
	m.startingStart = time.Now()
	m.fatalErr, m.exitAnnounced = "", false
	m.restarts, m.restartStart = 0, time.Now()
	m.model = model
	m.skipHistory = false
	m.refresh()

	o := m.opts
	o.model = model
	o.resume, o.cont = m.sessionID, false
	return restartEngine(o, m.sessionID, true)
}

// modelPanel renders the model picker in the lower pane.
func (m tuiModel) modelPanel() string {
	if m.modelFetching {
		return stHeader.Render("models") + stDim.Render("  fetching…")
	}
	if len(m.modelList) == 0 {
		return stHeader.Render("models") + "\n  " + stDim.Render("could not fetch model list — check your URL and key")
	}

	rows := make([]string, 0, len(m.modelList)+2)
	persistLabel := " "
	if m.modelPersist {
		persistLabel = "✓"
	}
	rows = append(rows, stHeader.Render("models")+stDim.Render(fmt.Sprintf("  ↑/↓ choose · enter switch · space persist%s · esc cancel", persistLabel)))

	// Show a window around the selection for long lists.
	const window = 8
	lo := m.modelSel - window/2
	if lo < 0 {
		lo = 0
	}
	hi := lo + window
	if hi > len(m.modelList) {
		hi = len(m.modelList)
		if lo = hi - window; lo < 0 {
			lo = 0
		}
	}

	for i := lo; i < hi; i++ {
		id := m.modelList[i]
		mark := "  "
		if id == m.model {
			mark = stTool.Render("• ") // current model
		}
		line := mark + id
		if i == m.modelSel {
			rows = append(rows, addGutter(line, "▎ ", stSel))
		} else {
			rows = append(rows, addGutter(line, "  ", lipgloss.NewStyle()))
		}
	}

	if lo > 0 || hi < len(m.modelList) {
		rows = append(rows, stDim.Render(fmt.Sprintf("     %d-%d of %d", lo+1, hi, len(m.modelList))))
	}

	persistLine := fmt.Sprintf("  [%s] save to config (future sessions)", persistLabel)
	rows = append(rows, persistLine)

	return strings.Join(rows, "\n")
}
