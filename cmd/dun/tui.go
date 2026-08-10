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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/cellbuf"

	"github.com/iodesystems/dun"
)

// The TUI is a CLIENT of the `-p` JSON event protocol: it re-execs `dun -p`,
// writes user events to its stdin, and renders the event stream. The engine
// stays headless; the UI is pure presentation.

// tuiOpts are the flags the TUI forwards to its `dun -p` subprocess.
type tuiOpts struct {
	workspace, model, url, key, docker string
	dockerNetwork                      bool
	worktree                           bool
	pr                                 bool
	ship                               bool
	cont                               bool   // --continue: resume the latest session
	resume                             string // --resume <id>: resume a specific session
	disableExit                        bool   // --disable-exit: ctrl+c/esc don't quit (use /exit)
	suggest                            bool   // --no-suggest=false: engine emits suggestions after each turn
	// rag/lsp are --rag/--lsp as typed: "" (unset, use the saved setting),
	// "true" or "false". Passed through to the -p engine verbatim.
	rag, lsp string
}

// runTUI launches the Bubble Tea app against a re-exec'd `dun -p` subprocess.
// lc (may be nil) is the launcher registration — its reload channel drives the
// "update ready" indicator + /reload.
func runTUI(o tuiOpts, lc *launcherConn) error {
	// Before the program starts, so the terminal query cannot block the input
	// loop or race the key reader for the reply bytes. See initMarkdownStyle.
	initMarkdownStyle()
	loadScriptRenderers() // ~/.dun/renderers/*.star override/extend the built-ins
	// A first engine that will not spawn is NOT a reason to refuse to open. The
	// UI is where the error is legible and where /reconnect lives; exiting to a
	// bare shell message just moves the same failure somewhere with fewer
	// options. Init retries from inside.
	proc, startErr := startDunProc(o)
	m := newTUIModel(proc, o.workspace)
	m.model, m.url, m.keySet = o.model, o.url, o.key != "" // for /config
	m.disableExit = o.disableExit
	m.lc = lc
	m.opts = o // for respawning the engine (see eofMsg)
	if startErr != nil {
		m.fatalErr = "engine did not start: " + startErr.Error()
		m.starting = false
	}
	// WithMouseCellMotion makes the terminal (and tmux) forward wheel events to
	// us instead of scrolling its own scrollback; the viewport consumes them.
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithMouseCellMotion()}
	// The shift+enter filter (keyfilter.go) has to see the bytes before
	// bubbletea's parser reduces the sequence to the letter M — so it goes in as
	// the input reader. bubbletea only puts the terminal into RAW mode when its
	// input is a tty *os.File, and a wrapped reader is neither, so raw mode
	// becomes ours: without it the tty stays canonical and nothing arrives until
	// the user presses enter. Restored by the deferred call, including on panic.
	if restore, ok := rawMode(os.Stdin); ok {
		defer restore()
		opts = append(opts, tea.WithInput(newKeyFilter(os.Stdin)))
	}
	fm, err := tea.NewProgram(m, opts...).Run()
	if tm, ok := fm.(tuiModel); ok {
		tm.proc.close() // the engine it ENDED with, which may not be the one it started
	} else {
		proc.close()
	}
	// /reload: bubbletea has restored the terminal by now, so re-exec cleanly
	// into the (launcher-rebuilt) binary with the same args, preserving the
	// session so the conversation is replayed on reconnect.
	if tm, ok := fm.(tuiModel); ok && tm.reloadReq && err == nil {
		lc.close() // drop the old registration; the new process re-registers
		exe, e := os.Executable()
		if e == nil {
			args := os.Args
			// Append --resume so the new process reattaches to the same session.
			// Without it, /reload starts a fresh session and the conversation is lost.
			if tm.sessionID != "" {
				args = append(args, "--resume", tm.sessionID)
			} else if tm.opts.cont {
				args = append(args, "--continue")
			}
			_ = syscall.Exec(exe, args, os.Environ())
		}
	}
	return err
}

// ── styles ─────────────────────────────────────────────────────────

var (
	stHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	stDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	stUser   = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("236")).Foreground(lipgloss.Color("144"))
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
// them: minimized (one-line preview) → expanded (styled renderer output) →
// raw (unstyled tool output, for reading verbatim or copying).
type viewState int

const (
	viewMinimized viewState = iota // collapsed one-line preview
	viewExpanded                   // styled renderer output
	viewRaw                        // unstyled raw tool output
)

func (s viewState) Next() viewState {
	switch s {
	case viewMinimized:
		return viewExpanded
	case viewExpanded:
		return viewRaw
	default:
		return viewMinimized
	}
}

// convoEntry is one conversation block. A tool call/result is collapsible: full
// holds the styled renderer output, collapsed a one-line preview, raw the
// unstyled tool output. Enter (when focused) cycles through the three states.
// A relevant-docs summary carries a docsBlock (nested navigation).
// A plain block has neither full nor docs.
type convoEntry struct {
	collapsed string
	full      string
	raw       string       // unstyled tool output (empty for non-tool blocks)
	state     viewState    // minimized | expanded | raw
	docs      *docsBlock   // proactive-RAG summary (nil for normal blocks)
	tool      *toolBlock   // tool call/result (nil for normal blocks) → enter opens the inspector
	// provisional marks a message that was typed mid-turn and is queued for
	// delivery. It appears in the conversation with a dimmed style so the user
	// knows it landed but hasn't been delivered to the model yet. When the turn
	// ends and the messages are delivered, they lose the provisional marker and
	// render as normal user messages.
	provisional      bool
	provisionalText  string // original text, used to restore normal style on delivery
	userText         string // raw user message text (non-empty for user messages only)

	// Wrapped render, cached. A finalized block's text never changes, but
	// refresh() runs once per STREAMED TOKEN, so re-wrapping the whole
	// scrollback every frame made the cost of a reply quadratic in the
	// conversation: measured at 7.8ms per frame with 200 blocks, ~1s of CPU for
	// one 100-token reply — on the goroutine that also reads the keyboard,
	// which is why keystrokes went missing. Invalidated by width (a resize) and
	// by state (expand/collapse/raw); docs blocks opt out, their inner per-doc
	// state is not captured here.
	wrapped   string
	wrapW     int
	wrapState viewState

	// wrapMax is the widest line of the UNWRAPPED block, measured once per
	// source rather than per width. Any terminal at least this wide needs no
	// wrapping at all, so a resize down to it costs nothing. That matters
	// because wrapping is what a width change actually pays for: on a
	// resume-sized conversation, measuring every block is 14ms and wrapping
	// every block is 73ms, and markdown that was rendered to fit the last
	// width usually still fits the next one.
	wrapMax   int
	wrapMaxOK bool

	// rowOffset is the viewport row (line index) where this entry starts.
	// Set by refresh() as a cumulative sum of block heights. Used by
	// scrollOverlay to map vp.YOffset back to conversation entries without
	// re-rendering.
	rowOffset int
}

// invalidateWrap drops everything refresh() cached about how this entry
// renders. Call it wherever the entry's CONTENT changes — the width and state
// keys cannot see that, and a stale wrapMax would let a block skip the wrap it
// needs and overflow the pane.
func (e *convoEntry) invalidateWrap() {
	e.wrapped, e.wrapW = "", 0
	e.wrapMax, e.wrapMaxOK = 0, false
}

func (e convoEntry) expandable() bool { return (e.full != "" || e.raw != "") || e.docs != nil }

// deeper is the next view state that actually shows something NEW: minimized →
// expanded → raw, skipping any level this block does not have and stopping at
// the deepest one it does.
//
// Next() wraps, which is right for enter (a cycle) and wrong for → (a
// direction): wrapping would make → close the block it had just opened, and it
// would land on viewRaw for a block with no raw, where view() silently falls
// back to the collapsed line — pressing "deeper" and getting the one-liner back.
func (e convoEntry) deeper() (viewState, bool) {
	switch e.state {
	case viewMinimized:
		if e.full != "" || e.docs != nil {
			return viewExpanded, true
		}
		if e.raw != "" {
			return viewRaw, true
		}
	case viewExpanded:
		if e.raw != "" {
			return viewRaw, true
		}
	}
	return e.state, false
}

// shallower is the inverse, for ←: one level out, stopping at minimized.
func (e convoEntry) shallower() (viewState, bool) {
	switch e.state {
	case viewRaw:
		if e.full != "" || e.docs != nil {
			return viewExpanded, true
		}
		return viewMinimized, true
	case viewExpanded:
		return viewMinimized, true
	}
	return e.state, false
}

// contextStats accumulates context usage data across the session for /context.
type contextStats struct {
	// Token usage (cumulative across all turns)
	totalTokens     int
	activeTokens    int  // last reported active window size
	cachedTokens    int
	processedTokens int
	generatedTokens int
	turns           int

	// Compaction history
	compactions     int
	tokensCompacted int // total tokens saved by compaction

	// Recap history
	recaps          int
	entriesRecapped int
	charsRecapped   int

	// Tool result truncation (LOD)
	toolResults      int  // total tool results seen
	resultsTruncated int  // how many were LOD-truncated

	// System prompt + tool schemas (estimated from engine)
	systemTokens int

	// Out-of-band delivery: messages typed mid-turn that were queued into
	// a running turn rather than starting a new one.
	oobMessages int

	// Notifications delivered via the queue (smuggled into tool results
	// rather than triggering their own turn).
	notificationsSmuggled int

	// Tool calls injected by the host (e.g. /ship) that appear as if the
	// model made them.
	forcedToolCalls int
}

// toolBlock carries a tool call's raw input + complete output so enter can open
// the scrollable/searchable inspector overlay (inspector.go), separate from the
// inline collapsed/full preview.
type toolBlock struct {
	name   string
	input  string
	output string // styled renderer output
	raw    string // original unstyled tool output
}

func (e convoEntry) view() string {
	if e.docs != nil {
		return e.docs.render(e.state > viewMinimized)
	}
	switch e.state {
	case viewExpanded:
		if e.full != "" {
			return e.full
		}
	case viewRaw:
		if e.raw != "" {
			return e.raw
		}
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
	proc        *dunProc
	workspace   string
	vp          convoPane
	input       multilineInput
	spin        spinner.Model
	convo       []convoEntry   // finalized conversation blocks
	pendingTool int            // index of a tool call awaiting its result; -1 = none
	pendingArgs map[string]any // args of the pending tool call (for its renderer)
	cur         string         // streaming assistant text (not yet finalized); string, not
	//                    strings.Builder — Bubble Tea copies the model each Update.
	tools         []string
	branch        string                // worktree branch (from the `workspace` event)
	starting      bool                  // spawning servers, before `ready`
	startingStart time.Time             // when the starting phase began
	busy          bool                  // a turn in flight
	busyStart     time.Time             // when the current busy turn started
	asking        bool                  // agent is waiting on an ask_user answer
	askOptions    []string              // the offered options; a trailing "custom" row is implicit
	askSel        int                   // highlighted answer row (== len(askOptions) → the custom row)
	askNote       string                // optional detail attached to the chosen option ("n")
	askMulti      bool                  // multi-select: space toggles, enter submits the checked set
	askChecked    []bool                // per-option checked state (multi mode; len == len(askOptions))
	noting        bool                  // capturing a detail for the selected option
	customAnswer  bool                  // capturing a free-text / chat answer
	md            *glamour.TermRenderer // markdown renderer for assistant replies
	mdWidth       int                   // width md was built for (rebuild only on change)
	history       []string              // sent inputs, for up/down recall
	histIdx       int                   // cursor into history (== len when not browsing)
	focus         int                   // focusInput | focusConvo | focusActivity
	task          string                // the last user message, for the task line
	agents        []agentRow            // live sub-agents (activity.go)
	jobs          []jobRow              // live background jobs (activity.go)
	actLevel      int                   // actCollapsed | actList — → descends, ← ascends
	actSel        actKey                // selected activity row, by identity not index
	// Agent scope: which child's conversation is on screen (0 = the root's),
	// and the root's own convo/task stashed while it is.
	scopeAgent   int
	rootConvo    []convoEntry
	rootTask     string
	sel          int             // selected message index (convo focus); -1 = none
	search       textinput.Model // vim-style "/" message search box
	searching    bool            // typing a search query
	searchActive bool            // navigating matches (↑/↓ step, esc exits)
	matches      []int           // convo indices matching the query
	matchPos     int             // cursor into matches
	blockH       []int           // rendered height of each convo block (for tall-message scroll)
	inspecting   bool            // the tool inspector overlay is open (owns all keys)
	insp         inspector       // the overlay (valid while inspecting)
	dumpSig      chan os.Signal  // SIGUSR1 → dump the rendered screen to a debug file
	paletteSel   int             // highlighted row in the "/" command palette
	model, url   string          // this session's LLM settings (for /config)
	keySet       bool            // whether an API key is configured
	disableExit  bool            // --disable-exit: ctrl+c/esc don't quit (use /exit)
	lc           *launcherConn   // launcher registration (nil = no launcher)
	reloadVer    string          // a newer build the launcher announced ("" = none)
	reloadReq    bool            // /reload requested → runTUI re-execs after quit
	// Engine supervision: the TUI outlives its engine. opts is what respawning
	// one takes, sessionID reattaches it to the same conversation, and the
	// restart counters stop a crash loop from spinning forever.
	opts         tuiOpts
	sessionID    string
	restarts     int
	restartStart time.Time
	skipHistory  bool            // a respawned engine replays what is already on screen
	wantServers  map[string]bool // /rag, /lsp the user turned on — reapplied after a restart
	// The viewport's rendered text, cached behind a POINTER on purpose.
	//
	// Bubble Tea calls View() after EVERY message — including each streamed
	// token — and bubbles' viewport re-slices and re-joins its window on every
	// call: 191µs of the 315µs a View costs at 200 blocks, paid per token no
	// matter how the redraws are paced. But View() has a value receiver, so a
	// cache stored in the model is written to a COPY and thrown away; only a
	// pointer field survives to the next call. Its output depends on exactly
	// (content, offset, size), which is the whole key; contentGen is bumped by
	// refresh(), the only place content is set.
	vpc        *viewportCache
	contentGen uint64

	renderDue        bool              // streamed text arrived; a tick will draw it
	tickPending      bool              // a render tick is already scheduled
	picking          bool              // the session picker owns the keys
	sessions         []dun.SessionInfo // what it is listing
	pickSel          int               // highlighted session
	// Model picker (/model): fetch available models, let the user switch.
	modelPicking     bool              // the model picker owns the keys
	modelList        []string          // fetched model ids
	modelSel         int               // highlighted model
	modelPersist     bool              // save to config.json (checkbox)
	modelFetching    bool              // still fetching from the API
	replaying        bool              // driven by a trace (--replay), not an engine
	quitting         bool              // the user is leaving; do not respawn
	exitAnnounced    bool              // the engine said it was going; it did not crash
	everUp           bool              // an engine reached `session` once; a retry may reattach
	suggestions      []suggestion      // predicted next messages (idle-only picker)
	suggestMode      string            // "on" | "off" | "auto" — /suggest controls this
	suggestSel       int               // highlighted suggestion in the selector
	// Idle debounce for the suggestion request. The engine cannot see whether
	// anyone is typing, so the decision lives here: the clock restarts on every
	// keystroke and at the end of every turn, and the request goes out only once
	// the person has actually stopped. See idleSuggestDelay.
	lastKeyAt         time.Time // last keystroke OR turn end — the idle clock
	idleTickPending   bool      // a debounce tick is already scheduled
	idleWantTick      bool      // an event asked for one (handleEvent has no tea.Cmd)
	suggestedThisIdle bool      // already asked during this idle; do not ask twice
	retry            string            // live retry banner ("" = not waiting on the provider)
	retryDue         time.Time         // when the next attempt is due, for the countdown
	retrySeen        int               // retries this outage; the first one also lands in scrollback
	queuedMsgs       int               // messages typed mid-turn, buffered for the running turn
	w, h             int
	fatalErr         string
	scrollPinned     bool // true when viewport should auto-follow (at bottom)
	traceFile        *os.File  // /trace on: recording events+scroll to this file
	tracePrevYOff    int       // last recorded YOffset (avoid duplicates)
	// Context stats (for /context)
	ctxStats contextStats
}

func newTUIModel(proc *dunProc, workspace string) tuiModel {
	in := newMultilineInput()
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	se := textinput.New()
	se.Prompt = "/"
	se.Placeholder = "search messages…"
	// SIGUSR1 → dump the rendered screen (see dumpMsg): the alt-screen hides what
	// the TUI is showing, so an out-of-band `kill -USR1 <pid>` snapshots it.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	return tuiModel{proc: proc, workspace: workspace, vpc: &viewportCache{}, input: in, search: se, spin: sp, dumpSig: sig, starting: true, sel: -1, pendingTool: -1, scrollPinned: true, suggestMode: "auto"}
}

func (m tuiModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick, m.input.BlinkTick(), waitForDump(m.dumpSig), waitReload(m.lc)}
	if m.proc != nil {
		cmds = append(cmds, waitEvent(m.proc.ch))
	} else {
		// The first engine never started. Come up anyway and keep trying —
		// a TUI that refuses to open cannot even show you the error.
		cmds = append(cmds, restartEngine(m.opts, "", false))
	}
	return tea.Batch(cmds...)
}

// reloadMsg carries a newer build version the launcher announced.
type reloadMsg string

// waitReload blocks on the launcher's reload channel (nil-safe) and turns a
// "new build" push into a reloadMsg. Re-armed after each one.
func waitReload(lc *launcherConn) tea.Cmd {
	if lc == nil {
		return nil
	}
	return func() tea.Msg {
		v, ok := <-lc.reload
		if !ok {
			return nil
		}
		return reloadMsg(v)
	}
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Timed as a whole: this is what a keystroke queues behind, and it is the
	// number that decides whether dun warns about itself (see perf.go).
	start := time.Now()
	nm, cmd := m.update(msg)
	if frames.observeMsg(stageUpdate, time.Since(start), msgKind(msg)) {
		// Noticed its own stutter. Say it once, in the conversation, where the
		// person having the problem is actually looking.
		if t, ok := nm.(tuiModel); ok {
			t.append(stErr.Render(frames.slowWarning()))
			nm = t
		}
	}
	// Every keystroke restarts the idle clock. Hooked HERE rather than in the
	// key switch because that switch returns from a dozen places and each mode
	// (asking, searching, the inspector) owns its own keys — this is the one
	// funnel every key actually passes through.
	if t, ok := nm.(tuiModel); ok {
		if _, isKey := msg.(tea.KeyMsg); isKey {
			t.armIdleSuggest()
		}
		if t.idleWantTick && !t.idleTickPending {
			t.idleWantTick, t.idleTickPending = false, true
			cmd = tea.Batch(cmd, idleSuggestTick(idleSuggestDelay))
		}
		t.idleWantTick = false
		// Leave Update with the viewport's height equal to the height View is
		// about to draw it at. AtBottom, GotoBottom and YOffset clamping all
		// measure against m.vp.Height, so a value left over from a frame that
		// had no task line (or a one-line input) scrolls against a window taller
		// than the real one and hides the bottom row. Hooked here for the same
		// reason armIdleSuggest is: update() returns from a dozen places.
		//
		// A changed height is also a reason to re-run the scroll policy. refresh()
		// runs it while handling the message, using the height the PREVIOUS frame
		// had — so the very message that adds the task line (the first send, a
		// resume) left a pinned viewport one row short of the bottom, hiding the
		// newest line until something else happened.
		if t.h > 0 {
			if got := t.convoHeight(); got != t.vp.Height {
				t.vp.Height = got
				t.applyScroll(t.focus == focusConvo && !t.asking && len(t.convo) > 0)
			}
		}
		nm = t
	}
	return nm, cmd
}

func (m tuiModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.traceResize(msg)
		first, prevW := m.w == 0 && m.h == 0, m.w
		m.w, m.h = msg.Width, msg.Height
		// Layout: head + top + convo + divider(1) + lower + status(1).
		// convoHeight() is the one place that budget lives — see it for why the
		// viewport's own height has to match the drawn one.
		//
		// Resize the viewport in place rather than building a new one: a fresh
		// viewport starts at YOffset 0, so every resize threw the reader back to
		// the top of the conversation. On a phone the soft keyboard opening and
		// closing is a resize, and it fires constantly.
		if first {
			m.vp = newConvoPane(msg.Width, msg.Height-4)
		} else {
			m.vp.Width = max(1, msg.Width)
		}
		m.input.width = msg.Width - 2
		m.search.Width = msg.Width - 4
		// Only when the WIDTH actually changed: a renderer is word-wrap state
		// and nothing else, and rebuilding one is not free. A height-only
		// resize (the common one — an ask panel growing) rebuilt it for nothing.
		if m.md == nil || m.mdWidth != msg.Width {
			m.md, m.mdWidth = newMarkdown(msg.Width-2), msg.Width
		}
		if m.inspecting {
			m.insp.setSize(m.w, m.h)
		}
		// A height-only resize does not change a single wrapped line — only the
		// row budget moved. Re-wrapping and re-joining the whole scrollback for
		// that is pure waste, and on a phone the soft keyboard makes it the most
		// frequent message there is. The funnel in Update re-heights the viewport
		// and re-runs the scroll policy, which is the entire job here.
		if !first && msg.Width == prevW {
			return m, nil
		}
		// Now that the new width has reached the input (lowerView measures it),
		// the row budget is knowable — refresh() scrolls against it.
		m.vp.Height = m.convoHeight()
		m.refresh()
		return m, nil

	case modelFetchMsg:
		m.modelList = []string(msg)
		sort.Strings(m.modelList)
		m.modelFetching = false
		// Start on the current model when it's in the list.
		for i, id := range m.modelList {
			if id == m.model {
				m.modelSel = i
				break
			}
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
		// So does the session picker.
		if m.picking {
			return m.updatePicking(msg)
		}
		// So does the model picker.
		if m.modelPicking {
			return m.updateModelPicking(msg)
		}
		// Typing a "/" search query owns the keys until enter/esc.
		if m.searching {
			return m.updateSearch(msg)
		}
		// The activity zone owns its keys while focused: ↑/↓ select, → descends,
		// ← ascends and finally leaves. Anything it does not claim (tab, ctrl+c,
		// "/") falls through to the switch below.
		if m.focus == focusActivity && !m.asking && !m.searching {
			if cmd, ok := m.actKeys(msg.String()); ok {
				m.refresh()
				return m, cmd
			}
		}
		switch msg.String() {
		case "ctrl+c":
			if m.disableExit {
				return m, nil // exit disabled — use /exit
			}
			m.quitting = true // deliberate exit: do not respawn the engine
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
			// Agent scope is the outermost thing esc can be inside. Quitting dun
			// from within a child's conversation is not "one level out" by any
			// reading, and it was what esc did.
			if m.scopeAgent != 0 {
				return m, m.leaveAgentScope()
			}
			if m.disableExit {
				return m, nil // exit disabled — use /exit
			}
			m.quitting = true // deliberate exit: do not respawn the engine
			return m, tea.Quit
		case "/":
			if m.focus == focusConvo {
				m.searching, m.searchActive, m.matches = true, false, nil
				m.search.Reset()
				m.search.Focus()
				m.refresh()
				return m, textinput.Blink
			}
			m.input = m.input.HandleKey(msg)
			return m, nil
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
			m.cycleFocus(1)
			m.refresh()
			return m, textinput.Blink
		case "shift+tab":
			m.cycleFocus(-1)
			m.refresh()
			return m, textinput.Blink
		case "pgup", "pgdown":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			m.updateScrollPin()
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
					m.insp = newInspector(tb.name, tb.input, tb.raw)
					m.insp.setSize(m.w, m.h)
					m.inspecting = true
					return m, nil
				}
				// Otherwise cycle the focused block through its view states.
				if m.sel >= 0 && m.sel < len(m.convo) && m.convo[m.sel].expandable() {
					m.convo[m.sel].state = m.convo[m.sel].state.Next()
					if m.convo[m.sel].state == viewMinimized && m.convo[m.sel].docs != nil {
						m.convo[m.sel].docs.descended = false // closing collapses the descent
					}
					m.refresh()
				}
				return m, nil
			}
			if m.suggestActive() && m.suggestSel < len(m.suggestions) { // accept the highlighted suggestion into the input
				m.input.SetValue(m.suggestions[m.suggestSel].text)
				m.input.CursorEnd()
				m.suggestions = nil // hide; they reappear when the input is cleared
				m.refresh()
				return m, nil
			}
			// Enter submits the message. Alt+Enter inserts a newline in the
			// multiline buffer (so you can compose multi-line messages).
			if msg.Type == tea.KeyEnter && msg.Alt {
				// Alt+Enter → insert newline.
				m.input = m.input.HandleKey(msg)
				m.refresh()
				return m, nil
			}
			v := strings.TrimSpace(m.input.Value())
			if strings.HasPrefix(v, "/") {
				return m, m.runPaletteEnter(v)
			}
			// Bare "exit" (no slash) is a hidden command: exits silently.
			if v == "exit" {
				m.quitting = true
				return m, tea.Quit
			}
			// Sending while a turn runs is allowed: the engine buffers the message
			// and lifts it into the next tool result, so the agent reads it inside
			// the turn it is already running. Still blocked before `ready`, when
			// there is no engine to receive it.
			if v == "" || m.starting {
				return m, nil
			}
			return m.sendUser(v), nil
		case "ctrl+up":
			// Ctrl+Up: navigate history (previous entry).
			if m.focus == focusConvo {
				// In convo focus, Ctrl+Up is ignored (plain Up handles convo nav).
				return m, nil
			}
			if len(m.history) > 0 && m.histIdx > 0 {
				m.histIdx--
				m.input.SetValue(m.history[m.histIdx])
				m.input.CursorEnd()
				m.refresh()
			}
			return m, nil
		case "ctrl+down":
			// Ctrl+Down: navigate history (next entry).
			if m.focus == focusConvo {
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
				m.refresh()
			}
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
			if m.suggestActive() { // move up in the suggestion selector
				if m.suggestSel > 0 {
					m.suggestSel--
					m.refresh()
				}
				return m, nil
			}
			if m.paletteActive() { // move up in the command palette
				if m.paletteSel > 0 {
					m.paletteSel--
					m.refresh()
				}
				return m, nil
			}
			// Plain up moves within the composer until it runs out of buffer;
			// from the FIRST row it recalls history, which is what a shell does
			// and what makes ↑ still mean "the thing I sent before" in a box
			// that is usually one line tall.
			if m.input.AtFirstLine() {
				if len(m.history) > 0 && m.histIdx > 0 {
					m.histIdx--
					m.input.SetValue(m.history[m.histIdx])
					m.input.CursorEnd()
					m.refresh()
				}
				return m, nil
			}
			m.input = m.input.HandleKey(msg)
			m.refresh()
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
			if m.suggestActive() { // move down in the suggestion selector
				if m.suggestSel < len(m.suggestions)-1 {
					m.suggestSel++
					m.refresh()
				}
				return m, nil
			}
			if m.paletteActive() { // move down in the command palette
				if m.paletteSel < len(m.paletteMatches())-1 {
					m.paletteSel++
					m.refresh()
				}
				return m, nil
			}
			// …and from the LAST row, ↓ walks history forward again.
			if m.input.AtLastLine() {
				if m.histIdx < len(m.history) {
					m.histIdx++
					if m.histIdx == len(m.history) {
						m.input.SetValue("")
					} else {
						m.input.SetValue(m.history[m.histIdx])
						m.input.CursorEnd()
					}
					m.refresh()
				}
				return m, nil
			}
			m.input = m.input.HandleKey(msg)
			m.refresh()
			return m, nil
		case "right":
			// Horizontal axis: [convo] ← input → [suggestions].
			//
			// Inside the conversation → means DESCEND, uniformly. `▸` already
			// means "there is something inside this" on the activity tree and on
			// a docs list, and both are opened with →; a message wearing the same
			// glyph that could only be opened with enter was the odd one out.
			// Only a block with nothing left to open hands → back to the input.
			if m.focus == focusConvo {
				if m.descendSel() {
					m.refresh()
					return m, nil
				}
				m.focus = focusInput
				m.input.Focus()
				m.refresh()
				return m, textinput.Blink
			}
			// focusInput: right from an EMPTY input accepts the ghost-text suggestion
			// (fills the buffer so the user can edit before sending).
			if m.suggestActive() && m.suggestSel < len(m.suggestions) {
				m.input.SetValue(m.suggestions[m.suggestSel].text)
				m.input.CursorEnd()
				m.suggestions = nil // hide; reappear when input is cleared
				m.refresh()
				return m, nil
			}
			m.input = m.input.HandleKey(msg)
			return m, nil
		case "left":
			if m.focus == focusConvo {
				// ← is exactly → run backwards: close the innermost thing that is
				// open, one level per press — the descend rule first, always.
				if m.ascendSel() {
					m.refresh()
					return m, nil
				}
				// Out of levels inside this conversation: if the conversation is a
				// CHILD's, the next level out is the session itself. That is what
				// the status bar promises, and until now nothing delivered it.
				if m.scopeAgent != 0 {
					return m, m.leaveAgentScope()
				}
				// With nothing to ascend OUT of, ← keeps going the way it was
				// already going: one more zone away from the input, which is the
				// activity strip when there is one and the input again when
				// there is not. Same order as tab, so the two agree.
				m.cycleFocus(1)
				m.refresh()
				return m, textinput.Blink
			}
			// focusInput: left at the FRONT of the input hops back to the
			// conversation — or, in a child's scope, back to the session. Scope
			// wins: it is where the user lands on descending, so it is where they
			// press ← to undo it.
			if m.input.Position() == 0 {
				if m.scopeAgent != 0 {
					return m, m.leaveAgentScope()
				}
				m.cycleFocus(1)
				m.refresh()
				return m, textinput.Blink
			}
			m.input = m.input.HandleKey(msg)
			return m, nil
		default:
			if m.focus == focusConvo { // keys don't type into a blurred input
				return m, nil
			}
			// Suggestion quick-pick: a digit while the picker is showing fills
			// the input (not sends), so the user can edit before sending.
			if m.suggestActive() {
				if k := msg.String(); len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
					if n := int(k[0] - '0'); n <= len(m.suggestions) {
						m.input.SetValue(m.suggestions[n-1].text)
						m.input.CursorEnd()
						m.suggestions = nil // hide; reappear when input is cleared
						m.refresh()
						return m, nil
					}
				}
			}
			m.input = m.input.HandleKey(msg)
			m.paletteSel = 0 // typing re-filters the palette; start at the top
			m.refresh()
			return m, nil
		}

	case reloadMsg:
		m.reloadVer = string(msg) // header shows "↻ …"; /reload restarts
		m.refresh()
		return m, waitReload(m.lc) // re-arm

	case dumpMsg:
		m.writeDump()
		return m, waitForDump(m.dumpSig) // re-arm for the next signal

	case evMsg:
		m.traceEvent(msg)
		nm := m.handleEvent(msg)
		cmds := []tea.Cmd{waitEvent(nm.proc.ch)}
		if nm.renderDue && !nm.tickPending {
			nm.tickPending = true
			cmds = append(cmds, renderTick())
		}
		return nm, tea.Batch(cmds...)

	case idleSuggestMsg:
		m.idleTickPending = false
		// A keystroke since the tick was armed moves the deadline: wait out the
		// remainder rather than firing early. This is what makes it a debounce
		// and not a 3-second poll.
		if wait := idleSuggestDelay - time.Since(m.lastKeyAt); wait > 0 {
			m.idleTickPending = true
			return m, idleSuggestTick(wait)
		}
		if m.idleSuggestReady() {
			m.suggestedThisIdle = true
			m.proc.controlCmd("suggest", "") // best-effort: no engine, no suggestions
		}
		return m, nil

	case renderTickMsg:
		// One frame per tick at most, no matter how fast the tokens arrive.
		m.tickPending = false
		if m.renderDue {
			m.renderDue = false
			m.refresh()
			// Still streaming? Keep the clock running rather than waiting for
			// the next token to restart it — that would add a tick of latency
			// to every frame.
			if m.busy {
				m.tickPending = true
				return m, renderTick()
			}
		}
		return m, nil

	case eofMsg:
		// From an engine we already replaced: its death was the point.
		if msg.proc != nil && m.proc != nil && msg.proc != m.proc {
			return m, nil
		}
		return m.engineGone()

	case engineUpMsg:
		if msg.err != nil {
			m.restarts++
			if m.restarts >= engineRestartMax {
				m.fatalErr = "engine will not start: " + msg.err.Error()
				m.append(stErr.Render(m.fatalErr + "\n/reconnect to try again"))
				m.refresh()
				return m, nil
			}
			m.fatalErr = "engine will not start: " + msg.err.Error() + " — retrying"
			m.refresh()
			return m, tea.Tick(engineRetryDelay, func(time.Time) tea.Msg { return retryEngineMsg{} })
		}
		m.proc = msg.proc
		m.fatalErr = ""
		m.starting = true
		m.startingStart = time.Now()
		m.refresh()
		return m, waitEvent(m.proc.ch)

	case retryEngineMsg:
		return m, restartEngine(m.opts, m.sessionID, m.everUp)

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg) // wheel scrolls the conversation viewport
		m.updateScrollPin()
		return m, cmd

	case blinkTickMsg:
		// Only refresh when content actually changed — a blind refresh on
		// every blink tick re-wraps the entire scrollback and pegs the CPU
		// for long conversations with tool calls.
		if m.renderDue {
			m.renderDue = false
			m.refresh()
		}
		return m, m.input.BlinkTick()

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
		if m.disableExit {
			return m, nil
		}
		m.quitting = true // deliberate exit: do not respawn the engine
		return m, tea.Quit
	case "esc":
		if m.noting || m.customAnswer {
			m.noting, m.customAnswer = false, false
			m.input.Reset()
			m.input.Blur()
			m.refresh()
			return m, nil
		}
		if m.disableExit {
			return m, nil
		}
		m.quitting = true // deliberate exit: do not respawn the engine
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
			m.input.placeholder = "type your answer, or chat…"
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
			m.input.placeholder = "add a detail…"
			m.input.Focus()
			m.refresh()
			return m, textinput.Blink
		}
	}
	if m.noting || m.customAnswer { // typing into the detail / custom field
		m.input = m.input.HandleKey(msg)
		return m, nil
	}
	return m, nil
}

// suggestion is one predicted next user message (--suggest).
type suggestion struct {
	text string
	prob float64
}

func parseSuggestionItems(v any) []suggestion {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]suggestion, 0, len(arr))
	for _, it := range arr {
		m, _ := it.(map[string]any)
		if t := strings.TrimSpace(str(m["text"])); t != "" {
			out = append(out, suggestion{text: t, prob: floatOf(m["prob"])})
		}
	}
	return out
}

// idleSuggestDelay is how long the input must sit still, and empty, before the
// UI spends a model call guessing what you were going to type.
//
// It exists because the engine used to volunteer suggestions at the end of
// EVERY turn — including the autonomous ones a heartbeat provoked — so a
// session nobody was typing into still billed a request per turn, and one
// landed between a tool result and the model's next move. Three seconds is long
// enough that finishing a thought does not trigger it and short enough that the
// picker is there when you look up.
const idleSuggestDelay = 3 * time.Second

// idleSuggestMsg fires when the debounce may have elapsed.
type idleSuggestMsg struct{}

func idleSuggestTick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return idleSuggestMsg{} })
}

// armIdleSuggest restarts the idle clock. Called from the keystroke funnel and
// at the end of a turn — the two things that mean "the human's turn now".
//
// It arms nothing when a suggestion could not fire anyway (suggestions off, or
// an empty conversation with nothing to predict from). Otherwise every
// keystroke of a session that will never ask schedules a timer, and a keypress
// that the UI deliberately ignores stops looking ignored to its caller.
func (m *tuiModel) armIdleSuggest() {
	if !m.idleSuggestPossible() {
		return
	}
	m.lastKeyAt = time.Now()
	m.suggestedThisIdle = false
	m.idleWantTick = true
}

// idleSuggestPossible is the cheap precondition: could this session ever ask?
func (m tuiModel) idleSuggestPossible() bool {
	return m.suggestMode != "off" && !m.quitting && len(m.convo) > 0
}

// idleSuggestReady is every condition that must hold to spend the call.
//
// The `len(m.suggestions) == 0` clause is what makes "once per idle" hold even
// though a keystroke restarts the clock: with a picker already on screen the
// answer is already there, so fiddling with the input cannot re-ask. Typing
// something and clearing it back to empty starts a genuinely new idle, and only
// then — with the old suggestions cleared by the turn that consumed them — can
// another request go out.
func (m tuiModel) idleSuggestReady() bool {
	if m.suggestMode == "off" || m.suggestedThisIdle || len(m.suggestions) > 0 {
		return false
	}
	// Never mid-turn, never over a modal: a suggestion is a question for a
	// person who is free to answer it.
	if m.busy || m.starting || m.asking || m.inspecting || m.searching || m.quitting {
		return false
	}
	if len(m.convo) == 0 { // nothing to predict from
		return false
	}
	return strings.TrimSpace(m.input.Value()) == "" &&
		time.Since(m.lastKeyAt) >= idleSuggestDelay
}

// suggestActive: the next-message picker shows only when idle with an empty
// input, so it never fights typing or a running turn. In "auto" mode it also
// requires that the input is empty (the default). In "on" mode it shows even
// when the user is typing (so suggestions are always visible).
func (m tuiModel) suggestActive() bool {
	if m.suggestMode == "off" {
		return false
	}
	return m.focus == focusInput && !m.busy && !m.starting && !m.asking &&
		len(m.suggestions) > 0 &&
		(m.suggestMode == "on" || strings.TrimSpace(m.input.Value()) == "")
}



// sendUser ships a user message to the engine (from the input or a suggestion),
// echoes it, and clears transient UI (suggestions, input).
func (m tuiModel) sendUser(v string) tuiModel {
	// In agent scope the input steers that CHILD instead of the session. Same
	// gesture the parent's agent_monitor(tell:) makes, same code path in the
	// engine — there is one definition of what telling a child means.
	if m.scopeAgent != 0 {
		m.history = append(m.history, v)
		m.histIdx = len(m.history)
		m.input.Reset()
		m.appendUser(v)
		if !m.proc.agentCmd(m.scopeAgent, "tell", v) {
			m.append(stErr.Render("no engine right now — not sent"))
		}
		m.refresh()
		return m
	}
	m.task = v
	m.history = append(m.history, v)
	m.histIdx = len(m.history)
	m.input.Reset()
	m.suggestions = nil
	m.appendUser(v)
	if m.replaying {
		m.append(stDim.Render("replaying a trace — there is no engine to send to"))
		m.refresh()
		return m
	}
	if !m.proc.send(v) {
		m.append(stErr.Render("no engine right now — not sent. /reconnect, then send it again"))
		m.refresh()
		return m
	}
	m.busy = true
	m.scrollPinned = true // user just sent a message — show it
	m.refresh()
	return m
}

// sendAnswer resolves the ask: echoes the answer, ships it to the engine, and
// resets to the input pane.
func (m tuiModel) sendAnswer(v string) tuiModel {
	m.appendUser(v)
	if !m.proc.answer(v) {
		m.append(stErr.Render("no engine right now — answer not sent"))
	}
	m.asking, m.noting, m.customAnswer = false, false, false
	m.askOptions, m.askSel, m.askNote = nil, 0, ""
	m.askMulti, m.askChecked = false, nil
	m.input.Reset()
	m.input.placeholder = "ask dun to do something…"
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

// noteServers records which switchable servers are up, so a restart can put
// them back. Only ones with a command (/rag, /lsp) — the rest are the config's
// business, and the new engine reads the same config.
func (m *tuiModel) noteServers(ev evMsg) {
	list, ok := ev["servers"].([]any)
	if !ok {
		return
	}
	if m.wantServers == nil {
		m.wantServers = map[string]bool{}
	}
	for _, s := range list {
		st, ok := s.(map[string]any)
		if !ok {
			continue
		}
		alias := aliasOf(str(st["id"]))
		if _, switchable := serverAliases[alias]; !switchable {
			continue
		}
		running, _ := st["running"].(bool)
		m.wantServers[alias] = running
	}
}

// reapplyServers turns back on whatever the user had turned on, after a
// restart. A no-op on the first ready, when nothing has been asked for yet.
func (m tuiModel) reapplyServers() tuiModel {
	if m.restarts == 0 {
		return m
	}
	for alias, want := range m.wantServers {
		if want {
			m.proc.serverCmd(alias, "on")
		}
	}
	return m
}

func (m tuiModel) handleEvent(ev evMsg) tuiModel {
	switch ev["type"] {
	case "workspace":
		m.branch = str(ev["branch"])
		m.append(stDim.Render("worktree branch: " + m.branch))
	case "session":
		// Remembered so a respawned engine reattaches to THIS conversation
		// rather than starting a new one. Before this point there is nothing to
		// reattach TO, and guessing (--continue) would grab an unrelated session.
		m.sessionID = str(ev["id"])
		m.everUp = true
	case "ready":
		m.starting = false
		m.tools = nil
		if ts, ok := ev["tools"].([]any); ok {
			for _, t := range ts {
				m.tools = append(m.tools, fmt.Sprint(t))
			}
		}
		m.append(stDim.Render(fmt.Sprintf("ready — %d tools: %s", len(m.tools), strings.Join(m.tools, ", "))))
		// Show per-server start times for anything that took >1s.
		if srvs, ok := ev["servers"].([]any); ok {
			for _, sv := range srvs {
				s, ok := sv.(map[string]any)
				if !ok {
					continue
				}
				secs, _ := s["startSeconds"].(float64)
				if secs > 1.0 {
					m.append(stDim.Render(fmt.Sprintf("%s started in %ds", s["id"], int(secs+0.5))))
				}
			}
		}
		// Say which tool families are off, once. Otherwise the only symptom is
		// an agent that never navigates code or searches docs, and no reason.
		if hint := strings.TrimSpace(str(ev["hint"])); hint != "" {
			m.append(stNote.Render(hint))
		}
		// Order matters: reapply reads what was running BEFORE this engine
		// existed, and this event reports the fresh one (everything off).
		m = m.reapplyServers()
		m.noteServers(ev)
		return m
	case "server":
		if msg := strings.TrimSpace(str(ev["message"])); msg != "" {
			m.append(stNote.Render(msg))
		}
		// The tool list changed; keep /help's count and the status bar honest.
		if ts, ok := ev["tools"].([]any); ok {
			m.tools = nil
			for _, t := range ts {
				m.tools = append(m.tools, fmt.Sprint(t))
			}
		}
		m.noteServers(ev)
	case "recap":
		// One dim line that OPENS. The scrollback keeps whatever was already
		// drawn — this is about the model's context, not about rewriting what you
		// have read — but a recap is no longer confirmed before it applies, so
		// the line has to be able to show what replaced the span. → expands it.
		note := stDim.Render("✂ " + str(ev["note"]))
		if detail := strings.TrimRight(str(ev["detail"]), "\n"); detail != "" {
			m.convo = append(m.convo, convoEntry{
				collapsed: stDim.Render("▸ ") + note,
				full:      stDim.Render("▾ ") + note + "\n" + stDim.Render(detail),
			})
			m.refresh()
		} else {
			m.append(note)
		}
		// Track for /context
		m.ctxStats.recaps++
		if entries := evNum(ev["entries"]); entries > 0 {
			m.ctxStats.entriesRecapped += int(entries)
		}
		if chars := evNum(ev["chars"]); chars > 0 {
			m.ctxStats.charsRecapped += int(chars)
		}
	case "control":
		if msg := strings.TrimSpace(str(ev["message"])); msg != "" {
			m.append(stNote.Render(msg))
		}
		// /close is terminal: the session it was editing no longer exists, so
		// staying attached to it would be showing a conversation that cannot be
		// resumed. Quit only once the engine has confirmed the work is done.
		if str(ev["id"]) == "close" {
			m.quitting = true
		}
	case "token":
		m.busy = true // a turn is active (incl. autonomous background-completion turns)
		if m.busyStart.IsZero() {
			m.busyStart = time.Now()
		}
		m.suggestions = nil
		m.cur += str(ev["text"])
		// Do NOT render here. A provider streaming 60 tokens/s would drive 60
		// full frames a second through the same Update loop that reads the
		// keyboard, and a frame is not free even with the wrap cache. Mark the
		// screen dirty; a tick renders at most renderHz. See renderTickMsg.
		m.renderDue = true
	case "suggestions":
		m.suggestions = parseSuggestionItems(ev["items"])
		m.suggestSel = 0
		m.refresh()
	case "tool_call":
		m.busy = true
		if m.busyStart.IsZero() {
			m.busyStart = time.Now()
		}
		m.suggestions = nil
		m.flushCur()
		args, _ := ev["args"].(map[string]any)
		m.convo = append(m.convo, convoEntry{collapsed: stTool.Render("⚙ " + str(ev["tool"]) + "(" + argPreview(args, 80) + ")")})
		m.pendingTool = len(m.convo) - 1
		m.pendingArgs = args
		m.refresh()
	case "tool_result":
		result := str(ev["result"])
		ce := m.foldedTool(str(ev["tool"]), m.pendingArgs, result)
		if idx := m.pendingTool; idx >= 0 && idx < len(m.convo) {
			// Fold the result into its call so the pair is one collapsible unit.
			m.convo[idx] = ce
			m.pendingTool, m.pendingArgs = -1, nil
		} else {
			m.convo = append(m.convo, ce)
		}
		// Track tool result stats for /context
		m.ctxStats.toolResults++
		if strings.Contains(result, "[") && strings.Contains(result, "characters elided") {
			m.ctxStats.resultsTruncated++
		}
		// liftQueued drains the buffer into every tool result. So any provisional
		// messages were just delivered to the LLM — clear the provisional marker
		// and restore normal style.
		for i := range m.convo {
			if m.convo[i].provisional {
				m.convo[i].provisional = false
				m.convo[i].collapsed = stUser.Render("› " + m.convo[i].provisionalText)
				m.convo[i].userText = m.convo[i].provisionalText
				m.convo[i].provisionalText = ""
				m.convo[i].invalidateWrap()
			}
		}
		m.queuedMsgs = 0
		m.scrollPinned = true
		m.refresh()
	case "history":
		if m.skipHistory {
			// A respawned engine replays the conversation it just resumed. It is
			// already on screen — rendering it again would double the scrollback.
			m.skipHistory = false
			break
		}
		items, _ := ev["items"].([]any)
		m.replay(items)
	case "message":
		// tokens already streamed the reply; nothing to add.
	case "agents":
		m.agents = agentRowsFromEvent(ev)
	case "jobs":
		m.jobs = jobRowsFromEvent(ev)
	case "agent_history":
		// A child's scrollback, pulled on demand. Ignored unless it is the
		// child currently on screen: a late reply for a scope we have already
		// left would paste someone else's conversation over this one.
		if num(ev["agent"]) == m.scopeAgent && m.scopeAgent != 0 {
			m.convo = nil
			if p := str(ev["prompt"]); p != "" {
				m.task = p
			}
			items, _ := ev["items"].([]any)
			m.replay(items)
		}
	case "agent_told":
		if msg := str(ev["message"]); msg != "" {
			m.append(stNote.Render("↳ " + oneLine(msg)))
		}
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
			m.input.placeholder = "type your answer…"
			m.input.Focus()
		}
	case "retry":
		return m.handleRetry(ev)
	case "queued":
		// The message was buffered into the RUNNING turn rather than starting a new
		// one. Mark it as provisional in the conversation so the user sees it
		// where it belongs — not stuck above the divider. When the turn ends
		// the provisional markers are cleared and the messages render normally.
		text := str(ev["text"])
		if text != "" {
			// If the last entry is already provisional, this is a new message
			// that needs its own entry (not the echo from sendUser).
			if len(m.convo) > 0 && m.convo[len(m.convo)-1].provisional {
				m.convo = append(m.convo, convoEntry{
					collapsed:       stDim.Render("› " + text + " (queued)"),
					provisional:     true,
					provisionalText: text,
					userText:        text,
				})
			} else if len(m.convo) > 0 {
				// Mark the echo that sendUser just appended as provisional.
				e := &m.convo[len(m.convo)-1]
				e.provisional = true
				e.provisionalText = text
				e.collapsed = stDim.Render("› " + text + " (queued)")
				e.invalidateWrap()
			}
		}
		m.queuedMsgs = int(evNum(ev["count"]))
		// Track OOB messages for /context.
		m.ctxStats.oobMessages++
		m.refresh()
	case "usage":
		// Accumulate token usage stats for /context.
		m.ctxStats.totalTokens += int(evNum(ev["total"]))
		m.ctxStats.activeTokens = int(evNum(ev["active"]))
		m.ctxStats.cachedTokens += int(evNum(ev["cached"]))
		m.ctxStats.processedTokens += int(evNum(ev["processed"]))
		m.ctxStats.generatedTokens += int(evNum(ev["generated"]))
		m.ctxStats.turns = int(evNum(ev["turns"]))
		// Session-level stats (cumulative — use last value, not sum).
		if v := evNum(ev["system_tokens"]); v > 0 {
			m.ctxStats.systemTokens = int(v)
		}
		if v := evNum(ev["forced_calls"]); v > 0 {
			m.ctxStats.forcedToolCalls = int(v)
		}
		if v := evNum(ev["notifications_lifted"]); v > 0 {
			m.ctxStats.notificationsSmuggled = int(v)
		}
	case "done":
		m.flushCur()
		// Clear any remaining provisional markers — the turn is done, so
		// everything was delivered to the LLM.
		for i := range m.convo {
			if m.convo[i].provisional {
				m.convo[i].provisional = false
				m.convo[i].collapsed = stUser.Render("› " + m.convo[i].provisionalText)
				m.convo[i].userText = m.convo[i].provisionalText
				m.convo[i].provisionalText = ""
				m.convo[i].invalidateWrap()
			}
		}
		// Force scroll to bottom so the delivered messages are visible.
		m.scrollPinned = true
		m.busy, m.queuedMsgs = false, 0
		m.busyStart = time.Time{}
		m.clearRetry()
		// The turn is done, so the human's idle starts NOW. This is the only
		// place a suggestion request can begin — `done` is what "the LLM is
		// marked done" means on the wire, so nothing can land between a tool
		// call and its result.
		m.armIdleSuggest()
		m.refresh()
	case "error":
		m.append(stErr.Render("error: " + str(ev["error"])))
		// Clear any remaining provisional markers — the engine flushes them on
		// error too, so they're part of the conversation history.
		for i := range m.convo {
			if m.convo[i].provisional {
				m.convo[i].provisional = false
				m.convo[i].collapsed = stUser.Render("› " + m.convo[i].provisionalText)
				m.convo[i].userText = m.convo[i].provisionalText
				m.convo[i].provisionalText = ""
				m.convo[i].invalidateWrap()
			}
		}
		// Force scroll to bottom so the delivered messages are visible.
		m.scrollPinned = true
		m.busy, m.queuedMsgs = false, 0
		m.busyStart = time.Time{}
		m.clearRetry()
		// Whether the SESSION survived is the engine's call, not something to
		// infer from the error text. It says so, because promising "send a
		// message to retry" to a session that cannot run another turn is worse
		// than saying nothing.
		if b, _ := ev["fatal"].(bool); b {
			m.append(stDim.Render("the session ended — resume it with: dun --continue"))
		} else {
			// The conversation is on disk, so the next message pairs off whatever
			// was interrupted and picks up where this stopped.
			m.append(stDim.Render("the session is intact — send a message to retry from here"))
		}
		m.refresh()
	case "reset":
		// Engine confirmed the store was cleared; nothing extra to show —
		// the /clear handler already appended a message.
	case "compaction":
		// On screen, not just in a log a TUI never shows: this is the one
		// operation that DESTROYS conversation, and it used to happen silently.
		m.append(stNote.Render("🗜 " + str(ev["text"])))
		// Track for /context: compute saved from before/after
		m.ctxStats.compactions++
		before := evNum(ev["tokens_before"])
		after := evNum(ev["tokens_after"])
		if before > after {
			m.ctxStats.tokensCompacted += int(before - after)
		}
		m.refresh()
	case "exit":
		// The engine only announces an exit it CHOSE — ctrl-C, an explicit stop,
		// stdin closing. That is the difference between "it left" and "it died",
		// and it decides whether the TUI puts a new one in its place.
		m.exitAnnounced = true
		if r := str(ev["reason"]); r != "" {
			m.fatalErr = "dun engine exited: " + r
		}
	}
	return m
}

// handleRetry renders the provider-wait state: a live banner while dun is backing
// off, a scrollback line for the start and the outcome.
//
// Only the FIRST wait of an outage goes to scrollback. The banner carries the
// attempt number and counts down, so awareness lives there; logging every attempt
// would bury the conversation under a hundred identical lines during a long
// outage.
func (m tuiModel) handleRetry(ev evMsg) tuiModel {
	switch str(ev["kind"]) {
	case "recovered":
		m.clearRetry()
		// Update the retry block in-place if it exists.
		if len(m.convo) > 0 && m.convo[len(m.convo)-1].tool == nil && m.convo[len(m.convo)-1].collapsed != "" && strings.HasPrefix(m.convo[len(m.convo)-1].collapsed, "⏳ ") {
			e := &m.convo[len(m.convo)-1]
			e.collapsed = stTool.Render("✓ " + str(ev["text"]))
			e.full = ""
			e.state = viewMinimized
		} else {
			m.append(stTool.Render("✓ " + str(ev["text"])))
		}
	case "giveup":
		m.clearRetry()
		m.busy = false
		m.busyStart = time.Time{}
		// Update the retry block in-place if it exists.
		if len(m.convo) > 0 && m.convo[len(m.convo)-1].tool == nil && m.convo[len(m.convo)-1].collapsed != "" && strings.HasPrefix(m.convo[len(m.convo)-1].collapsed, "⏳ ") {
			e := &m.convo[len(m.convo)-1]
			e.collapsed = stErr.Render("✗ " + str(ev["text"]))
			e.full = ""
			e.state = viewMinimized
		} else {
			m.append(stErr.Render("✗ " + str(ev["text"])))
		}
	default:
		// A turn-scope retry means the generation died mid-stream and will be
		// redone, so drop the half-streamed text rather than letting the regenerated
		// reply append to a broken sentence. Tool results already recorded are kept
		// (the engine resumes from them), only the interrupted reply is discarded.
		if str(ev["scope"]) == "turn" {
			m.cur = ""
		}
		m.busy = true
		m.retry = m.retryBanner(ev)
		if ms := evNum(ev["delay_ms"]); ms > 0 {
			m.retryDue = time.Now().Add(time.Duration(ms) * time.Millisecond)
		} else {
			m.retryDue = time.Time{}
		}
		if m.retrySeen == 0 {
			// First retry: create a collapsible block with a brief collapsed line
			// and full formatted details.
			collapsed := stNote.Render("⏳ " + str(ev["reason"]))
			full := m.retryDetails(ev)
			m.convo = append(m.convo, convoEntry{
				collapsed: stDim.Render("▸ ") + collapsed,
				full:      full,
			})
		} else {
			// Subsequent retry: update the existing block's full details.
			if len(m.convo) > 0 {
				e := &m.convo[len(m.convo)-1]
				e.full = m.retryDetails(ev)
				e.collapsed = stDim.Render("▸ ") + stNote.Render("⏳ " + str(ev["reason"]))
				e.invalidateWrap()
			}
		}
		m.retrySeen++
	}
	m.refresh()
	return m
}

// retryDetails builds the expanded view for a retry block: formatted fields
// instead of a raw JSON payload.
func (m tuiModel) retryDetails(ev evMsg) string {
	var lines []string
	reason := str(ev["reason"])
	if reason != "" {
		lines = append(lines, stNote.Render("reason:  ") + reason)
	}
	if detail := str(ev["detail"]); detail != "" {
		lines = append(lines, stDim.Render("detail:  ") + detail)
	}
	if cap := evNum(ev["capacity"]); cap > 0 {
		lines = append(lines, stDim.Render(fmt.Sprintf("capacity:  %d/%d busy  (%d ahead)",
			int(evNum(ev["in_flight"])), int(cap), int(evNum(ev["waiting"])))))
	}
	if queue := str(ev["queue"]); queue != "" {
		lines = append(lines, stDim.Render("queue:   ") + queue)
	}
	if attempt := evNum(ev["attempt"]); attempt > 0 {
		lines = append(lines, stDim.Render(fmt.Sprintf("attempt: %d", int(attempt))))
	}
	if delay := evNum(ev["delay_ms"]); delay > 0 {
		lines = append(lines, stDim.Render(fmt.Sprintf("retry in:  %s", time.Duration(delay)*time.Millisecond)))
	}
	if elapsed := evNum(ev["elapsed_ms"]); elapsed > 0 {
		lines = append(lines, stDim.Render(fmt.Sprintf("elapsed:   %s", time.Duration(elapsed)*time.Millisecond)))
	}
	if budget := evNum(ev["budget_ms"]); budget > 0 {
		lines = append(lines, stDim.Render(fmt.Sprintf("budget:    %s", time.Duration(budget)*time.Millisecond)))
	}
	if str(ev["server_asked"]) == "true" {
		lines = append(lines, stDim.Render("server asked for this delay"))
	}
	return strings.Join(lines, "\n")
}

// retryBanner is the status-line text for a wait in progress: what the provider
// said, including the queue numbers when it sent them (corrallm does).
func (m tuiModel) retryBanner(ev evMsg) string {
	b := str(ev["reason"])
	if cap := evNum(ev["capacity"]); cap > 0 {
		b += fmt.Sprintf(" · %d/%d busy", int(evNum(ev["in_flight"])), int(cap))
	}
	if w := evNum(ev["waiting"]); w > 0 {
		b += fmt.Sprintf(" · %d ahead", int(w))
	}
	return b
}

// clearRetry takes the banner down.
func (m *tuiModel) clearRetry() {
	m.retry, m.retryDue, m.retrySeen = "", time.Time{}, 0
}

// evNum reads a JSON number out of an event (they decode as float64).
func evNum(v any) float64 {
	f, _ := v.(float64)
	return f
}

// retryCountdown is the " · next try in 7s" tail of the retry banner. Recomputed
// on every spinner tick, which is what makes the wait visibly a wait rather than
// a hang.
func (m tuiModel) retryCountdown() string {
	if m.retryDue.IsZero() {
		return ""
	}
	left := time.Until(m.retryDue)
	if left <= 0 {
		return " · retrying now"
	}
	return fmt.Sprintf(" · next try in %s", left.Round(time.Second))
}

// startingElapsed returns the " · 3s" elapsed tail for the spawning banner.
func (m tuiModel) startingElapsed() string {
	if m.startingStart.IsZero() {
		return ""
	}
	return fmt.Sprintf(" · %s", time.Since(m.startingStart).Round(time.Second))
}

// busyElapsed returns the " · 3s" elapsed tail for the generic busy banner.
func (m tuiModel) busyElapsed() string {
	if m.busyStart.IsZero() {
		return ""
	}
	return fmt.Sprintf(" · %s", time.Since(m.busyStart).Round(time.Second))
}

// queuedHint reports messages typed mid-turn that are waiting to be lifted into
// the next tool result, so the user knows they landed.
func (m tuiModel) queuedHint() string {
	switch m.queuedMsgs {
	case 0:
		return ""
	case 1:
		return " (1 message queued for this turn)"
	}
	return fmt.Sprintf(" (%d messages queued for this turn)", m.queuedMsgs)
}

// exitHint is the status-bar exit prompt — "/exit to exit" when ctrl+c is
// disabled (--disable-exit), else the usual "ctrl+c quit".
func (m tuiModel) exitHint() string {
	if m.disableExit {
		return "/exit to exit"
	}
	return "ctrl+c quit"
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
func (m tuiModel) scrollOverlay() string {
	if m.vp.YOffset == 0 || m.scrollPinned {
		return "" // at top or pinned to bottom — nothing to show
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
	var closestText string
	closestBottom := -1
	for i, e := range m.convo {
		if e.userText == "" {
			continue
		}
		h := 0
		if i < len(m.blockH) {
			h = m.blockH[i]
		}
		bottom := e.rowOffset + h
		if bottom <= yOff && bottom > closestBottom {
			closestBottom = bottom
			closestText = e.userText
		}
	}
	if closestText == "" {
		return ""
	}
	// Exactly one row, always. clip() overshoots — it appends the ellipsis to n
	// characters rather than fitting inside n — so "› " + clip(text, width-2)
	// came to width+1 cells and lipgloss wrapped the bar onto a second line.
	// That put the frame a row over the terminal, which costs the top row: this
	// bar. fitPlain is the width-aware fit the task line right below already
	// uses, and oneLine keeps an embedded newline from doing the same thing.
	text, _ := fitPlain(oneLine(closestText), max(0, m.vp.Width-2))
	return stUser.Width(max(1, m.vp.Width)).Render("› " + text)
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
func (m tuiModel) convoHeight() int {
	h := m.h - 2 // divider 1 + status 1; every other row is measured, not assumed
	h -= lipgloss.Height(m.headView())
	// The activity strip sits above the conversation. It vanishes when it has
	// nothing to say, so a session that never delegates loses no space to it.
	if a := m.activityView(); a != "" {
		h -= lipgloss.Height(a)
	}
	h -= lipgloss.Height(m.lowerView()) // input, or the answer picker (several rows)
	return max(1, h)
}

// headView is the top row: the scroll overlay when the conversation is scrolled
// up, else the dun title bar. Measured rather than assumed to be one row — it
// is the row the renderer drops first when a frame overdraws, so a head that
// wrapped would delete itself.
func (m tuiModel) headView() string {
	if overlay := m.scrollOverlay(); overlay != "" {
		return overlay
	}
	head := stHeader.Render("dun") + stDim.Render(" "+version+"  "+m.workspace)
	if m.branch != "" {
		head += stDim.Render("  ⎇ " + m.branch)
	}
	if n := len(m.tools); n > 0 {
		head += stDim.Render(fmt.Sprintf("  · %d tools", n))
	}
	if n := m.liveAgents(); n > 0 {
		head += stNote.Render(fmt.Sprintf("  · %d agents", n))
	}
	if m.reloadVer != "" {
		head += "  " + stNote.Render("↻ "+m.reloadVer+" (/reload)")
	}
	return head
}

func (m tuiModel) View() string {
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
	convoH := m.convoHeight()
	vp := m.vp
	vp.Height = convoH
	// Focus cue lives entirely in the divider's bright half — no pane borders.
	div := divider(m.w, m.focus == focusConvo && !m.asking)

	var status string
	switch {
	case m.fatalErr != "":
		status = stErr.Render(m.fatalErr)
	case m.starting:
		status = m.spin.View() + stDim.Render(" spawning tool servers…"+m.startingElapsed())
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
	case m.retry != "":
		status = stNote.Render("⏳ "+m.retry) + stDim.Render(m.retryCountdown()+"  ("+m.exitHint()+")")
	case m.busy:
		status = m.spin.View() + stDim.Render(" working…"+m.busyElapsed()+m.queuedHint()+"  ("+m.exitHint()+")")
	case m.focus == focusActivity && m.actLevel == actCollapsed:
		status = stDim.Render("activity  ·  → open · tab input · " + m.exitHint())
	case m.focus == focusActivity:
		status = stDim.Render("activity  ·  ↑/↓ select · → open (agent = its view) · ← back · tab input")
	case m.scopeAgent != 0:
		status = stDim.Render(fmt.Sprintf("agent #%d  ·  type to tell it · tab activity · ←/esc back to the session", m.scopeAgent))
	case m.focus == focusConvo:
		if d := m.selDocs(); d != nil && d.descended {
			status = stDim.Render("docs  ·  ↑/↓ doc · → open · ← close, then out · ctrl+c quit")
		} else {
			status = stDim.Render("convo  ·  ↑/↓ select · →/← open/close ▸ · enter inspector · / search · tab input")
		}
	case m.suggestActive():
		status = stDim.Render(fmt.Sprintf("next?  ·  ↑/↓ cycle · enter/right/1–%d accept · or type · "+m.exitHint(), len(m.suggestions)))
	default:
		status = stDim.Render("ready  ·  tab scroll · ↑/↓ edit · alt+enter newline · ctrl+↑/↓ history · enter send · " + m.exitHint())
	}
	// The header row shows the off-screen user message when scrolled up, or the
	// normal dun title bar when at the bottom — convoHeight() budgeted for it.
	out := append([]string{m.headView()}, top...)
	out = append(out, m.viewportView(vp), div, lower, status)
	return strings.Join(out, "\n")
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
		return m.modelPanel()
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

// appendUser adds a user message and stores the raw text for full-width re-rendering.
func (m *tuiModel) appendUser(text string) {
	m.convo = append(m.convo, convoEntry{collapsed: stUser.Render("› " + text), userText: text})
	m.refresh()
}

func (m *tuiModel) flushCur() {
	if m.cur != "" {
		// Finalize the streamed reply as rendered markdown (headers, lists, code).
		m.convo = append(m.convo, convoEntry{collapsed: renderMarkdown(m.md, strings.TrimRight(m.cur, "\n"))})
		m.cur = ""
	}
}

// foldedTool builds the collapsed/full/raw folded block for a completed tool
// call+result — shared by the live tool_result path and history replay so both
// render identically. The tool block feeds the inspector overlay (raw input +
// complete output).
func (m *tuiModel) foldedTool(tool string, args map[string]any, result string) convoEntry {
	preview, body := renderToolResult(renderCtx{tool: tool, args: args, result: result, width: m.vp.Width})
	callShort := stTool.Render("⚙ " + tool + "(" + argPreview(args, 80) + ")")
	callFull := stTool.Render("⚙ " + tool)
	af := argFull(args)
	if af != "" {
		callFull += "\n" + af
	}
	return convoEntry{
		collapsed: stDim.Render("▸ ") + callShort + "\n" + preview,
		full:      stDim.Render("▾ ") + callFull + "\n" + body,
		raw:       stDim.Render("▾ ") + callFull + "\n" + stDim.Render(result),
		tool:      &toolBlock{name: tool, input: af, output: body, raw: result},
	}
}

// replay rebuilds scrollback from a `history` event (a resumed session), reusing
// the same rendering as live events: user echo, assistant markdown, folded tool
// call/result blocks, notifications. No turn runs — this is pure display. A
// trailing dim marker delimits the resumed history from new activity.
func (m *tuiModel) replay(items []any) {
	var pendName, pendID string
	var pendArgs map[string]any
	hadPend := false
	flushPend := func() {
		if hadPend {
			// A tool_call with no following result (truncated session): show it alone.
			m.convo = append(m.convo, convoEntry{collapsed: stTool.Render("⚙ " + pendName + "(" + argPreview(pendArgs, 80) + ")")})
			pendName, pendID, pendArgs, hadPend = "", "", nil, false
		}
	}
	n := 0
	for _, it := range items {
		im, _ := it.(map[string]any)
		switch str(im["kind"]) {
		case "user":
			flushPend()
			m.task = str(im["content"]) // the task line survives a resume
			m.convo = append(m.convo, convoEntry{collapsed: stUser.Render("› " + str(im["content"])), userText: str(im["content"])})
			n++
		case "assistant":
			flushPend()
			m.convo = append(m.convo, convoEntry{collapsed: renderMarkdown(m.md, strings.TrimRight(str(im["content"]), "\n"))})
			n++
		case "tool_call":
			flushPend()
			pendName, pendID, hadPend = str(im["tool"]), str(im["call_id"]), true
			pendArgs, _ = im["args"].(map[string]any)
		case "tool_result":
			args := pendArgs
			if !hadPend || str(im["call_id"]) != pendID {
				args = nil // unmatched result: render without the call's args
			}
			m.convo = append(m.convo, m.foldedTool(str(im["tool"]), args, str(im["content"])))
			pendName, pendID, pendArgs, hadPend = "", "", nil, false
			n++
		case "notification":
			flushPend()
			m.convo = append(m.convo, convoEntry{collapsed: stNote.Render("🔔 " + oneLine(str(im["content"])))})
			n++
		}
	}
	flushPend()
	if n > 0 {
		m.convo = append(m.convo, convoEntry{collapsed: stDim.Render(fmt.Sprintf("── resumed %d entries ──", n))})
	}
	m.refresh()
}

// selDocs returns the docsBlock of the selected entry, or nil.
func (m *tuiModel) selDocs() *docsBlock {
	if m.sel >= 0 && m.sel < len(m.convo) {
		return m.convo[m.sel].docs
	}
	return nil
}

// descendSel opens the selected block one level further and reports whether →
// was consumed by the conversation. False means "this block has nothing left to
// open", which is the only case where → hands focus back to the input.
//
// The order is innermost-first, so → always acts on the deepest thing already
// open rather than on the block as a whole.
func (m *tuiModel) descendSel() bool {
	if m.sel < 0 || m.sel >= len(m.convo) {
		return false
	}
	e := &m.convo[m.sel]
	if d := e.docs; d != nil {
		if d.descended {
			// Inside the list: → opens the highlighted document. Already open,
			// it stays put — a nested focus does not leak out to the input, or ←
			// would no longer be the way back to where you came from.
			if d.cur >= 0 && d.cur < len(d.docs) && !d.docs[d.cur].open {
				d.docs[d.cur].open = true
			}
			return true
		}
		if e.state > viewMinimized && len(d.docs) > 0 {
			d.descended = true
			if d.cur < 0 || d.cur >= len(d.docs) {
				d.cur = 0
			}
			return true
		}
	}
	if next, ok := e.deeper(); ok {
		e.state = next
		return true
	}
	return false
}

// ascendSel closes one level and reports whether ← was consumed. False means
// the block is already shut, and ← falls through to moving between zones.
func (m *tuiModel) ascendSel() bool {
	if m.sel < 0 || m.sel >= len(m.convo) {
		return false
	}
	e := &m.convo[m.sel]
	if d := e.docs; d != nil && d.descended {
		if d.cur >= 0 && d.cur < len(d.docs) && d.docs[d.cur].open {
			d.docs[d.cur].open = false // close the document before leaving the list
			return true
		}
		d.descended = false
		return true
	}
	if prev, ok := e.shallower(); ok {
		e.state = prev
		if prev == viewMinimized && e.docs != nil {
			e.docs.descended = false
		}
		return true
	}
	return false
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





func (m *tuiModel) refresh() {
	start := time.Now()
	defer func() { frames.observe(stageRefresh, time.Since(start)) }()
	blocks := make([]string, 0, len(m.convo)+1)
	for _, e := range m.convo {
		blocks = append(blocks, e.view())
	}
	if m.cur != "" {
		blocks = append(blocks, renderMarkdown(m.md, m.cur))
	}
	// Suggestions live inside the viewport so they don't steal height from the
	// conversation — the user can still see the last messages above them.
	if m.suggestActive() {
		for i, s := range m.suggestions {
			body := stSel.Render(fmt.Sprintf("%d", i+1)) + " " + s.text + "  " +
				stDim.Render(fmt.Sprintf("%d%%", int(s.prob*100+0.5)))
			if i == m.suggestSel {
				blocks = append(blocks, addGutter(body, "▎ ", stSel))
			} else {
				blocks = append(blocks, addGutter(body, "  ", lipgloss.NewStyle()))
			}
		}
	}
	// In convo focus, gutter every block (selected one bright) so the highlight
	// aligns and the selected message shows a left border down its full height.
	selMode := m.focus == focusConvo && !m.asking && len(blocks) > 0
	width := m.vp.Width
	if selMode {
		width-- // reserve ONE column for the gutter bar — minimal reflow on focus
	}
	wrapW := max(1, width)
	rendered := make([]string, 0, len(m.vp.lines)) // display rows, not blocks
	m.blockH = make([]int, len(m.convo)) // cache convo-block heights for scroll math

	// Render all blocks. For user messages, re-render with full-width
	// background style (ignoring the pre-styled collapsed from e.view()).
	cumRow := 0 // running row offset for scrollOverlay mapping
	for i, b := range blocks {
		var w string
		if i < len(m.convo) && m.convo[i].docs == nil {
			e := &m.convo[i]
			switch {
			case e.userText != "":
				if e.wrapped == "" || e.wrapW != width || e.wrapState != e.state {
					e.wrapped, e.wrapW, e.wrapState = stUser.Width(wrapW).Render("› "+e.userText), width, e.state
				}
				w = e.wrapped
			default:
				if !e.wrapMaxOK || e.wrapState != e.state {
					// New or changed source: measure it once. Cheaper than wrapping
					// it, and it answers the question for every future width.
					e.wrapMax, e.wrapMaxOK, e.wrapState = maxLineWidth(b), true, e.state
					e.wrapped, e.wrapW = "", 0
				}
				switch {
				case e.wrapMax <= wrapW:
					w = b // already fits — no wrapping at this width or any wider
				case e.wrapped != "" && e.wrapW == width:
					w = e.wrapped
				default:
					e.wrapped, e.wrapW = cellbuf.Wrap(b, wrapW, ""), width
					w = e.wrapped
				}
			}
		} else {
			w = cellbuf.Wrap(b, wrapW, "")
		}
		if selMode {
			if i == m.sel {
				w = addGutter(w, "▎", stSel)
			} else {
				w = addGutter(w, " ", lipgloss.NewStyle())
			}
		}
		rendered = append(rendered, strings.Split(w, "\n")...)
		h := len(rendered) - cumRow
		if i < len(m.blockH) {
			m.blockH[i] = h
		}
		if i < len(m.convo) {
			m.convo[i].rowOffset = cumRow
		}
		cumRow += h
	}
	// Hand the pane the rows themselves. Joining them into one string only for
	// it to split them back was ~12ms of every refresh on a long conversation —
	// and every streamed token is a refresh.
	m.vp.SetLines(rendered)
	m.contentGen++

	m.applyScroll(selMode)
}

// applyScroll is where the viewport is allowed to move on its own: keep the
// selection in view, else stick to the bottom (live tail). It reads the block
// geometry refresh() just measured (rowOffset + blockH) rather than a rendered
// slice, so it can also be re-run when only the WINDOW changed — a resize, the
// task line appearing, the input growing a line. Height is as much an input to
// this decision as content is, and running it on content alone left the
// viewport a row short of the bottom whenever a frame's row budget moved.
func (m *tuiModel) applyScroll(selMode bool) {
	vh := m.vp.Height
	if vh <= 0 {
		return
	}
	if selMode && m.sel >= 0 && m.sel < len(m.blockH) {
		top, h := m.selGeom()
		if h >= vh {
			// Taller than the window: don't fight intra-message scroll — only snap
			// when the selection is entirely off-screen. ↑/↓ scroll within it.
			switch {
			case top >= m.vp.YOffset+vh: // fully below the fold
				m.vp.SetYOffset(top)
			case top+h <= m.vp.YOffset: // fully above the fold
				m.vp.SetYOffset(top + h - vh)
			}
			return
		}
		switch {
		case top < m.vp.YOffset:
			m.vp.SetYOffset(top)
		case top+h > m.vp.YOffset+vh:
			m.vp.SetYOffset(top + h - vh)
		}
		return
	}
	if !m.scrollPinned {
		return
	}
	// Auto-follow new content. When there's a streaming reply (m.cur), don't
	// GotoBottom() blindly — that scrolls past the user's last message to the
	// bottom of the growing reply. Instead keep that message visible.
	lastUserRow := -1
	if m.cur != "" {
		for i := range m.convo {
			if m.convo[i].userText != "" {
				lastUserRow = m.convo[i].rowOffset
			}
		}
	}
	switch {
	case lastUserRow < 0:
		m.vp.GotoBottom()
	case lastUserRow >= m.vp.YOffset+vh: // below the fold — bring it near the top
		m.vp.SetYOffset(lastUserRow - 1)
	case lastUserRow < m.vp.YOffset:
		m.vp.SetYOffset(lastUserRow)
	}
}

// updateScrollPin unpins the scroll when the user scrolls away from the bottom,
// and re-pins when they scroll back. This prevents new messages from jumping
// the viewport while the user is reading older content.
func (m *tuiModel) updateScrollPin() {
	if m.vp.AtBottom() {
		m.scrollPinned = true
	} else {
		m.scrollPinned = false
	}
	m.traceScroll()
}

// traceScroll writes the current scroll state to the trace file if tracing is on.
func (m *tuiModel) traceScroll() {
	if m.traceFile == nil || m.vp.YOffset == m.tracePrevYOff {
		return
	}
	m.tracePrevYOff = m.vp.YOffset
	fmt.Fprintf(m.traceFile, "scroll yoff=%d pinned=%v\n", m.vp.YOffset, m.scrollPinned)
}

// traceEvent writes an engine event to the trace file if tracing is on.
func (m *tuiModel) traceEvent(ev evMsg) {
	if m.traceFile == nil {
		return
	}
	etype, _ := ev["type"].(string)
	data, _ := json.Marshal(ev)
	fmt.Fprintf(m.traceFile, "event %s %s\n", etype, data)
}

// traceResize writes a resize event to the trace file if tracing is on.
func (m *tuiModel) traceResize(msg tea.WindowSizeMsg) {
	if m.traceFile == nil {
		return
	}
	fmt.Fprintf(m.traceFile, "resize w=%d h=%d\n", msg.Width, msg.Height)
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
		{"config", "", "show this session's LLM settings (change with `dun --setup`)", func(m *tuiModel, _ []string) tea.Cmd { m.showConfig(); return nil }},
		{"model", "[name]", "switch model (bare opens the picker, enter to confirm, space to toggle persist)", modelSlash},
		{"context", "", "inspect context usage: tokens, compaction, recaps, truncation", func(m *tuiModel, _ []string) tea.Cmd { m.showContext(); return nil }},
		{"reload", "", "restart into the latest build (the launcher rebuilds on source change)", func(m *tuiModel, _ []string) tea.Cmd {
			m.reloadReq = true
			return tea.Quit
		}},
		{"perf", "", "redraw timings for this session (and how to get a profile)", func(m *tuiModel, _ []string) tea.Cmd {
			m.append(stHeader.Render("render performance") + "\n" + frames.report())
			return nil
		}},
		{"scrolldebug", "", "debug: dump scroll overlay state (rowOffset, blockH, YOffset)", func(m *tuiModel, _ []string) tea.Cmd {
			var lines []string
			lines = append(lines, fmt.Sprintf("YOffset=%d pinned=%v vpH=%d convo=%d blockH=%d",
				m.vp.YOffset, m.scrollPinned, m.vp.Height, len(m.convo), len(m.blockH)))
			for i, e := range m.convo {
				if e.userText == "" {
					continue
				}
				h := 0
				if i < len(m.blockH) {
					h = m.blockH[i]
				}
				lines = append(lines, fmt.Sprintf("  [%d] user=%q rowOffset=%d h=%d bottom=%d offScreen=%v",
					i, e.userText, e.rowOffset, h, e.rowOffset+h, e.rowOffset+h <= m.vp.YOffset))
			}
			m.append(stDim.Render(strings.Join(lines, "\n")))
			return nil
		}},
		{"resume", "[id]", "switch to another saved session (bare opens the picker)", resumeSlash},
		{"trace", "[on|off|file]", "record events+scroll to a file for debugging (default: trace.jsonl in workspace)", func(m *tuiModel, args []string) tea.Cmd {
			if len(args) == 0 || args[0] == "on" {
				path := "trace.jsonl"
				if len(args) >= 2 {
					path = args[1]
				}
				f, err := os.OpenFile(filepath.Join(m.workspace, path), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
				if err != nil {
					m.append(stErr.Render("trace: " + err.Error()))
					return nil
				}
				if m.traceFile != nil {
					m.traceFile.Close()
				}
				m.traceFile = f
				m.tracePrevYOff = -1
				// Dump the current conversation layout so we can replay without
				// needing the history event (trace doesn't survive /reload re-exec).
				fmt.Fprintf(f, "resize w=%d h=%d\n", m.w, m.h)
				for i, e := range m.convo {
					h := 0
					if i < len(m.blockH) {
						h = m.blockH[i]
					}
					kind := "assistant"
					if e.userText != "" {
						kind = "user"
					} else if e.tool != nil {
						kind = "tool"
					}
					fmt.Fprintf(f, "layout %d kind=%s userText=%q rowOffset=%d h=%d\n",
						i, kind, e.userText, e.rowOffset, h)
				}
				m.append(stNote.Render("tracing → " + filepath.Join(m.workspace, path)))
				return nil
			}
			if args[0] == "off" {
				if m.traceFile != nil {
					m.traceFile.Close()
					m.traceFile = nil
				}
				m.append(stNote.Render("tracing off"))
				return nil
			}
			m.append(stDim.Render("usage: /trace [on|off [file]]"))
			return nil
		}},
		{"close", "", "discard this session for good: remove its worktree and branch, forget its transcript", func(m *tuiModel, _ []string) tea.Cmd {
			if !m.proc.controlCmd("close", "") {
				m.append(stErr.Render("no engine right now — nothing closed"))
				return nil
			}
			m.append(stDim.Render("closing…"))
			return nil
		}},
		{"reconnect", "", "restart the engine (after it gave up), keeping this session", func(m *tuiModel, _ []string) tea.Cmd {
			m.restarts, m.restartStart = 0, time.Now()
			m.fatalErr = ""
			m.exitAnnounced = false
			m.skipHistory = m.everUp
			m.append(stDim.Render("reconnecting…"))
			return restartEngine(m.opts, m.sessionID, m.everUp)
		}},
		{"rag", "[on|off|restart|auto|manual]", "docs index (raglit): bare shows status, auto starts it every session", serverSlash("rag")},
		{"lsp", "[on|off|restart|auto|manual]", "code intelligence (poly-lsp-mcp): bare shows status, auto starts it every session", serverSlash("lsp")},
		{"mcp", "[restart [server]]", "every MCP server at once: bare lists them, restart bounces the running ones", mcpSlash},
		{"docker", "[on|off|status]", "exec backend: on (Docker), off (host), bare shows status", controlSlash("docker")},
		{"worktree", "[status|new|commit]", "git: bare shows status (in place too), new isolates in a worktree, commit writes the message and asks", controlSlash("worktree")},
		{"ship", "[verify|push|pr]", "ship the worktree: fetch, rebase, run checks, push (default: push)", controlSlash("ship")},
		{"clear", "", "clear scrollback and start a fresh session log", func(m *tuiModel, _ []string) tea.Cmd {
			m.proc.sendReset()
			m.convo = nil
			m.cur = ""
			m.pendingTool, m.pendingArgs = -1, nil
			m.sel, m.blockH = -1, nil
			m.busy, m.asking = false, false
			m.append(stDim.Render("session cleared — fresh start"))
			m.refresh()
			return nil
		}},
		{"exit", "", "exit dun", func(m *tuiModel, _ []string) tea.Cmd {
			m.quitting = true
			return tea.Quit
		}},
		{"suggest", "[on|off|auto]", "next-message suggestions: bare triggers one now, on/off/auto set the mode", suggestSlash},
	}
}

// serverSlash builds the /rag and /lsp handlers. The engine owns the work (it
// holds the harness); the TUI just forwards the action and renders the `server`
// event that comes back.
func serverSlash(alias string) func(*tuiModel, []string) tea.Cmd {
	return func(m *tuiModel, args []string) tea.Cmd {
		action := ""
		if len(args) > 0 {
			action = strings.ToLower(args[0])
		}
		if !m.proc.serverCmd(alias, action) {
			m.append(stErr.Render("no engine right now — /reconnect first"))
		}
		return nil
	}
}

// suggestSlash handles /suggest [on|off|auto].
// - Bare /suggest: trigger an immediate suggestion request.
// - /suggest on: always show suggestions (even while typing).
// - /suggest off: hide suggestions entirely.
// - /suggest auto: show suggestions only when idle with empty input (default).
func suggestSlash(m *tuiModel, args []string) tea.Cmd {
	if len(args) == 0 {
		// Bare /suggest: trigger an immediate suggestion request.
		if !m.proc.controlCmd("suggest", "") {
			m.append(stErr.Render("no engine right now — /reconnect first"))
		}
		return nil
	}
	mode := strings.ToLower(args[0])
	switch mode {
	case "on", "off", "auto":
		m.suggestMode = mode
		label := map[string]string{"on": "always", "off": "never", "auto": "when idle"}[mode]
		m.append(stDim.Render("suggestions: " + mode + " (" + label + ")"))
	default:
		m.append(stErr.Render("usage: /suggest [on|off|auto] (bare triggers one now)"))
	}
	return nil
}

// mcpSlash is /mcp. Where serverSlash binds ONE server per command, this one
// addresses the SET: bare lists every server, `restart` bounces the running
// ones, `restart <server>` bounces (or starts) just that one. It rides the same
// `server` event — the engine reads the allServers id as "all of them".
func mcpSlash(m *tuiModel, args []string) tea.Cmd {
	action, target := "", allServers
	if len(args) > 0 {
		action = strings.ToLower(args[0])
	}
	if len(args) > 1 {
		target = strings.ToLower(args[1])
	}
	if !m.proc.serverCmd(target, action) {
		m.append(stErr.Render("no engine right now — /reconnect first"))
	}
	return nil
}

// controlSlash builds the /docker and /worktree handlers. The TUI forwards the
// action to the engine via a `control` event and renders the response.
func controlSlash(id string) func(*tuiModel, []string) tea.Cmd {
	return func(m *tuiModel, args []string) tea.Cmd {
		action := ""
		if len(args) > 0 {
			action = strings.ToLower(args[0])
		}
		if !m.proc.controlCmd(id, action) {
			m.append(stErr.Render("no engine right now — /reconnect first"))
		}
		return nil
	}
}

// showConfig appends the running session's LLM settings to the conversation.
// The wizard (`dun --setup`) is a terminal flow — bubbletea owns stdin here — so
// this is read-only + a pointer to it.
func (m *tuiModel) showConfig() {
	key := "(none)"
	if m.keySet {
		key = "set"
	}
	var b strings.Builder
	b.WriteString(stHeader.Render("LLM settings"))
	b.WriteString("\n  " + stDim.Render("url:   ") + m.url)
	b.WriteString("\n  " + stDim.Render("model: ") + stTool.Render(m.model))
	b.WriteString("\n  " + stDim.Render("key:   ") + key)
	b.WriteString("\n  " + stDim.Render("saved: ") + configPath())
	b.WriteString("\n" + stDim.Render("use /model to switch models, or `dun --setup` to reconfigure"))
	m.append(b.String())
}

// showContext appends a detailed breakdown of context usage to the conversation.
func (m *tuiModel) showContext() {
	s := &m.ctxStats
	var b strings.Builder
	b.WriteString(stHeader.Render("context"))

	b.WriteString("\n\n  " + stHeader.Render("tokens"))
	b.WriteString("\n  " + stDim.Render("processed:  ") + fmt.Sprintf("%d", s.processedTokens))
	b.WriteString("\n  " + stDim.Render("generated:  ") + fmt.Sprintf("%d", s.generatedTokens))
	b.WriteString("\n  " + stDim.Render("cached:     ") + fmt.Sprintf("%d", s.cachedTokens))
	b.WriteString("\n  " + stDim.Render("total:      ") + fmt.Sprintf("%d", s.totalTokens))
	b.WriteString("\n  " + stDim.Render("active:     ") + fmt.Sprintf("%d", s.activeTokens) + stDim.Render(" (last turn's window)"))
	b.WriteString("\n  " + stDim.Render("turns:      ") + fmt.Sprintf("%d", s.turns))

	b.WriteString("\n\n  " + stHeader.Render("conversation"))
	b.WriteString("\n  " + stDim.Render("blocks:     ") + fmt.Sprintf("%d", len(m.convo)))
	b.WriteString("\n  " + stDim.Render("tools:      ") + fmt.Sprintf("%d", len(m.tools)))

	b.WriteString("\n\n  " + stHeader.Render("context shaping"))
	if s.compactions > 0 {
		b.WriteString("\n  " + stDim.Render("compactions: ") + fmt.Sprintf("%d", s.compactions) + stDim.Render(" · saved ") + fmt.Sprintf("%d tokens", s.tokensCompacted))
	} else {
		b.WriteString("\n  " + stDim.Render("compactions: ") + "0")
	}
	if s.recaps > 0 {
		b.WriteString("\n  " + stDim.Render("recaps:      ") + fmt.Sprintf("%d", s.recaps) + stDim.Render(" · ") + fmt.Sprintf("%d entries, %d chars → disk", s.entriesRecapped, s.charsRecapped))
	} else {
		b.WriteString("\n  " + stDim.Render("recaps:      ") + "0")
	}

	b.WriteString("\n\n  " + stHeader.Render("tool results"))
	b.WriteString("\n  " + stDim.Render("total:      ") + fmt.Sprintf("%d", s.toolResults))
	if s.resultsTruncated > 0 {
		b.WriteString("\n  " + stDim.Render("truncated:  ") + fmt.Sprintf("%d", s.resultsTruncated) + stDim.Render(" (LOD-capped at 4000 chars)"))
	} else {
		b.WriteString("\n  " + stDim.Render("truncated:  ") + "0")
	}

	b.WriteString("\n\n  " + stHeader.Render("system"))
	if s.systemTokens > 0 {
		b.WriteString("\n  " + stDim.Render("prompt + schemas:  ") + fmt.Sprintf("%d tokens", s.systemTokens) + stDim.Render(" (estimated)"))
	} else {
		b.WriteString("\n  " + stDim.Render("prompt + schemas:  ") + "not reported")
	}
	b.WriteString("\n  " + stDim.Render("Use DUN_CONTEXT_TOKENS to set a shaping budget (unset = no compaction)."))

	b.WriteString("\n\n  " + stHeader.Render("out-of-band"))
	b.WriteString("\n  " + stDim.Render("queued msgs:    ") + fmt.Sprintf("%d", s.oobMessages) + stDim.Render(" (delivered mid-turn)"))
	if s.notificationsSmuggled > 0 {
		b.WriteString("\n  " + stDim.Render("notifications:  ") + fmt.Sprintf("%d", s.notificationsSmuggled) + stDim.Render(" (smuggled via queue)"))
	}
	if s.forcedToolCalls > 0 {
		b.WriteString("\n  " + stDim.Render("forced calls:   ") + fmt.Sprintf("%d", s.forcedToolCalls) + stDim.Render(" (host-injected)"))
	}

	m.append(b.String())
}

// paletteActive reports whether the "/" command palette should be shown/driven.
func (m tuiModel) paletteActive() bool {
	return m.focus == focusInput && !m.asking && strings.HasPrefix(m.input.Value(), "/")
}

// paletteMatches returns the commands whose name starts with the typed word.
func (m tuiModel) paletteMatches() []slashCmd {
	word := strings.TrimPrefix(strings.Fields(m.input.Value() + " ")[0], "/")
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

// eofMsg says an engine's output ended. It names WHICH engine: deliberately
// closing one (a session switch, /reconnect) still produces an EOF from its
// reader goroutine, and a supervisor that cannot tell that from a crash will
// "restart" the engine you just replaced — measured as three restarts and a
// give-up in the middle of a working /resume.
type eofMsg struct{ proc *dunProc }

// engineUpMsg carries a respawned engine (or why it could not be respawned).
type engineUpMsg struct {
	proc *dunProc
	err  error
}

// Engine deaths worth surviving are the ones a session can outlive: a turn that
// took the process down, an OOM, a bad build. A crash LOOP is not — restarting
// forever would hide the real failure behind a flickering UI, so the attempts
// are capped per window and then reported.
const (
	engineRestartMax    = 3
	engineRestartWindow = 2 * time.Minute
)

// engineGone handles the engine's stdout closing. The TUI does not die with it:
// the conversation is on disk, so a fresh engine can reattach to the same
// session id and carry on with the scrollback still on screen.
func (m tuiModel) engineGone() (tea.Model, tea.Cmd) {
	m.busy, m.starting, m.asking = false, false, false
	m.busyStart = time.Time{}
	reason := m.fatalErr
	if reason == "" {
		reason = "dun engine exited"
	}
	// A finished trace is not a crash — there is nothing to restart.
	if m.replaying {
		m.fatalErr = "replay finished — /perf for the timings"
		if s := m.proc.replay.String(); s != "" {
			m.append(stDim.Render("replayed " + s))
		}
		m.refresh()
		return m, nil
	}
	// It left on purpose (or we are leaving): let it go.
	if m.quitting || m.reloadReq || m.exitAnnounced {
		m.fatalErr = reason
		m.refresh()
		return m, nil
	}
	if time.Since(m.restartStart) > engineRestartWindow {
		m.restarts, m.restartStart = 0, time.Now()
	}
	if m.restarts >= engineRestartMax {
		m.fatalErr = reason + " — gave up restarting it"
		m.append(stErr.Render(m.fatalErr + "\n/reconnect to try again · your conversation is saved: dun --continue"))
		m.refresh()
		return m, nil
	}
	m.restarts++
	m.fatalErr = ""
	m.skipHistory = true // it will replay what is already on screen
	m.append(stErr.Render(reason + " — restarting it; the session is kept"))
	m.refresh()
	return m, restartEngine(m.opts, m.sessionID, true)
}

// restartEngine spawns a replacement engine.
//
// reattach says whether there is a conversation to rejoin. It must be false for
// the very first engine: forcing --continue there would silently attach a fresh
// `dun -tui` to some unrelated older session, which is worse than the failure
// it is recovering from.
func restartEngine(o tuiOpts, sessionID string, reattach bool) tea.Cmd {
	return func() tea.Msg {
		if reattach {
			if sessionID != "" {
				o.resume, o.cont = sessionID, false
			} else {
				// The engine died before naming its session; the workspace's
				// most recent one is the one that just died.
				o.resume, o.cont = "", true
			}
		}
		p, err := startDunProc(o)
		if err != nil {
			return engineUpMsg{err: err}
		}
		return engineUpMsg{proc: p}
	}
}

// retryEngineMsg is a delayed retry after a failed spawn — an engine that
// cannot start usually cannot start a millisecond later either.
type retryEngineMsg struct{}

const engineRetryDelay = 2 * time.Second

type dunProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	ch     chan tea.Msg
	trace  *traceWriter // DUN_TRACE: records what the engine said, for --replay
	replay *replayStats // --replay: what the pacing did to the recording
}

// procArgs builds a `dun <mode>` argv from the shared flags. mode is "-p" (the
// engine, for the TUI) or "--tui" (the full TUI, for the xterm/PTY terminal
// view served at /term).
func procArgs(o tuiOpts, mode string) []string {
	args := []string{mode, "--workspace", o.workspace}
	if o.disableExit && mode == "--tui" {
		args = append(args, "--disable-exit")
	}
	if !o.suggest {
		args = append(args, "--no-suggest") // propagate to -p (engine) and web -tui
	}
	if o.rag != "" {
		args = append(args, "--rag="+o.rag)
	}
	if o.lsp != "" {
		args = append(args, "--lsp="+o.lsp)
	}
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
	if o.dockerNetwork {
		args = append(args, "--docker-network")
	}
	if o.worktree {
		args = append(args, "--worktree")
	}
	if o.pr {
		args = append(args, "--pr")
	}
	// Ship is on by default in the engine too, so the flag that has to survive
	// the re-exec is the OPT-OUT. Passing nothing when o.ship is false would
	// silently hand the child the default and undo --no-ship.
	if !o.ship {
		args = append(args, "--no-ship")
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
	cmd.Env = append(os.Environ(), "DUN_CHILD=1") // a spawned engine never self-rebuilds
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
	tw := newTraceWriter() // DUN_TRACE=path — see replay.go
	p := &dunProc{cmd: cmd, stdin: stdin, ch: ch, trace: tw}
	go func() {
		defer tw.close()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				tw.write(sc.Bytes())
				ch <- evMsg(ev)
			}
		}
		ch <- eofMsg{proc: p}
	}()
	return p, nil
}

// The proc methods are nil-safe: between an engine dying and its replacement
// coming up there is no engine, and the UI stays usable in that window. They
// report whether the message actually went anywhere.
func (p *dunProc) send(content string) bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]string{"type": "user", "content": content}) == nil
}

// sendReset tells the engine to clear its session store.
func (p *dunProc) sendReset() bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]string{"type": "reset"}) == nil
}

// serverCmd asks the engine to report on, start, or stop a tool server.
func (p *dunProc) serverCmd(alias, action string) bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]string{"type": "server", "id": alias, "action": action}) == nil
}

// controlCmd asks the engine to perform a control action (docker/worktree).
func (p *dunProc) controlCmd(id, action string) bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]string{"type": "control", "id": id, "action": action}) == nil
}

func (p *dunProc) answer(value string) bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]string{"type": "answer", "value": value}) == nil
}

func (p *dunProc) close() {
	if p == nil {
		return
	}
	p.trace.close()
	_ = p.stdin.Close()
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// waitEvent blocks for the next engine event and delivers it as a tea.Msg.
func waitEvent(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// ── frame pacing ───────────────────────────────────────────────────

// renderTickMsg drives the coalesced redraw (see the "token" event).
type renderTickMsg struct{}

// renderHz caps redraws while text streams in. A terminal cannot show more
// than this usefully, and every frame competes with keyboard input on the same
// Update loop — the measured failure was a session missing keystrokes while
// tokens arrived, with tmux itself perfectly responsive.
const renderHz = 30

func renderTick() tea.Cmd {
	return tea.Tick(time.Second/renderHz, func(time.Time) tea.Msg { return renderTickMsg{} })
}

// agentCmd asks the engine to view or steer a sub-agent (see agent scope).
func (p *dunProc) agentCmd(id int, action, content string) bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]string{
		"type": "agent", "id": strconv.Itoa(id), "action": action, "content": content,
	}) == nil
}
