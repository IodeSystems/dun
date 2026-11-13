package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/dunconfig"
)

// The roles window (/model): every role (dunconfig.FacetNames) with its
// model and where it came from. It is the model picker's first screen; enter
// opens the picker for the highlighted role (pickFacet).

// openRoles opens the roles window with the cursor on row sel.
func (m *tuiModel) openRoles(sel int) {
	m.rolesPicking, m.modelPicking, m.loginPicking, m.providerForm = true, true, false, false
	m.pickFacet = ""
	m.rolesSel = sel
	m.input.Blur()
	m.refresh()
}

// updateRoles owns the keys while the roles window is open.
func (m tuiModel) updateRoles(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k := msg.String(); k {
	case "esc", "ctrl+c":
		m.closeModelPicker()
	case "up", "ctrl+p":
		m.rolesSel = max(0, m.rolesSel-1)
		m.refresh()
	case "down", "ctrl+n":
		m.rolesSel = min(len(dunconfig.FacetNames)-1, m.rolesSel+1)
		m.refresh()
	}
	return m, nil
}

// rolesPanel renders the roles window in the lower pane.
func (m tuiModel) rolesPanel() string {
	return strings.Join([]string{stHeader.Render("models by role")}, "\n")
}

// rolesHint is the roles window's status line.
func rolesHint() string {
	return "roles  ·  ↑/↓ move · esc close"
}
