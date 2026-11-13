package tui

import (
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/dunconfig"
)

// The roles window (/model): every role (dunconfig.FacetNames) with its
// model and where it came from. It is the model picker's first screen; enter
// opens the picker for the highlighted role (pickFacet).

// rolePurpose is what a role does and what it does when it has no model.
var rolePurpose = map[string]string{
	dunconfig.FacetMain:     "answers you in the conversation",
	dunconfig.FacetChild:    "the sub-agents main starts; with no model, main's",
	dunconfig.FacetVision:   "reads images and does OCR; off: tesseract if it is installed",
	dunconfig.FacetEmbed:    "turns text into vectors for search; off: search by text only",
	dunconfig.FacetSegment:  "splits documents into parts; off: the vision model, else rules",
	dunconfig.FacetIdentity: "captions and tags what is indexed; off: no captions or tags",
}

// openRoles opens the roles window with the cursor on row sel.
func (m *tuiModel) openRoles(sel int) {
	m.rolesPicking, m.modelPicking, m.loginPicking, m.providerForm = true, true, false, false
	m.pickFacet = ""
	m.rolesSel = max(0, sel)
	m.input.Blur()
	m.refresh()
}

// resolvedRoles is every role as it is served: the config's resolution with
// this client's flags (dunconfig.Resolve), main as the engine runs it when
// a session-only pick or a restart moved it. Keys are never in it.
func (m tuiModel) resolvedRoles(cfg dunconfig.DunConfig) []dunconfig.Resolved {
	out := cfg.ResolveAll(m.opts.Flags, os.Getenv)
	for i, r := range out {
		if r.Facet == dunconfig.FacetMain && m.model != "" &&
			(r.Model != m.model || r.URL != strings.TrimRight(m.url, "/")) {
			r = dunconfig.Resolved{Facet: r.Facet, Provider: m.provider, URL: m.url, Model: m.model, Effort: r.Effort, Source: dunconfig.SourceSession}
			if r.Provider == "" {
				r.Provider = dunconfig.HostName(m.url)
			}
			out[i] = r
		}
	}
	return out
}

// updateRoles owns the keys while the roles window is open.
func (m tuiModel) updateRoles(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	facet := dunconfig.FacetNames[min(m.rolesSel, len(dunconfig.FacetNames)-1)]
	switch msg.String() {
	case "esc", "ctrl+c":
		m.closeModelPicker()
	case "up", "ctrl+p":
		m.rolesSel = max(0, m.rolesSel-1)
		m.refresh()
	case "down", "ctrl+n":
		m.rolesSel = min(len(dunconfig.FacetNames)-1, m.rolesSel+1)
		m.refresh()
	case "enter":
		return m, m.openRolePicker(facet)
	case "x":
		m.resetRole(facet)
	case "p":
		// The probe says its answer in the conversation: the window closes.
		m.closeModelPicker()
		return m, engineSlash("model")(&m, []string{"probe", facet})
	}
	return m, nil
}

// openRolePicker opens the model picker for one role.
func (m *tuiModel) openRolePicker(facet string) tea.Cmd {
	m.pickFacet = facet
	return m.openModelPicker("")
}

// resetRole removes facets.<facet>: the role is what it falls back to.
func (m *tuiModel) resetRole(facet string) {
	cfg := dunconfig.LoadConfig()
	if _, ok := cfg.Facets[facet]; !ok {
		m.append(StDim.Render("facets." + facet + " is not set"))
		return
	}
	cfg.SetFacet(facet, dunconfig.Facet{})
	if err := dunconfig.SaveConfig(cfg); err != nil {
		m.append(StErr.Render("failed to save config: " + err.Error()))
		return
	}
	r := cfg.Resolve(facet, m.opts.Flags, os.Getenv)
	m.appendNotice(convoEntry{collapsed: okNotice("reset facets." + facet + " · " + strings.TrimSpace(strings.TrimPrefix(r.Line(), facet)))})
}

// rolesPanel renders the roles window in the lower pane. The highlighted
// row has the gutter bar and the accent; no row is dimmed.
func (m tuiModel) rolesPanel() string {
	cfg := dunconfig.LoadConfig()
	rows := []string{stHeader.Render("models by role")}
	for i, r := range m.resolvedRoles(cfg) {
		rows = append(rows, listRow("", r.Line(), "", i == m.rolesSel))
	}
	for _, n := range cfg.UnknownFacets() {
		rows = append(rows, listIndent+StDim.Render("facets."+n+": not a role; ignored"))
	}
	rows = append(rows, "")
	if i := m.rolesSel; i >= 0 && i < len(dunconfig.FacetNames) {
		f := dunconfig.FacetNames[i]
		rows = append(rows, wholeRows(f+": "+rolePurpose[f], m.w, listIndent, StDim)...)
	}
	rows = append(rows, wholeRows("embed, vision, segment and identity take effect when dund restarts", m.w, listIndent, StDim)...)
	rows = append(rows, wholeRows("the raglit server has its own roles: /rag setup", m.w, listIndent, StDim)...)
	return strings.Join(rows, "\n")
}

// rolesHint is the roles window's status line.
func rolesHint() string {
	return "roles  ·  ↑/↓ move · enter pick a model · p probe · x reset · esc close"
}
