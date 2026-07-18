package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// The TUI is a CLIENT of the `-p` JSON event protocol: it re-execs `dun -p`,
// writes user events to its stdin, and renders the event stream. The engine
// stays headless; the UI is pure presentation.

// tuiOpts are the flags the TUI forwards to its `dun -p` subprocess.
type tuiOpts struct {
	workspace, model, url, key, docker string
	noWorktree                         bool
	pr                                 bool
	cont                               bool   // --continue: resume the latest session
	resume                             string // --resume <id>: resume a specific session
}

// runTUI launches the Bubble Tea app against a re-exec'd `dun -p` subprocess.
func runTUI(o tuiOpts) error {
	loadScriptRenderers() // ~/.dun/renderers/*.star override/extend the built-ins
	proc, err := startDunProc(o)
	if err != nil {
		return err
	}
	m := newTUIModel(proc, o.workspace)
	// WithMouseCellMotion makes the terminal (and tmux) forward wheel events to
	// us instead of scrolling its own scrollback; the viewport consumes them.
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	proc.close()
	return err
}

// ── styles ─────────────────────────────────────────────────────────

var (
	stHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	stDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	stUser   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	stTool   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	stErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	stNote   = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // proactive notifications
	stAsk    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	stSel    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")) // selection gutter
	stEdge   = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))            // focused divider half
)

// paneStyle borders a pane; the focused one is bright (212), else dim (240) —
// the tmux split-pane look (the bright border is the focused pane's half-edge).
// divider is the single thin rule between the panes (tmux-minimal, no boxes).
// The focused pane's half is bright: first half if the top (convo) is focused,
// last half if the bottom (input/ask) is — the "half-edge" focus cue.
func divider(w int, focusUp bool) string {
	if w < 2 {
		w = 2
	}
	half := w / 2
	left, right := strings.Repeat("─", half), strings.Repeat("─", w-half)
	if focusUp {
		return stEdge.Render(left) + stDim.Render(right)
	}
	return stDim.Render(left) + stEdge.Render(right)
}

// addGutter prefixes every line of a (possibly multi-line) block with marker,
// so a highlighted message/option shows a left border down its whole height.
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
const (
	focusInput = iota // typing + ↑/↓ history (default)
	focusConvo        // ↑/↓ select a message, viewport follows
)

// convoEntry is one conversation block. A tool call/result is collapsible: full
// holds the whole output, collapsed a one-line preview; enter (when focused)
// toggles open. A relevant-docs summary carries a docsBlock (nested navigation).
// A plain block has neither full nor docs.
type convoEntry struct {
	collapsed string
	full      string
	open      bool
	docs      *docsBlock // proactive-RAG summary (nil for normal blocks)
	tool      *toolBlock // tool call/result (nil for normal blocks) → enter opens the inspector
}

func (e convoEntry) expandable() bool { return e.full != "" || e.docs != nil }

// toolBlock carries a tool call's raw input + complete output so enter can open
// the scrollable/searchable inspector overlay (inspector.go), separate from the
// inline collapsed/full preview.
type toolBlock struct {
	name   string
	input  string
	output string
}

func (e convoEntry) view() string {
	if e.docs != nil {
		return e.docs.render(e.open)
	}
	if e.open && e.full != "" {
		return e.full
	}
	return e.collapsed
}

// docNode is one surfaced document inside a docsBlock; open shows its snippet.
type docNode struct {
	title, line string
	score       float64
	open        bool
}

// docsBlock is a collapsed proactive-RAG summary ("N relevant · M surfaced").
// Expanding (enter) reveals the surfaced docs; → descends into the list, where
// ↑/↓ move between docs, enter expands a doc's snippet, ← / esc ascends.
type docsBlock struct {
	found, surfaced int
	docs            []docNode
	descended       bool // focus is inside the doc list
	cur             int  // selected doc when descended
}

func (d *docsBlock) render(open bool) string {
	glyph := "▸ "
	if open {
		glyph = "▾ "
	}
	head := stNote.Render(fmt.Sprintf("%s🔎 %d relevant doc(s) · %d surfaced", glyph, d.found, d.surfaced))
	if !open || len(d.docs) == 0 {
		return head
	}
	lines := []string{head}
	for i, doc := range d.docs {
		cursor := "  "
		title := doc.title
		if d.descended && i == d.cur {
			cursor = stSel.Render("➤ ")
			title = stSel.Render(title)
		}
		dg := "▸"
		if doc.open {
			dg = "▾"
		}
		lines = append(lines, fmt.Sprintf("   %s%s %s  %s", cursor, dg, title, stDim.Render(fmt.Sprintf("(%.2f)", doc.score))))
		if doc.open && doc.line != "" {
			lines = append(lines, stDim.Render("        "+doc.line))
		}
	}
	return strings.Join(lines, "\n")
}

type tuiModel struct {
	proc      *dunProc
	workspace string
	vp        viewport.Model
	input     textinput.Model
	spin      spinner.Model
	convo     []convoEntry // finalized conversation blocks
	pendingTool int        // index of a tool call awaiting its result; -1 = none
	pendingArgs map[string]any // args of the pending tool call (for its renderer)
	cur         string     // streaming assistant text (not yet finalized); string, not
	//                    strings.Builder — Bubble Tea copies the model each Update.
	tools      []string
	branch     string // worktree branch (from the `workspace` event)
	starting   bool   // spawning servers, before `ready`
	busy       bool   // a turn in flight
	asking       bool     // agent is waiting on an ask_user answer
	askOptions   []string // the offered options; a trailing "custom" row is implicit
	askSel       int      // highlighted answer row (== len(askOptions) → the custom row)
	askNote      string   // optional detail attached to the chosen option ("n")
	askMulti     bool     // multi-select: space toggles, enter submits the checked set
	askChecked   []bool   // per-option checked state (multi mode; len == len(askOptions))
	noting       bool     // capturing a detail for the selected option
	customAnswer bool     // capturing a free-text / chat answer
	md           *glamour.TermRenderer // markdown renderer for assistant replies
	history    []string              // sent inputs, for up/down recall
	histIdx    int                   // cursor into history (== len when not browsing)
	focus      int                   // focusInput | focusConvo
	sel        int                   // selected message index (convo focus); -1 = none
	search       textinput.Model // vim-style "/" message search box
	searching    bool            // typing a search query
	searchActive bool            // navigating matches (↑/↓ step, esc exits)
	matches      []int           // convo indices matching the query
	matchPos     int             // cursor into matches
	blockH       []int           // rendered height of each convo block (for tall-message scroll)
	inspecting bool      // the tool inspector overlay is open (owns all keys)
	insp       inspector // the overlay (valid while inspecting)
	dumpSig    chan os.Signal // SIGUSR1 → dump the rendered screen to a debug file
	webAddr    string         // bound address of the embedded /web server ("" = off)
	paletteSel int            // highlighted row in the "/" command palette
	w, h       int
	fatalErr   string
}

func newTUIModel(proc *dunProc, workspace string) tuiModel {
	in := textinput.New()
	in.Placeholder = "ask dun to do something…"
	in.Prompt = "› "
	in.Focus()
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	se := textinput.New()
	se.Prompt = "/"
	se.Placeholder = "search messages…"
	// SIGUSR1 → dump the rendered screen (see dumpMsg): the alt-screen hides what
	// the TUI is showing, so an out-of-band `kill -USR1 <pid>` snapshots it.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	return tuiModel{proc: proc, workspace: workspace, input: in, search: se, spin: sp, dumpSig: sig, starting: true, sel: -1, pendingTool: -1}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(waitEvent(m.proc.ch), m.spin.Tick, textinput.Blink, waitForDump(m.dumpSig))
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		// Layout: head(1) + convo + divider(1) + lower + status(1). Convo takes
		// h-4 in the normal (1-line input) case; View recomputes it when the
		// lower pane grows (an ask panel).
		m.vp = viewport.New(max(1, msg.Width), max(1, msg.Height-4))
		m.input.Width = msg.Width - 2
		m.search.Width = msg.Width - 4
		m.md = newMarkdown(msg.Width - 2)
		if m.inspecting {
			m.insp.setSize(m.w, m.h)
		}
		m.refresh()
		return m, nil

	case tea.KeyMsg:
		// The tool inspector overlay owns every key while open.
		if m.inspecting {
			open, cmd := m.insp.update(msg)
			m.inspecting = open
			return m, cmd
		}
		// Answering an ask_user is a mode of its own (select an option, add a
		// detail, or type a custom/chat answer) — it owns the keys.
		if m.asking {
			return m.updateAsking(msg)
		}
		// Typing a "/" search query owns the keys until enter/esc.
		if m.searching {
			return m.updateSearch(msg)
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.searchActive { // leave match-scroll mode, back to free selection
				m.searchActive = false
				m.matches = nil
				m.refresh()
				return m, nil
			}
			if d := m.selDocs(); d != nil && d.descended { // ascend out of the doc list
				d.descended = false
				m.refresh()
				return m, nil
			}
			if m.paletteActive() { // dismiss the command palette, keep the app
				m.input.Reset()
				m.paletteSel = 0
				m.refresh()
				return m, nil
			}
			return m, tea.Quit
		case "/":
			if m.focus == focusConvo {
				m.searching, m.searchActive, m.matches = true, false, nil
				m.search.Reset()
				m.search.Focus()
				m.refresh()
				return m, textinput.Blink
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		case "tab":
			// In the command palette, tab completes to the highlighted command.
			if m.paletteActive() {
				if ms := m.paletteMatches(); len(ms) > 0 {
					sel := m.paletteSel
					if sel < 0 || sel >= len(ms) {
						sel = 0
					}
					m.input.SetValue("/" + ms[sel].name + " ")
					m.input.CursorEnd()
					m.paletteSel = 0
					m.refresh()
				}
				return m, nil
			}
			// Toggle focus between the input and the conversation (tmux-style).
			if m.focus == focusInput {
				m.focus = focusConvo
				m.input.Blur()
				m.sel = len(m.convo) - 1 // start at the newest message
			} else {
				m.focus = focusInput
				m.input.Focus()
			}
			m.refresh()
			return m, textinput.Blink
		case "pgup", "pgdown":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		case "enter":
			if m.focus == focusConvo {
				// Inside a doc list: enter expands/collapses the current document.
				if d := m.selDocs(); d != nil && d.descended {
					if d.cur >= 0 && d.cur < len(d.docs) {
						d.docs[d.cur].open = !d.docs[d.cur].open
						m.refresh()
					}
					return m, nil
				}
				// A tool call opens the scrollable/searchable inspector overlay.
				if m.sel >= 0 && m.sel < len(m.convo) && m.convo[m.sel].tool != nil {
					tb := m.convo[m.sel].tool
					m.insp = newInspector(tb.name, tb.input, tb.output)
					m.insp.setSize(m.w, m.h)
					m.inspecting = true
					return m, nil
				}
				// Otherwise open/close the focused block (tool output or docs summary).
				if m.sel >= 0 && m.sel < len(m.convo) && m.convo[m.sel].expandable() {
					m.convo[m.sel].open = !m.convo[m.sel].open
					if !m.convo[m.sel].open && m.convo[m.sel].docs != nil {
						m.convo[m.sel].docs.descended = false // closing collapses the descent
					}
					m.refresh()
				}
				return m, nil
			}
			v := strings.TrimSpace(m.input.Value())
			// Local slash commands (never sent to the engine): run the palette's
			// highlighted match (or the exactly-typed command).
			if strings.HasPrefix(v, "/") {
				return m, m.runPaletteEnter(v)
			}
			if v == "" || m.busy || m.starting {
				return m, nil
			}
			m.history = append(m.history, v)
			m.histIdx = len(m.history)
			m.input.Reset()
			m.append(stUser.Render("› " + v))
			m.busy = true
			m.proc.send(v)
			return m, nil
		case "up":
			if m.focus == focusConvo {
				if d := m.selDocs(); d != nil && d.descended { // move within the doc list
					if d.cur > 0 {
						d.cur--
					}
					m.refresh()
					return m, nil
				}
				if m.searchActive && len(m.matches) > 0 { // step to the previous match
					if m.matchPos > 0 {
						m.matchPos--
					}
					m.sel = m.matches[m.matchPos]
				} else if top, h := m.selGeom(); m.vp.Height > 0 && h > m.vp.Height && top < m.vp.YOffset {
					m.vp.SetYOffset(m.vp.YOffset - 1) // scroll up within a tall message first
					return m, nil
				} else if m.sel > 0 {
					m.sel--
				}
				m.refresh()
				return m, nil
			}
			if m.paletteActive() { // move up in the command palette
				if m.paletteSel > 0 {
					m.paletteSel--
					m.refresh()
				}
				return m, nil
			}
			if len(m.history) > 0 && m.histIdx > 0 {
				m.histIdx--
				m.input.SetValue(m.history[m.histIdx])
				m.input.CursorEnd()
			}
			return m, nil
		case "down":
			if m.focus == focusConvo {
				if d := m.selDocs(); d != nil && d.descended { // move within the doc list
					if d.cur < len(d.docs)-1 {
						d.cur++
					}
					m.refresh()
					return m, nil
				}
				if m.searchActive && len(m.matches) > 0 { // step to the next match
					if m.matchPos < len(m.matches)-1 {
						m.matchPos++
					}
					m.sel = m.matches[m.matchPos]
				} else if top, h := m.selGeom(); m.vp.Height > 0 && h > m.vp.Height && top+h > m.vp.YOffset+m.vp.Height {
					m.vp.SetYOffset(m.vp.YOffset + 1) // scroll down within a tall message first
					return m, nil
				} else if m.sel < len(m.convo)-1 {
					m.sel++
				}
				m.refresh()
				return m, nil
			}
			if m.paletteActive() { // move down in the command palette
				if m.paletteSel < len(m.paletteMatches())-1 {
					m.paletteSel++
					m.refresh()
				}
				return m, nil
			}
			if m.histIdx < len(m.history) {
				m.histIdx++
				if m.histIdx == len(m.history) {
					m.input.SetValue("")
				} else {
					m.input.SetValue(m.history[m.histIdx])
					m.input.CursorEnd()
				}
			}
			return m, nil
		case "right":
			if m.focus == focusConvo { // descend into an expanded docs summary
				if m.sel >= 0 && m.sel < len(m.convo) {
					if e := m.convo[m.sel]; e.docs != nil && e.open && len(e.docs.docs) > 0 {
						e.docs.descended = true
						if e.docs.cur < 0 || e.docs.cur >= len(e.docs.docs) {
							e.docs.cur = 0
						}
						m.refresh()
					}
				}
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		case "left":
			if m.focus == focusConvo { // ascend out of the doc list
				if d := m.selDocs(); d != nil {
					d.descended = false
					m.refresh()
				}
				return m, nil
			}
			var lcmd tea.Cmd
			m.input, lcmd = m.input.Update(msg)
			return m, lcmd
		default:
			if m.focus == focusConvo { // keys don't type into a blurred input
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			m.paletteSel = 0 // typing re-filters the palette; start at the top
			m.refresh()
			return m, cmd
		}

	case dumpMsg:
		m.writeDump()
		return m, waitForDump(m.dumpSig) // re-arm for the next signal

	case evMsg:
		return m.handleEvent(msg), waitEvent(m.proc.ch)

	case eofMsg:
		if m.fatalErr == "" {
			m.fatalErr = "dun engine exited"
		}
		m.busy, m.starting = false, false
		m.refresh()
		return m, nil

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg) // wheel scrolls the conversation viewport
		return m, cmd

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

// updateAsking drives the answer picker: ↑/↓ highlight an option (the last row
// is a custom/chat free-text answer), enter selects, `n` attaches a detail to
// the highlighted option. While typing a detail or a custom answer, keys go to
// the input; esc backs out of that sub-mode.
func (m tuiModel) updateAsking(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	custom := len(m.askOptions) // index of the trailing "custom / chat" row
	// In multi mode a "✓ done" row follows the custom row (submits the checked
	// set) — so options toggle on enter without stealing space (a typed char).
	maxRow := custom
	if m.askMulti {
		maxRow = custom + 1
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.noting || m.customAnswer {
			m.noting, m.customAnswer = false, false
			m.input.Reset()
			m.input.Blur()
			m.refresh()
			return m, nil
		}
		return m, tea.Quit
	case "enter":
		switch {
		case m.noting:
			m.askNote = strings.TrimSpace(m.input.Value())
			m.noting = false
			m.input.Reset()
			m.input.Blur()
			m.refresh()
			return m, nil
		case m.customAnswer:
			v := strings.TrimSpace(m.input.Value())
			if v == "" {
				return m, nil
			}
			return m.sendAnswer(v), nil
		case m.askMulti && m.askSel < custom: // toggle the highlighted option
			m.askChecked[m.askSel] = !m.askChecked[m.askSel]
			m.refresh()
			return m, nil
		case m.askSel == custom: // open free-text / chat entry
			m.customAnswer = true
			m.input.Reset()
			m.input.Placeholder = "type your answer, or chat…"
			m.input.Focus()
			m.refresh()
			return m, textinput.Blink
		case m.askMulti: // the "✓ done" row: submit the checked set
			var picked []string
			for i, on := range m.askChecked {
				if on {
					picked = append(picked, m.askOptions[i])
				}
			}
			if len(picked) == 0 {
				return m, nil // nothing checked yet
			}
			return m.sendAnswer(strings.Join(picked, ", ")), nil
		default:
			ans := m.askOptions[m.askSel]
			if m.askNote != "" {
				ans += " — " + m.askNote
			}
			return m.sendAnswer(ans), nil
		}
	case "up":
		if !m.noting && !m.customAnswer && m.askSel > 0 {
			m.askSel--
			m.refresh()
			return m, nil
		}
	case "down":
		if !m.noting && !m.customAnswer && m.askSel < maxRow {
			m.askSel++
			m.refresh()
			return m, nil
		}
	case "n":
		if !m.noting && !m.customAnswer && !m.askMulti && m.askSel < custom {
			m.noting = true
			m.input.Reset()
			m.input.Placeholder = "add a detail…"
			m.input.Focus()
			m.refresh()
			return m, textinput.Blink
		}
	}
	if m.noting || m.customAnswer { // typing into the detail / custom field
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// sendAnswer resolves the ask: echoes the answer, ships it to the engine, and
// resets to the input pane.
func (m tuiModel) sendAnswer(v string) tuiModel {
	m.append(stUser.Render("› " + v))
	m.proc.answer(v)
	m.asking, m.noting, m.customAnswer = false, false, false
	m.askOptions, m.askSel, m.askNote = nil, 0, ""
	m.askMulti, m.askChecked = false, nil
	m.input.Reset()
	m.input.Placeholder = "ask dun to do something…"
	m.input.Focus()
	m.focus = focusInput
	m.refresh()
	return m
}

// updateSearch drives the "/" query box: keys type into it (matches recompute
// live and the selection jumps to the first hit), enter commits to match-scroll
// mode (↑/↓ step between hits), esc cancels.
func (m tuiModel) updateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.searching, m.searchActive, m.matches = false, false, nil
		m.search.Blur()
		m.refresh()
		return m, nil
	case "enter":
		m.searching = false
		m.search.Blur()
		m.matches = m.computeMatches()
		if len(m.matches) > 0 {
			m.searchActive = true
			m.matchPos = 0
			m.sel = m.matches[0]
		} else {
			m.searchActive = false
		}
		m.refresh()
		return m, nil
	default:
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		m.matches = m.computeMatches() // live: preview the first hit as you type
		if len(m.matches) > 0 {
			m.matchPos = 0
			m.sel = m.matches[0]
		}
		m.refresh()
		return m, cmd
	}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// computeMatches returns the convo indices whose text (collapsed AND full, so
// hidden tool output is searchable) contains the query, case-insensitively.
func (m tuiModel) computeMatches() []int {
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	if q == "" {
		return nil
	}
	var out []int
	for i, e := range m.convo {
		hay := strings.ToLower(stripANSI(e.collapsed + "\n" + e.full))
		if strings.Contains(hay, q) {
			out = append(out, i)
		}
	}
	return out
}

func (m tuiModel) handleEvent(ev evMsg) tuiModel {
	switch ev["type"] {
	case "workspace":
		m.branch = str(ev["branch"])
		m.append(stDim.Render("worktree branch: " + m.branch))
	case "ready":
		m.starting = false
		if ts, ok := ev["tools"].([]any); ok {
			for _, t := range ts {
				m.tools = append(m.tools, fmt.Sprint(t))
			}
		}
		m.append(stDim.Render(fmt.Sprintf("ready — %d tools: %s", len(m.tools), strings.Join(m.tools, ", "))))
	case "token":
		m.busy = true // a turn is active (incl. autonomous background-completion turns)
		m.cur += str(ev["text"])
		m.refresh()
	case "tool_call":
		m.busy = true
		m.flushCur()
		args, _ := ev["args"].(map[string]any)
		m.convo = append(m.convo, convoEntry{collapsed: stTool.Render("⚙ " + str(ev["tool"]) + "(" + argPreview(args, 80) + ")")})
		m.pendingTool = len(m.convo) - 1
		m.pendingArgs = args
		m.refresh()
	case "tool_result":
		tool := str(ev["tool"])
		// A per-tool renderer turns the result into a preview + full body.
		preview, body := renderToolResult(renderCtx{
			tool: tool, args: m.pendingArgs, result: str(ev["result"]), width: m.vp.Width,
		})
		// Collapsed shows a one-line arg preview; expanded shows the full input.
		callShort := stTool.Render("⚙ " + tool + "(" + argPreview(m.pendingArgs, 80) + ")")
		callFull := stTool.Render("⚙ " + tool)
		af := argFull(m.pendingArgs)
		if af != "" {
			callFull += "\n" + af
		}
		// The tool block feeds the inspector overlay: raw input + complete output.
		tb := &toolBlock{name: tool, input: af, output: body}
		if idx := m.pendingTool; idx >= 0 && idx < len(m.convo) {
			// Fold the result into its call so the pair is one collapsible unit.
			m.convo[idx] = convoEntry{
				collapsed: stDim.Render("▸ ") + callShort + "\n" + preview,
				full:      stDim.Render("▾ ") + callFull + "\n" + body,
				tool:      tb,
			}
			m.pendingTool, m.pendingArgs = -1, nil
			m.refresh()
		} else {
			m.convo = append(m.convo, convoEntry{
				collapsed: stDim.Render("▸ ") + preview,
				full:      stDim.Render("▾ ") + body,
				tool:      tb,
			})
			m.refresh()
		}
	case "message":
		// tokens already streamed the reply; nothing to add.
	case "notification":
		if str(ev["kind"]) == "docs" {
			m.convo = append(m.convo, convoEntry{docs: docsFromEvent(ev)})
			m.refresh()
		} else {
			m.append(stNote.Render("🔔 " + oneLine(str(ev["text"]))))
		}
	case "ask":
		m.flushCur()
		m.asking, m.noting, m.customAnswer = true, false, false
		m.askSel, m.askNote = 0, ""
		m.input.Blur()
		m.append(stAsk.Render("❓ " + str(ev["question"])))
		m.askOptions = nil
		if opts, ok := ev["options"].([]any); ok {
			for _, o := range opts {
				m.askOptions = append(m.askOptions, fmt.Sprint(o))
			}
		}
		m.askMulti, _ = ev["multi"].(bool)
		m.askChecked = make([]bool, len(m.askOptions))
		// No options → a pure free-text prompt: drop straight into text entry so
		// the user can just type. Otherwise typing is inert until enter opens the
		// "custom answer" row — surprising when there's nothing else to pick.
		if len(m.askOptions) == 0 {
			m.customAnswer = true
			m.input.Reset()
			m.input.Placeholder = "type your answer…"
			m.input.Focus()
		}
	case "done":
		m.flushCur()
		m.busy = false
		m.refresh()
	case "error":
		m.append(stErr.Render("error: " + str(ev["error"])))
		m.busy = false
	}
	return m
}

func (m tuiModel) View() string {
	// The inspector is a full-screen overlay — it replaces the normal layout.
	if m.inspecting {
		return m.insp.view(m.w, m.h)
	}
	head := stHeader.Render("dun") + stDim.Render("  "+m.workspace)
	if m.branch != "" {
		head += stDim.Render("  ⎇ " + m.branch)
	}
	if n := len(m.tools); n > 0 {
		head += stDim.Render(fmt.Sprintf("  · %d tools", n))
	}

	// The lower pane is the input, or — while answering — the option picker. The
	// convo pane takes whatever height is left (the picker can be several rows).
	lower := m.lowerView()
	convoH := m.h - 3 - lipgloss.Height(lower) // head 1 + divider 1 + status 1
	if convoH < 1 {
		convoH = 1
	}
	vp := m.vp
	vp.Height = convoH
	// Focus cue lives entirely in the divider's bright half — no pane borders.
	div := divider(m.w, m.focus == focusConvo && !m.asking)

	var status string
	switch {
	case m.fatalErr != "":
		status = stErr.Render(m.fatalErr)
	case m.starting:
		status = m.spin.View() + stDim.Render(" spawning tool servers…")
	case m.asking && len(m.askOptions) == 0:
		status = stAsk.Render("❓ type your answer · enter send · esc/ctrl+c quit")
	case m.asking && m.askMulti:
		status = stAsk.Render("❓ ↑/↓ move · enter toggle · ✓ done to submit · esc/ctrl+c quit")
	case m.asking:
		status = stAsk.Render("❓ ↑/↓ choose · enter select · n add detail · esc/ctrl+c quit")
	case m.searching:
		status = m.search.View() + stDim.Render(fmt.Sprintf("  (%d matches · enter to navigate)", len(m.matches)))
	case m.searchActive:
		status = stDim.Render(fmt.Sprintf("match %d/%d  ·  ↑/↓ prev/next · / new search · esc exit", m.matchPos+1, len(m.matches)))
	case m.paletteActive():
		status = stDim.Render("command  ·  ↑/↓ select · tab complete · enter run · esc/type to edit")
	case m.busy:
		status = m.spin.View() + stDim.Render(" working…  (ctrl+c to quit)")
	case m.focus == focusConvo:
		if d := m.selDocs(); d != nil && d.descended {
			status = stDim.Render("docs  ·  ↑/↓ doc · enter expand · ← back · ctrl+c quit")
		} else {
			status = stDim.Render("convo  ·  ↑/↓ select · → docs · / search · enter open (tool→inspector) · tab input")
		}
	default:
		status = stDim.Render("ready  ·  tab scroll · ↑/↓ history · ctrl+c quit")
	}
	return strings.Join([]string{head, vp.View(), div, lower, status}, "\n")
}

// lowerView is the bottom pane: the input line, or the answer picker when asking.
func (m tuiModel) lowerView() string {
	if m.asking {
		return m.askPanel()
	}
	if m.paletteActive() {
		return m.palettePanel()
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
		rows = append(rows, gut(label, sel(i)))
	}
	rows = append(rows, gut(stDim.Render("✎ custom answer / chat…"), sel(custom) || m.customAnswer))
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
		rows = append(rows, stDim.Render("   detail: "+m.askNote))
	}
	if m.noting || m.customAnswer {
		rows = append(rows, m.input.View())
	}
	return strings.Join(rows, "\n")
}

// ── helpers ────────────────────────────────────────────────────────

func (m *tuiModel) append(line string) {
	m.convo = append(m.convo, convoEntry{collapsed: line})
	m.refresh()
}

func (m *tuiModel) flushCur() {
	if m.cur != "" {
		// Finalize the streamed reply as rendered markdown (headers, lists, code).
		m.convo = append(m.convo, convoEntry{collapsed: renderMarkdown(m.md, strings.TrimRight(m.cur, "\n"))})
		m.cur = ""
	}
}

// selDocs returns the docsBlock of the selected entry, or nil.
func (m *tuiModel) selDocs() *docsBlock {
	if m.sel >= 0 && m.sel < len(m.convo) {
		return m.convo[m.sel].docs
	}
	return nil
}

// selGeom returns the top line offset and rendered height of the selected block
// (from the last refresh), for tall-message intra-scroll decisions.
func (m tuiModel) selGeom() (top, h int) {
	for i := 0; i < m.sel && i < len(m.blockH); i++ {
		top += m.blockH[i]
	}
	if m.sel >= 0 && m.sel < len(m.blockH) {
		h = m.blockH[m.sel]
	}
	return top, h
}

func intOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

func floatOf(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// docsFromEvent builds a docsBlock from a `notification` event of kind "docs".
func docsFromEvent(ev evMsg) *docsBlock {
	d := &docsBlock{found: intOf(ev["found"]), surfaced: intOf(ev["surfaced"])}
	if arr, ok := ev["docs"].([]any); ok {
		for _, it := range arr {
			dm, _ := it.(map[string]any)
			d.docs = append(d.docs, docNode{title: str(dm["title"]), line: str(dm["line"]), score: floatOf(dm["score"])})
		}
	}
	return d
}

// convoText joins the visible text of every block (test/inspection helper).
func (m tuiModel) convoText() string {
	parts := make([]string, len(m.convo))
	for i, e := range m.convo {
		parts[i] = e.view()
	}
	return strings.Join(parts, "\n")
}

func (m *tuiModel) refresh() {
	blocks := make([]string, 0, len(m.convo)+1)
	for _, e := range m.convo {
		blocks = append(blocks, e.view())
	}
	if m.cur != "" {
		blocks = append(blocks, m.cur)
	}
	// In convo focus, gutter every block (selected one bright) so the highlight
	// aligns and the selected message shows a left border down its full height.
	selMode := m.focus == focusConvo && !m.asking && len(blocks) > 0
	width := m.vp.Width
	if selMode {
		width-- // reserve ONE column for the gutter bar — minimal reflow on focus
	}
	wrap := lipgloss.NewStyle().Width(max(1, width))
	rendered := make([]string, len(blocks))
	m.blockH = make([]int, len(m.convo)) // cache convo-block heights for scroll math
	for i, b := range blocks {
		w := wrap.Render(b)
		if selMode {
			if i == m.sel {
				w = addGutter(w, "▎", stSel)
			} else {
				w = addGutter(w, " ", lipgloss.NewStyle())
			}
		}
		rendered[i] = w
		if i < len(m.blockH) {
			m.blockH[i] = lipgloss.Height(w)
		}
	}
	m.vp.SetContent(strings.Join(rendered, "\n"))

	// Keep the selection in view; otherwise stick to the bottom (live tail).
	if selMode && m.sel >= 0 && m.sel < len(rendered) {
		top := 0
		for i := 0; i < m.sel; i++ {
			top += lipgloss.Height(rendered[i])
		}
		h := lipgloss.Height(rendered[m.sel])
		vh := m.vp.Height
		if h >= vh {
			// Taller than the window: don't fight intra-message scroll — only snap
			// when the selection is entirely off-screen. ↑/↓ scroll within it.
			switch {
			case top >= m.vp.YOffset+vh: // fully below the fold
				m.vp.SetYOffset(top)
			case top+h <= m.vp.YOffset: // fully above the fold
				m.vp.SetYOffset(top + h - vh)
			}
		} else {
			switch {
			case top < m.vp.YOffset:
				m.vp.SetYOffset(top)
			case top+h > m.vp.YOffset+vh:
				m.vp.SetYOffset(top + h - vh)
			}
		}
	} else {
		m.vp.GotoBottom()
	}
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// argPreview is a one-line `key=value` summary of a call's args (each value
// clipped), for the collapsed call line — so you SEE the input, not just keys.
func argPreview(args map[string]any, max int) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args))
	for _, k := range sortedKeys(args) {
		parts = append(parts, k+"="+clip(oneLine(fmt.Sprint(args[k])), 48))
	}
	return clip(strings.Join(parts, ", "), max)
}

// argFull renders the call's args in full (multi-line values kept intact), shown
// when the tool block is expanded.
func argFull(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	var b strings.Builder
	for _, k := range sortedKeys(args) {
		b.WriteString(stDim.Render("  "+k+":") + " " + fmt.Sprint(args[k]) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ── slash commands ─────────────────────────────────────────────────
//
// Input beginning with "/" is a local command (never sent to the engine).
// Typing "/" opens a live PALETTE (see palettePanel) listing matching commands
// with descriptions; ↑/↓ selects, tab completes, enter runs. New commands go in
// slashCommands — the palette + /help pick them up automatically.

type slashCmd struct {
	name, args, desc string
	run              func(m *tuiModel, args []string) tea.Cmd
}

// Populated in init() (not a var literal) — the help handler reads
// slashCommands, which would be a static initialization cycle otherwise.
var slashCommands []slashCmd

func init() {
	slashCommands = []slashCmd{
		{"help", "", "list these commands", func(m *tuiModel, _ []string) tea.Cmd { m.showHelp(); return nil }},
		{"web", "[addr]", "serve this live session to a browser (default 0.0.0.0:8734)", func(m *tuiModel, a []string) tea.Cmd {
			addr := "0.0.0.0:8734"
			if len(a) > 0 {
				addr = a[0]
			}
			m.startWeb(addr)
			return nil
		}},
		{"quit", "", "exit dun", func(_ *tuiModel, _ []string) tea.Cmd { return tea.Quit }},
	}
}

// paletteActive reports whether the "/" command palette should be shown/driven.
func (m tuiModel) paletteActive() bool {
	return m.focus == focusInput && !m.asking && strings.HasPrefix(m.input.Value(), "/")
}

// paletteMatches returns the commands whose name starts with the typed word.
func (m tuiModel) paletteMatches() []slashCmd {
	word := strings.TrimPrefix(strings.Fields(m.input.Value()+" ")[0], "/")
	var out []slashCmd
	for _, c := range slashCommands {
		if strings.HasPrefix(c.name, word) {
			out = append(out, c)
		}
	}
	return out
}

// runSlash dispatches "/name args…" by exact name or a unique prefix.
func (m *tuiModel) runSlash(v string) tea.Cmd {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return nil
	}
	name := strings.TrimPrefix(fields[0], "/")
	var hit *slashCmd
	n := 0
	for i := range slashCommands {
		if slashCommands[i].name == name { // exact wins outright
			return slashCommands[i].run(m, fields[1:])
		}
		if strings.HasPrefix(slashCommands[i].name, name) {
			hit = &slashCommands[i]
			n++
		}
	}
	if n == 1 {
		return hit.run(m, fields[1:])
	}
	m.append(stDim.Render("unknown command: /" + name + " — try /help"))
	return nil
}

// runPaletteEnter runs the highlighted palette command (preserving any typed
// args), or the exactly-typed command.
func (m *tuiModel) runPaletteEnter(v string) tea.Cmd {
	fields := strings.Fields(v)
	matches := m.paletteMatches()
	sel := m.paletteSel
	m.input.Reset()
	m.paletteSel = 0
	if sel < 0 || sel >= len(matches) {
		sel = 0
	}
	// If the first word is already an exact command, honor it (with args).
	if len(fields) > 0 {
		if word := strings.TrimPrefix(fields[0], "/"); commandNamed(word) {
			return m.runSlash(v)
		}
	}
	if len(matches) == 0 {
		m.append(stDim.Render("unknown command: " + v + " — try /help"))
		return nil
	}
	args := ""
	if len(fields) > 1 {
		args = " " + strings.Join(fields[1:], " ")
	}
	return m.runSlash("/" + matches[sel].name + args)
}

func commandNamed(name string) bool {
	for _, c := range slashCommands {
		if c.name == name {
			return true
		}
	}
	return false
}

// showHelp appends the command list to the conversation.
func (m *tuiModel) showHelp() {
	var b strings.Builder
	b.WriteString(stHeader.Render("commands"))
	for _, c := range slashCommands {
		usage := "/" + c.name
		if c.args != "" {
			usage += " " + c.args
		}
		b.WriteString("\n  " + stTool.Render(usage) + "  " + stDim.Render(c.desc))
	}
	m.append(b.String())
}

// palettePanel renders the live command list above the input (like the ask
// picker), the highlighted row gutter-marked.
func (m tuiModel) palettePanel() string {
	matches := m.paletteMatches()
	rows := make([]string, 0, len(matches)+1)
	for i, c := range matches {
		line := stTool.Render("/" + c.name)
		if c.args != "" {
			line += " " + stDim.Render(c.args)
		}
		line += "  " + stDim.Render(c.desc)
		if i == m.paletteSel {
			rows = append(rows, addGutter(line, "▎ ", stSel))
		} else {
			rows = append(rows, addGutter(line, "  ", lipgloss.NewStyle()))
		}
	}
	if len(matches) == 0 {
		rows = append(rows, stDim.Render("  no matching command · /help"))
	}
	return strings.Join(rows, "\n") + "\n" + m.input.View()
}

// startWeb attaches an embedded web server to the LIVE session: it mirrors the
// engine's event stream to browsers (proc.tap → hub.broadcast) and forwards
// their input to the same engine stdin the TUI writes to. So a browser on
// another host watches and drives exactly what the TUI is doing.
func (m *tuiModel) startWeb(addr string) {
	if m.webAddr != "" {
		m.append(stNote.Render("🌐 web already serving · " + m.webAddr))
		return
	}
	lw := &lockedWriter{mu: &m.proc.mu, w: m.proc.stdin}
	hub, bound, err := startEmbeddedWeb(addr, lw)
	if err != nil {
		m.append(stErr.Render("web: " + err.Error()))
		return
	}
	m.proc.setTap(hub.broadcast)
	m.webAddr = bound
	for _, u := range reachableURLs(bound) {
		m.append(stNote.Render("🌐 " + u))
	}
	m.append(stDim.Render("⚠ no auth — anyone who can reach that address drives this session"))
}

// ── screen dump (SIGUSR1) ──────────────────────────────────────────

// dumpMsg is delivered when SIGUSR1 arrives; Update writes the current screen.
type dumpMsg struct{}

// waitForDump blocks on the signal channel and turns SIGUSR1 into a dumpMsg,
// re-armed after each dump so the TUI can be snapshotted repeatedly.
func waitForDump(sig chan os.Signal) tea.Cmd {
	return func() tea.Msg {
		<-sig
		return dumpMsg{}
	}
}

// dumpPath is where a screen dump is appended: $DUN_DUMP_FILE or a temp default.
func dumpPath() string {
	if p := os.Getenv("DUN_DUMP_FILE"); p != "" {
		return p
	}
	return filepath.Join(os.TempDir(), "dun-screen.txt")
}

// dumpString renders the current screen (ANSI stripped) plus a state header —
// what the TUI is showing and the mode flags behind it.
func (m tuiModel) dumpString() string {
	var b strings.Builder
	fmt.Fprintf(&b, "═══ dun screen @ %s ═══\n", time.Now().Format("15:04:05.000"))
	fmt.Fprintf(&b, "focus=%d busy=%v starting=%v asking=%v(multi=%v) inspecting=%v searching=%v sel=%d convo=%d w=%d h=%d\n",
		m.focus, m.busy, m.starting, m.asking, m.askMulti, m.inspecting, m.searching, m.sel, len(m.convo), m.w, m.h)
	if m.cur != "" {
		fmt.Fprintf(&b, "streaming: %q\n", clip(oneLine(m.cur), 200))
	}
	b.WriteString("───\n")
	b.WriteString(stripANSI(m.View()))
	b.WriteString("\n\n")
	return b.String()
}

// writeDump appends the current screen dump to dumpPath (best-effort; a debug
// aid must never disturb the UI, so errors are swallowed).
func (m tuiModel) writeDump() {
	f, err := os.OpenFile(dumpPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(m.dumpString())
}

// ── subprocess (dun -p) ────────────────────────────────────────────

type evMsg map[string]any
type eofMsg struct{}

type dunProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	ch    chan tea.Msg
	mu    sync.Mutex     // serializes stdin writes (TUI + embedded /web hub)
	tap   func(string)   // if set, every raw engine event line is mirrored here (/web)
}

func (p *dunProc) setTap(f func(string)) {
	p.mu.Lock()
	p.tap = f
	p.mu.Unlock()
}

// procArgs builds a `dun <mode>` argv from the shared flags. mode is "-p" (the
// engine, for the TUI + `dun serve` web-native bridge) or "-tui" (the full TUI,
// for the xterm/PTY terminal view served at /term).
func procArgs(o tuiOpts, mode string) []string {
	args := []string{mode, "--workspace", o.workspace}
	if o.model != "" {
		args = append(args, "--model", o.model)
	}
	if o.url != "" {
		args = append(args, "--url", o.url)
	}
	if o.key != "" {
		args = append(args, "--key", o.key)
	}
	if o.docker != "" {
		args = append(args, "--docker", o.docker)
	}
	if o.noWorktree {
		args = append(args, "--no-worktree")
	}
	if o.pr {
		args = append(args, "--pr")
	}
	if o.cont {
		args = append(args, "--continue")
	}
	if o.resume != "" {
		args = append(args, "--resume", o.resume)
	}
	return args
}

func startDunProc(o tuiOpts) (*dunProc, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, procArgs(o, "-p")...)
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Engine stderr (mcp startup logs) → a temp log so it doesn't corrupt the UI.
	if f, err := os.CreateTemp("", "dun-tui-*.log"); err == nil {
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := make(chan tea.Msg, 256)
	p := &dunProc{cmd: cmd, stdin: stdin, ch: ch}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			// Mirror the raw event to the embedded web hub (/web), if attached.
			p.mu.Lock()
			tap := p.tap
			p.mu.Unlock()
			if tap != nil {
				tap(line)
			}
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) == nil {
				ch <- evMsg(ev)
			}
		}
		ch <- eofMsg{}
	}()
	return p, nil
}

func (p *dunProc) send(content string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = json.NewEncoder(p.stdin).Encode(map[string]string{"type": "user", "content": content})
}

func (p *dunProc) answer(value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = json.NewEncoder(p.stdin).Encode(map[string]string{"type": "answer", "value": value})
}

func (p *dunProc) close() {
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// waitEvent blocks for the next engine event and delivers it as a tea.Msg.
func waitEvent(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}
