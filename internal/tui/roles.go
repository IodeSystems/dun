package tui

import (
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

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
		r := cfg.Resolve(facet, m.opts.Flags, os.Getenv)
		m.append(StDim.Render(facet + " already uses its default (" + roleWhat(r) + ")"))
		return
	}
	cfg.SetFacet(facet, dunconfig.Facet{})
	if err := dunconfig.SaveConfig(cfg); err != nil {
		m.append(StErr.Render("failed to save config: " + err.Error()))
		return
	}
	r := cfg.Resolve(facet, m.opts.Flags, os.Getenv)
	m.appendNotice(convoEntry{collapsed: okNotice(facet + " reset to its default: " + roleWhat(r))})
}

// roleWhat is what a role is served by, in words: "vision-standard on local",
// "off", "same as main". No role name, no source.
func roleWhat(r dunconfig.Resolved) string {
	l := strings.TrimSpace(strings.TrimPrefix(r.Line(), r.Facet))
	if i := strings.LastIndex(l, " · "); i >= 0 {
		l = l[:i]
	}
	return l
}

// rolesPanel renders the roles window in the lower pane, in at most half the
// terminal's rows. The highlighted row has the gutter bar and the accent; no
// row is dimmed. When the rows do not fit the window scrolls to keep the
// highlighted one in view; the notes go first, then the rows scroll.
func (m tuiModel) rolesPanel() string {
	cfg := dunconfig.LoadConfig()
	roles := m.resolvedRoles(cfg)
	budget := 1 << 30
	if m.h > 0 {
		budget = max(5, m.h/2)
	}
	rows := []string{stHeader.Render("models by role")}
	avail := max(1, budget-1)
	start := 0
	if len(roles) > avail {
		start = min(max(0, m.rolesSel-avail+1), len(roles)-avail)
	}
	for i := start; i < len(roles) && i < start+avail; i++ {
		rows = append(rows, listRow("", roles[i].Line(), "", i == m.rolesSel))
	}
	var notes [][]string
	if i := m.rolesSel; i >= 0 && i < len(dunconfig.FacetNames) {
		f := dunconfig.FacetNames[i]
		notes = append(notes, wholeRows(f+": "+rolePurpose[f], m.w, listIndent, StDim))
	}
	notes = append(notes, wholeRows("the search and image roles apply after dund restarts", m.w, listIndent, StDim))
	notes = append(notes, wholeRows("the raglit server has its own roles: /rag setup", m.w, listIndent, StDim))
	for _, n := range cfg.UnknownFacets() {
		notes = append(notes, []string{listIndent + StDim.Render("facets."+n+": not a role; ignored")})
	}
	// A blank row, then each note that fits whole, in order of use.
	if len(rows)+1 < budget {
		rows = append(rows, "")
		for _, n := range notes {
			if len(rows)+len(n) <= budget {
				rows = append(rows, n...)
			}
		}
		if rows[len(rows)-1] == "" {
			rows = rows[:len(rows)-1]
		}
	}
	return strings.Join(rows, "\n")
}

// rolesHint is the roles window's status line. At a narrow width the keys
// that fit are the most needed first: enter, esc, x, p, then move.
func rolesHint(w int) string {
	keys := []struct {
		order int
		text  string
	}{
		{0, "↑/↓ move"}, {1, "enter pick a model"}, {2, "p probe"}, {3, "x reset"}, {4, "esc close"},
	}
	priority := []int{1, 4, 3, 2, 0}
	const head = "roles"
	for n := len(priority); n >= 1; n-- {
		keep := map[int]bool{}
		for _, o := range priority[:n] {
			keep[o] = true
		}
		var parts []string
		for _, k := range keys {
			if keep[k.order] {
				parts = append(parts, k.text)
			}
		}
		line := head + "  ·  " + strings.Join(parts, " · ")
		if w <= 0 || ansi.StringWidth(line) <= w || n == 1 {
			return line
		}
	}
	return head
}
