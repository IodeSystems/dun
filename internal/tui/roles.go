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
	dunconfig.FacetVision:   "reads images and does OCR; off: basic OCR (tesseract) if installed",
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
		m.rolesSaid = ""
		m.rolesSel = max(0, m.rolesSel-1)
		m.refresh()
	case "down", "ctrl+n":
		m.rolesSaid = ""
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
		m.roleSaid(facet+" already uses its default ("+roleWhat(r)+")", false)
		return
	}
	cfg.SetFacet(facet, dunconfig.Facet{})
	if err := dunconfig.SaveConfig(cfg); err != nil {
		m.append(StErr.Render("failed to save config: " + err.Error()))
		return
	}
	r := cfg.Resolve(facet, m.opts.Flags, os.Getenv)
	m.roleSaid(facet+" reset to its default: "+roleWhat(r), true)
}

// roleNote is a result of the roles window, kept for the conversation until
// the window closes. ok: a ✓ notice (appendNotice); else a dim line.
type roleNote struct {
	text string
	ok   bool
}

// roleSaid says what a role change did. With the roles window (or a
// role's picker) open it goes on the window's hint row and waits: added to
// the conversation then, the pinned view followed it and the rows above the
// window moved up (ux models run, steps 06-07). The close puts the waiting
// lines in the conversation (flushRoleNotes), into the rows the window gave
// back, so nothing moves then either. With no window open (/model use) it
// is said in the conversation at once.
func (m *tuiModel) roleSaid(text string, ok bool) {
	if !m.modelPicking || (!m.rolesPicking && m.pickFacet == "") {
		m.sayRoleNote(roleNote{text, ok})
		return
	}
	m.rolesSaid = text
	if ok {
		m.rolesSaid = "✓ " + text
	}
	m.rolesQueued = append(m.rolesQueued, roleNote{text, ok})
	m.refresh()
}

func (m *tuiModel) sayRoleNote(n roleNote) {
	if n.ok {
		m.appendNotice(convoEntry{collapsed: okNotice(n.text)})
		return
	}
	m.append(StDim.Render(n.text))
}

// flushRoleNotes puts the window's waiting results in the conversation.
func (m *tuiModel) flushRoleNotes() {
	q := m.rolesQueued
	m.rolesQueued, m.rolesSaid = nil, ""
	for _, n := range q {
		m.sayRoleNote(n)
	}
}

// rolesStatus is the window's status row: the last result, then the keys
// that fit after it. A result too long for any key is cut before esc
// close; the next move gives the keys back.
func (m tuiModel) rolesStatus() string {
	if m.rolesSaid == "" {
		return StDim.Render(rolesHint(m.w))
	}
	said := StDim.Render(m.rolesSaid)
	if strings.HasPrefix(m.rolesSaid, "✓ ") {
		said = stOK.Render(m.rolesSaid)
	}
	w := m.w
	if w <= 0 {
		w = 1 << 30
	}
	if keys, ok := rolesKeys(w - ansi.StringWidth(m.rolesSaid)); ok {
		return said + StDim.Render("  ·  "+keys)
	}
	const out = "  ·  esc close" // the way out stays; the result is cut
	return fitText(said, max(1, m.w-len(out))) + StDim.Render(out)
}

// restartNote is said once, in the window, and says what dund is: the save
// message says only what was saved.
const restartNote = "chat images: new sessions · search and page OCR: after the background service (dund) restarts"

// roleShort is a role in a few words, for the completion list.
var roleShort = map[string]string{
	dunconfig.FacetMain:     "the conversation",
	dunconfig.FacetChild:    "sub-agents",
	dunconfig.FacetVision:   "images and OCR",
	dunconfig.FacetEmbed:    "search vectors",
	dunconfig.FacetSegment:  "splits documents",
	dunconfig.FacetIdentity: "captions and tags",
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
	notes = append(notes, wholeRows(restartNote, m.w, listIndent, StDim))
	notes = append(notes, wholeRows("the raglit server has its own roles: /rag setup", m.w, listIndent, StDim))
	for _, n := range cfg.UnknownFacets() {
		notes = append(notes, []string{listIndent + StDim.Render("facets."+n+": not a role; ignored")})
	}
	// A blank row, then each note that fits whole, in order of use, then a
	// blank row before the hints (the last row of the budget).
	if len(rows)+1 < budget {
		rows = append(rows, "")
		noted := false
		for _, n := range notes {
			if len(rows)+len(n) < budget {
				rows = append(rows, n...)
				noted = true
			}
		}
		if !noted {
			rows = rows[:len(rows)-1]
		}
	}
	if len(rows) < budget {
		rows = append(rows, "")
	}
	return strings.Join(rows, "\n")
}

// rolesHint is the roles window's status line. At a narrow width the keys
// that fit are the most needed first: enter, esc, x, p, then move.
func rolesHint(w int) string {
	const head = "roles"
	if keys, ok := rolesKeys(w - len(head)); ok {
		return head + "  ·  " + keys
	}
	return head
}

func rolesKeys(w int) (string, bool) {
	keys := []struct {
		order int
		text  string
	}{
		{0, "↑/↓ move"}, {1, "enter pick a model"}, {2, "p test it"}, {3, "x reset"}, {4, "esc close"},
	}
	priority := []int{1, 4, 3, 2, 0}
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
		line := strings.Join(parts, " · ")
		if ansi.StringWidth(line)+5 <= w {
			return line, true
		}
	}
	return "", false
}
