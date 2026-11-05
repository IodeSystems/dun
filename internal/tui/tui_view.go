package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/iodesystems/dun/internal/textfmt"
)

// ── styles ─────────────────────────────────────────────────────────

// answerFG is the foreground of an answer's text: glamour's dark document
// colour, which every reply is rendered in.
var answerFG = *styles.DarkStyleConfig.Document.Color
var (
	stHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	// StDim is chrome: rules, arrows, key hints, labels. Not for text the
	// person reads to follow what happened (240 is 2.5:1 on a dark background).
	StDim = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	// stSecondary is content one step below the answer: a tool call's result
	// preview and body, a thought, a group's targets. Showing how an answer
	// was produced is required, so it is readable: 246 is 5.9:1 on the
	// renderer's #16181c and 6.9:1 on black. On a light terminal it is 240
	// (7.1:1 on white), set by initMarkdownStyle.
	stSecondary = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	// stUser is the question band: bold, on a filled band (236), in the
	// answer's own foreground — the colour glamour gives a reply's text
	// (answerFG, 8.6:1 on the band) — so the question reads as plainly as
	// the answer does.
	stUser = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("236")).Foreground(lipgloss.Color(answerFG))
	StTool = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	StErr  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	stNote = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // proactive notifications
	stAsk  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	stSel  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")) // selection gutter
	stEdge = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))            // focused divider half
	// stCmd is a command's name in a list (the palette, /help): neutral and
	// bold. Pink (212) is where you are; green is a thing that worked.
	stCmd = lipgloss.NewStyle().Bold(true)
	// stBrand is the wordmark: bold, not the accent — the accent is for
	// where you are.
	stBrand = lipgloss.NewStyle().Bold(true)
)

// divider is the single rule between the panes — the tmux split look, no
// boxes. It carries focus two ways (owner, 2026-10-24):
//
//   - colour: the focused pane's half in the accent, the other dim — the
//     first half when the upper side (conversation or activity strip) has the
//     keys, the last when the input (or a form, picker or question in its
//     place) has them;
//   - side: a thin line on the cell's edge toward the focused pane, the
//     bottom edge (▁) for the upper side, the top edge (▔) for the input.
//
// The two are mirror images (owner, 2026-10-25): a line 1/8 of a row, and
// beside the center, on the lit side, a short bump of half a row (▄ left of
// center for the upper side, ▀ right of it for the input). The ends used to
// thicken to 3/8, and downward that was lower blocks in reverse video, which
// drew much taller than the upward ramp; ▁/▔ and ▄/▀ are the mirror pairs
// fonts have. Moving focus moves the line from one edge of its row to the
// other; nothing else. Neither pane's CONTENT changes with focus: the person
// reads one pane while typing in the other (owner, 2026-10-23).
func divider(w int, focusUp bool) string {
	if w < 2 {
		w = 2
	}
	// The split is at the middle of the CONTENT, not of the terminal: on a
	// wide screen the text stops at bandCap, and a split at the terminal's
	// middle sat inside the text's column, under nothing on one side.
	half := min(w, bandCap) / 2
	bump := min(max(half/3, 2), 8)
	if focusUp {
		return stEdge.Render(strings.Repeat("▁", max(0, half-bump))+strings.Repeat("▄", bump)) +
			StDim.Render(strings.Repeat("▁", max(0, w-half)))
	}
	bump = min(bump, w-half)
	return StDim.Render(strings.Repeat("▔", half)) +
		stEdge.Render(strings.Repeat("▀", bump)+strings.Repeat("▔", max(0, w-half-bump)))
}

// bottomCue is drawn over the divider's right end while the conversation is
// scrolled away from its newest output: the way back, clickable, with its key.
// On the divider because that row is always there, so showing it moves nothing
// (a row of its own would shrink the view as you scroll — a bounce of its own).
func (m tuiModel) bottomCue() string {
	if k := m.keys.label("scroll.bottom"); k != "" {
		return " ↓ newest · " + k + " "
	}
	return " ↓ newest "
}

// withBottomCue puts the cue at the right end of a divider of width w, or
// returns the divider unchanged when it is too narrow to keep the focus half.
func withBottomCue(div, cue string, w int) string {
	cw := lipgloss.Width(cue)
	if w < 2*cw {
		return div
	}
	return ansi.Truncate(div, w-cw, "") + stNote.Render(cue)
}

// onBottomCue reports whether a click at column x on the divider row lands on
// the cue.
func (m tuiModel) onBottomCue(x int) bool {
	cw := lipgloss.Width(m.bottomCue())
	return !m.scrollPinned && m.w >= 2*cw && x >= m.w-cw
}
func addGutter(block, marker string, style lipgloss.Style) string {
	pre := style.Render(marker)
	lines := strings.Split(block, "\n")
	for i, ln := range lines {
		lines[i] = pre + ln
	}
	return strings.Join(lines, "\n")
}

// ── model ──────────────────────────────────────────────────────────

// Focus is which pane keys drive — tmux-style: Tab toggles, and the divider's
// focused half brightens. In convo focus, ↑/↓ move a message selection
// (left-border highlight) instead of recalling input history.
// Three zones, cycled by tab in screen order (input at the bottom, then the
// conversation, then the activity strip at the top). An EMPTY activity zone is
// SKIPPED, so a session that never delegates and never backgrounds a command
// keeps exactly the two-way toggle it always had.
const (
	focusInput    = iota // typing + ↑/↓ history (default)
	focusConvo           // ↑/↓ select a message, viewport follows
	focusActivity        // ↑/↓ pick an agent or job, → descends into it
)

// viewState is the inline display mode for a convoEntry. Enter cycles through
// them: minimized (one-line preview) → expanded (its full form).
type viewState int

const (
	viewMinimized viewState = iota // collapsed one-line preview
	viewExpanded                   // the full form
)

func (s viewState) Next() viewState {
	if s == viewMinimized {
		return viewExpanded
	}
	return viewMinimized
}

// panelOpen says a panel the person opened is below the conversation, in
// the composer's place: the palette, a form, a picker.
func (m tuiModel) panelOpen() bool {
	return m.picking || m.modelPicking || m.mcpForm || m.rag != nil || m.providerForm || m.loginPicking || m.paletteActive()
}

// histBrowsing says plain ↑/↓ walk history: the composer is empty, or holds
// the history entry it was last given, unchanged. Text the person typed is
// theirs, and ↑/↓ keep their composer/scroll meaning on it.
func (m tuiModel) histBrowsing() bool {
	if len(m.history) == 0 || len(m.input.Images()) > 0 {
		return false
	}
	v := m.input.Value()
	if m.histIdx >= len(m.history) {
		return v == ""
	}
	return m.histIdx >= 0 && v == m.history[m.histIdx]
}

// viewportCache memoizes one viewport render. Shared by every copy of the
// model, which is the point — see the vpc field. Bubble Tea drives Update and
// View from a single goroutine, so this needs no lock.
type viewportCache struct {
	out       string
	gen       uint64
	off, w, h int
	valid     bool
}

// viewportView is vp.View() memoized on (content, offset, size).
func (m tuiModel) viewportView(vp convoPane) string {
	c := m.vpc
	if c == nil {
		return vp.View()
	}
	if c.valid && c.gen == m.contentGen && c.off == vp.YOffset && c.w == vp.Width && c.h == vp.Height {
		return c.out
	}
	c.out, c.valid = vp.View(), true
	c.gen, c.off, c.w, c.h = m.contentGen, vp.YOffset, vp.Width, vp.Height
	return c.out
}

// scrollOverlay returns a one-line bar showing the last user message that
// is fully above the viewport (the one just scrolled past), or "" when
// scrolled to bottom or no user message is off-screen. The bar uses the
// user message style so it's visually distinct from the conversation
// content below it.
//
// Following the tail, it shows only while a turn runs, and only that turn's
// message: the view no longer holds the message on screen while the reply
// streams (applyScroll), so this is where it stays readable. An older message
// would replace the title bar for the whole of a long session.
func (m tuiModel) scrollOverlay() string {
	text := m.overlayQuestion()
	if text == "" {
		return ""
	}
	return m.overlayBar(text, max(1, m.vp.Width))
}

// overlayBar is the question as a one-row band w cells wide.
func (m tuiModel) overlayBar(text string, w int) string {
	// Exactly one row, always. clip() overshoots — it appends the ellipsis to n
	// characters rather than fitting inside n — so "› " + clip(text, width-2)
	// came to width+1 cells and lipgloss wrapped the bar onto a second line.
	// That put the frame a row over the terminal, which costs the top row: this
	// bar. fitPlain is the width-aware fit the task line right below already
	// uses, and oneLine keeps an embedded newline from doing the same thing.
	text, _ = fitPlain(textfmt.OneLine(text), max(0, w-2))
	return stUser.Width(max(1, w)).Render("› " + text)
}

// overlayQuestion is the user message the overlay names, "" for none.
func (m tuiModel) overlayQuestion() string {
	if m.vp.YOffset == 0 || (m.scrollPinned && !m.busy) {
		return "" // at top, or following with no turn running — nothing to show
	}
	lastUser := -1
	for i := range m.convo {
		if m.convo[i].userText != "" {
			lastUser = i
		}
	}
	yOff := m.vp.YOffset
	// Each entry's rowOffset (set by refresh()) is the viewport line where
	// it starts. blockH[i] is its height. Together they give exact row
	// ranges — no re-rendering needed.
	//
	// Pick the off-screen user message whose bottom is closest to the
	// viewport top — the one the user most recently scrolled past. When
	// both "first" and "second" are off-screen, we want "second" only
	// until the user scrolls past it; then we want "first".
	// Read the geometry from the frame — the single snapshot refresh last wrote
	// — not m.blockH / e.rowOffset directly. Same numbers (refresh writes the
	// entries from the frame), one value every reader agrees on.
	bh, ro := m.frame.blockH, m.frame.rowOffset
	var closestText string
	closestBottom := -1
	for i, e := range m.convo {
		if e.userText == "" || (m.scrollPinned && i != lastUser) {
			continue
		}
		h := 0
		if i < len(bh) {
			h = bh[i]
		}
		top := 0
		if i < len(ro) {
			top = ro[i]
		}
		bottom := top + h
		if bottom <= yOff && bottom > closestBottom {
			closestBottom = bottom
			closestText = e.userText
		}
	}
	return closestText
}

// convoHeight is the number of rows the conversation viewport actually gets
// drawn at: the terminal minus every fixed row around it. It is the single
// layout truth — View lays the frame out with it, and Update keeps m.vp.Height
// equal to it.
//
// Both matter. Overcount and View emits more rows than the terminal has, and
// Bubble Tea's renderer keeps the LAST h lines — it silently drops the header,
// which is where the scroll overlay lives. Undercount m.vp.Height and
// AtBottom/GotoBottom clamp against a window taller than the drawn one, hiding
// the bottom row of the conversation.
// convoHeightBudget is the PURE budget: how many rows the convo viewport gets
// at the model's CURRENT size, computed from model state (terminal height, the
// head/activity/lower rows). It is a function of (h, head, activity, lower) and
// reads no layout cache, so it is safe to call from the Update funnel BEFORE a
// refresh has run — which is exactly when a resize must be detected.
//
// The frame (m.frame.height) records this number for the last refresh, so the
// render path reads ONE computed value instead of re-deriving it. The two agree
// because refresh is the only writer and it sets frame.height from this.
func (m tuiModel) convoHeightBudget() int {
	h := m.h - 2 // divider 1 + status 1; every other row is measured, not assumed
	h -= lipgloss.Height(m.headView())
	// The activity strip sits above the conversation. It vanishes when it has
	// nothing to say, so a session that never delegates loses no space to it.
	if a := m.activityView(); a != "" {
		h -= lipgloss.Height(a)
	}
	h -= lipgloss.Height(m.lowerView()) // input, or the answer picker (several rows)
	if u := m.upgradeView(); u != "" {
		h -= lipgloss.Height(u)
	}
	return max(1, h)
}

// convoHeight is the height the render path uses: the frame's recorded budget,
// or the freshly computed one while no frame exists yet (startup, before the
// first refresh). It is the single number View, applyScroll, and the scroll
// overlay agree on.
func (m tuiModel) convoHeight() int {
	if m.frame.lines != nil {
		return m.frame.height
	}
	return m.convoHeightBudget()
}

// headView is the top row: the scroll overlay when the conversation is scrolled
// up, else the dun title bar. Measured rather than assumed to be one row — it
// is the row the renderer drops first when a frame overdraws, so a head that
// wrapped would delete itself.
func (m tuiModel) headView() string {
	if q := m.overlayQuestion(); q != "" {
		// The question shares the row with the request metrics, which a turn
		// is when they matter (USER 2026-10-28: the question "leaving the dun
		// status header out of view"). The metrics get at most half the row,
		// dropping their least pieces first; the question the rest.
		w := max(1, m.vp.Width)
		stats := m.reqs.view(m.w, w/2-2)
		if stats == "" {
			return m.overlayBar(q, w)
		}
		sw := ansi.StringWidth(stats)
		return m.overlayBar(q, w-sw-2) + "  " + stats
	}
	// Minimal: the project's folder and the model in use, where they can be
	// checked at any time. The build, the full path, the tools and the
	// servers are in the splash and /status (splash.go).
	head := stBrand.Render("dun")
	if m.workspace != "" {
		head += StDim.Render(" · " + filepath.Base(m.workspace))
	}
	if m.branch != "" {
		head += StDim.Render(" ⎇ " + m.branch)
	}
	if m.model != "" && m.url != "" && !m.splashModelShown() {
		head += StDim.Render(" · " + m.model)
	}
	if n := m.liveAgents(); n > 0 {
		head += stNote.Render("  · " + textfmt.Plural(n, "agent"))
	}
	return m.reqs.withStats(head, m.w)
}

// View is drawView, remembered: /repaint's expected screen is the last frame
// the renderer was handed (repaint.go).
//
// The terminal modes are declared here too (Bubble Tea v2 diffs them), so
// /mouse is a field and not a command.
//
// Mouse: ALL-motion (1003+1006) by default, which is what Claude Code runs —
// measured in a pty from a trusted cwd, claude 2.1.246 writes 1000h 1002h
// 1003h 1006h. (A capture taken from an UNtrusted cwd stops at the "trust
// this folder?" prompt and shows no mouse mode at all; that artifact is what
// made 96f9436 drop mouse mode.)
//
// 1003 rather than 1002 is the whole point on Termux. tmux forwards the
// ACTIVE pane's mouse mode to the outer terminal, and with `mouse on` it
// upgrades a pane that asks for nothing to MODE_MOUSE_BUTTON — so a
// 1002-or-nothing pane and a 1003 pane hand Termux different sequences.
// Measured on the real device: panes running claude (`mouse_all_flag=1`)
// raise the soft keyboard on tap; dun's pane (1002, then none) did not.
// Matching claude's mode is what makes tap-to-type work.
func (m tuiModel) View() tea.View {
	out := m.drawView()
	if m.shown != nil {
		m.shown.s = out
	}
	v := tea.NewView(out)
	v.AltScreen = true
	v.MouseMode = mouseModeOf(m.mouseMode)
	v.ReportFocus = true // a focus-in clears the done mark (tmux.go)
	return v
}

// mouseModeOf is /mouse's setting as the View field.
func mouseModeOf(mode string) tea.MouseMode {
	switch mode {
	case "cell":
		return tea.MouseModeCellMotion
	case "off":
		return tea.MouseModeNone
	}
	return tea.MouseModeAllMotion
}
func (m tuiModel) drawView() string {
	start := time.Now()
	defer func() { frames.observe(stageView, time.Since(start)) }()
	// The inspector is a full-screen overlay — it replaces the normal layout.
	if m.inspecting {
		return m.insp.view(m.w, m.h)
	}
	// The lower pane is the input, or — while answering — the option picker. The
	// convo pane takes whatever height is left (the picker can be several rows).
	lower := m.lowerView()
	// The activity strip sits above the conversation and takes its height off
	// the top. It vanishes when it has nothing to say, so a session that never
	// delegates loses no space to it.
	top := make([]string, 0, 1)
	if a := m.activityView(); a != "" {
		top = append(top, a)
	}
	// Read the height from the frame — the same snapshot applyScroll and the
	// scroll overlay read — rather than computing a fourth copy here. The local
	// vp is a value copy (View has a value receiver); setHeight on it keeps the
	// join below consistent with the frame without mutating the model.
	vp := m.vp
	vp.setHeight(m.convoHeight())
	// Focus cue lives entirely in the divider's bright half — no pane borders.
	div := divider(m.w, m.focus != focusInput && !m.asking)
	if !m.scrollPinned {
		div = withBottomCue(div, m.bottomCue(), m.w)
	}

	var status string
	switch {
	case m.fatalErr != "":
		status = StErr.Render(m.fatalErr)
	case m.starting:
		status = m.spin.View() + StDim.Render(" "+m.startingWhat()+m.startingElapsed()+m.startQueuedHint())
	case m.confirm.q != "":
		status = stAsk.Render("❓ " + m.confirm.q)
	case m.flashing():
		status = stNote.Render(m.flashText)
	case m.activeScreen() != nil:
		status = m.scr.hint(&m)
	case m.jobViewing:
		status = StDim.Render(m.jv.hint())
	case m.paging:
		if v := m.serversPaging(); v != nil {
			status = v.hint() // styled itself: its hotkeys are marked
		} else if v := m.switchesPaging(); v != nil {
			status = StDim.Render(v.hint())
		} else {
			status = StDim.Render(m.pg.hint())
		}
	case m.asking && len(m.askOptions) == 0:
		status = stAsk.Render("❓ type your answer · enter send · esc answer later")
	case m.asking && m.noting:
		status = stAsk.Render("❓ type a detail · enter keep it · esc back to the options")
	case m.asking && m.customAnswer && m.askMulti:
		status = stAsk.Render("❓ type the option · enter keep it, checked · esc back to the options")
	case m.asking && m.customAnswer:
		status = stAsk.Render("❓ type your answer · enter send · esc back to the options")
	case m.asking && m.askMulti:
		status = stAsk.Render("❓ ↑/↓ move · enter toggle · tab/→ edit · ✓ done to submit · esc answer later")
	case m.asking:
		status = stAsk.Render("❓ ↑/↓ choose · enter select · tab/→ edit · n add detail · esc answer later")
	case m.searching:
		status = m.search.View() + StDim.Render(fmt.Sprintf("  (%d matches · enter to navigate)", len(m.matches)))
	case m.searchActive:
		status = StDim.Render(fmt.Sprintf("match %d/%d  ·  ↑/↓ prev/next · / new search · esc exit", m.matchPos+1, len(m.matches)))
	// A form or picker shows its own keys here, never the composer's: two
	// meanings of enter on one screen was the fault.
	case m.modelPicking && m.loginPicking:
		status = StDim.Render(loginHint())
	case m.modelPicking && m.providerForm:
		status = StDim.Render(m.providerHint())
	case m.modelPicking:
		status = StDim.Render(modelHint())
	case m.mcpForm:
		status = StDim.Render(m.mcpHint())
	case m.rag != nil:
		status = StDim.Render(m.ragSetupHint())
	case m.paletteActive():
		what := "command"
		if _, _, ok := m.paletteArgs(); ok {
			what = "argument"
		}
		status = StDim.Render(what + "  ·  ↑/↓ select · enter run · tab complete · esc close")
	case m.askParked && m.focus != focusConvo:
		status = stAsk.Render("❓ a question is waiting") + StDim.Render(" · tab, select it, enter to answer · "+m.exitHint())
	case m.retry != "":
		// The countdown first: a row too wide for the window loses its end.
		cd := strings.TrimPrefix(m.retryCountdown(), " · ")
		if cd != "" {
			cd += " · "
		}
		status = stNote.Render("⏳ "+cd+m.retry) + StDim.Render("  ("+m.busyHint()+")")
	case m.busy && m.interrupting:
		status = m.spin.View() + " " + StDim.Render("interrupting…"+m.busyElapsed()+m.queuedHint())
	case m.busy:
		what := ""
		if m.activeTool != "" {
			what = " " + m.activeTool
		}
		status = m.spin.View() + " " + m.spendStatus(time.Now()) + StDim.Render("working…"+what+m.busyElapsed()+m.queuedHint()+" · "+m.busyHint())
	case m.focus == focusActivity && m.actLevel == actCollapsed:
		status = StDim.Render("activity  ·  → open · tab input · " + m.exitHint())
	case m.focus == focusActivity:
		status = StDim.Render("activity  ·  ↑/↓ select · →/enter open (agent = its view) · x kill job · tab next zone · esc input")
	case m.scopeAgent != 0:
		status = StDim.Render(fmt.Sprintf("agent #%d  ·  type to tell it (its next message) · ←/esc back to the session", m.scopeAgent))
	case m.focus == focusConvo:
		if d := m.selDocs(); d != nil && d.descended {
			status = StDim.Render("docs  ·  ↑/↓ doc · → open · ← close, then out · ctrl+c quit")
		} else {
			switch {
			case m.insideBox() != nil:
				status = StDim.Render("inside  ·  ↑/↓ pgup/pgdn g/G scroll · ← leave · enter open")
			case m.insideWide() != nil:
				w := m.insideWide()
				status = StDim.Render(fmt.Sprintf("sideways  ·  ←/→ scroll · home/end edge · esc leave · col %d of %d", w.off+1, w.max+m.vp.Width-wideIndicatorRoom))
			case m.selWide() != nil && m.selWide().max > 0:
				status = StDim.Render("convo  ·  ↑/↓ select · → scroll sideways · y copy · tab input")
			case m.selBox() != nil:
				status = StDim.Render("convo  ·  ↑/↓ select · → scroll inside · enter open · tab input")
			default:
				status = StDim.Render("convo  ·  ↑/↓ select · →/← open/close · y copy · tab input")
			}
		}
	case m.suggestActive():
		status = StDim.Render(fmt.Sprintf("next?  ·  ↑/↓ cycle · 1–%d pick · enter/right accept · or type · "+m.exitHint(), len(m.suggestions)))
	default:
		// Most-used first: a narrow window drops hints from the end.
		// "ready" only when there is something to be ready with.
		// Not logged in: the conversation's line says so (in the attention
		// colour), so the status line does not say it again, and names only
		// what works yet.
		if !m.loggedIn() {
			enter := "enter send"
			if strings.TrimSpace(m.input.Value()) == "" {
				enter = "enter log in" // an empty composer's enter opens the form
			}
			status = StDim.Render(enter + " · / commands · " + m.exitHint())
			break
		}
		if m.restoredMsg != "" && m.input.Value() == m.restoredMsg {
			// The composer holds the question asked before the login: say
			// it is that, not something just typed. The text sent is only
			// the question.
			status = m.spendStatus(time.Now()) + StDim.Render("your earlier question  ·  enter asks it · or change it first")
			break
		}
		// Four hints at most: the rest are in /help. The way out is never
		// the one a narrow row drops (fitHints).
		status = m.spendStatus(time.Now()) + StDim.Render("ready"+m.serversStartingTail()+"  ·  "+m.composeHint()+m.exitHint())
	}
	// The header row shows the off-screen user message when scrolled up, or the
	// normal dun title bar when at the bottom — convoHeight() budgeted for it.
	convo := m.selOverlay(m.viewportView(vp), vp.YOffset)
	if m.paging {
		convo = m.pg.view(vp.Width, vp.Height)
	}
	if m.jobViewing {
		convo = m.jv.view(vp.Width, vp.Height)
	}
	out := []framePart{{"head", m.headView()}}
	for _, t := range top {
		out = append(out, framePart{"activity", t})
	}
	out = append(out, framePart{"conversation", convo}, framePart{"divider", div}, framePart{"input", lower})
	if s := m.activeScreen(); s != nil { // it takes the rows the base screen had
		out = []framePart{{"head", m.headView()}, {"screen", s.view(&m, m.w, m.screenRows())}}
	}
	if u := m.upgradeView(); u != "" {
		out = append(out, framePart{"upgrade", u}) // persistent while dund upgrades
	}
	out = append(out, framePart{"status", fitHints(status, m.w)})
	// The one check that no part pushed the input off the screen (overflow.go).
	return fitFrame(out, m.h, m.w)
}

// lowerView is the bottom pane: the input line, or the answer picker when asking.
func (m tuiModel) lowerView() string {
	if m.asking {
		return m.askPanel()
	}
	if m.picking {
		return m.sessionPanel()
	}
	if m.modelPicking {
		return fitRows(m.modelPanel(), m.w)
	}
	if m.mcpForm {
		return fitRows(m.mcpFormPanel(), m.w)
	}
	if m.rag != nil {
		return fitRows(m.ragSetupPanel(), m.w)
	}
	if m.paletteActive() {
		return m.palettePanel()
	}
	// Suggestions are rendered inside the viewport (refresh), not here — so they
	// don't steal height from the conversation. The input line is always one row.
	if m.suggestActive() {
		if len(m.suggestions) > 0 && m.suggestSel < len(m.suggestions) &&
			strings.TrimSpace(m.input.Value()) == "" {
			return m.input.ghostView(m.suggestions[m.suggestSel].text)
		}
		return m.input.View()
	}
	return m.input.View()
}

// askPanel renders the answer options (highlighted selection), a trailing
// custom/chat row, any attached detail, and the text field when capturing one.
func (m tuiModel) askPanel() string {
	custom := len(m.askOptions)
	rows := make([]string, 0, custom+2)
	sel := func(i int) bool { return m.askSel == i && !m.noting && !m.customAnswer }
	gut := func(text string, on bool) string {
		if on {
			return addGutter(text, "▎ ", stSel)
		}
		return addGutter(text, "  ", lipgloss.NewStyle())
	}
	for i, opt := range m.askOptions {
		label := opt
		if m.askMulti { // a checkbox per option
			box := "☐ "
			if i < len(m.askChecked) && m.askChecked[i] {
				box = "☑ "
			}
			label = box + opt
		}
		if slices.Contains(m.askRec, opt) {
			label += StDim.Render("  (recommended)")
		}
		rows = append(rows, gut(label, sel(i)))
	}
	// The affordance belongs on the row itself: highlighted and not yet
	// capturing is exactly the moment someone wonders whether to press enter.
	customLabel := "✎ custom answer / chat…"
	if sel(custom) && !m.customAnswer {
		customLabel += "   (just type)"
	}
	rows = append(rows, gut(StDim.Render(customLabel), sel(custom) || m.customAnswer))
	if m.askMulti { // a submit row so enter can toggle options without needing space
		n := 0
		for _, on := range m.askChecked {
			if on {
				n++
			}
		}
		rows = append(rows, gut(stAsk.Render(fmt.Sprintf("✓ done — submit %d selected", n)), sel(custom+1)))
	}
	if m.askNote != "" {
		rows = append(rows, StDim.Render("   detail: "+m.askNote))
	}
	if m.noting || m.customAnswer {
		rows = append(rows, m.input.View())
	}
	return strings.Join(rows, "\n")
}
