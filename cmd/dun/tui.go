package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/cellbuf"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// The TUI is a CLIENT of the `-p` JSON event protocol: it re-execs `dun -p`,
// writes user events to its stdin, and renders the event stream. The engine
// stays headless; the UI is pure presentation.

// tuiOpts are the flags the TUI forwards to its `dun -p` subprocess.
// frame is one computed layout: the conversation's rendered rows plus the
// per-block geometry (rowOffset + height) and the viewport's window size. It is
// the SINGLE geometry truth — refresh() computes it, and everything that
// reads layout (View, convoHeight, applyScroll, the scroll overlay) reads from
// it. The old m.blockH / convoEntry.rowOffset / m.vp.Height were three parallel
// structures kept in sync by one writer and drifted in the reads; the model
// keeps only convoEntry.rowOffset (the per-entry record the trace/replay path
// parses back) and vp.Height (the pane's own dimension) — block heights and
// row offsets live ONLY here, in the frame.
//
// It is a value, not a pointer: a layout is a snapshot, and copying it is
// cheap (one []string header + two int slices). The model stores the last one
// (m.frame) so View — a value receiver — sees the same geometry the scroll
// policy last decided on, instead of re-deriving a fourth copy inline.
type frame struct {
	lines     []string // the wrapped convo display rows
	rowOffset []int    // row where block i begins
	blockH    []int    // rendered height of block i
	height    int      // rows the convo viewport gets
	width     int      // the wrap width this layout was built at
}

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
	// rag/poly-lsp are --rag/--lsp as typed: "" (unset, use the saved setting),
	// "true" or "false". Passed through to the -p engine verbatim.
	rag, lsp string
	// rephrase is --rephrase: rephrase each user prompt for specificity
	// before acting on it. Passed to the engine verbatim (a CLI flag), so it
	// survives a /reload or engine restart; /rephrase on|off flips the RUNTIME
	// copy, which wins over this at turn time.
	rephrase bool
	// fresh is --new (and /new): a new session even when the workspace has
	// one nobody is attached to. Without it a plain `dun` reuses that one.
	fresh bool
	// noDir is --no-dir: passed to the engine, whose workspace is dun's
	// directory for such sessions (dun/nodir.go).
	noDir bool
}

// runTUI launches the Bubble Tea app against a re-exec'd `dun -p` subprocess.
// lc (may be nil) is the launcher registration — its reload channel drives the
// "update ready" indicator + /reload.
func runTUI(o tuiOpts, lc *launcherConn) error {
	// Before the program starts, so the terminal query cannot block the input
	// loop or race the key reader for the reply bytes. See initMarkdownStyle.
	initMarkdownStyle()
	setupBand()           // after the style: the band's filter follows dark or light
	setupMascot()         // after the style: no colour, no foreman
	loadScriptRenderers() // ~/.dun/renderers/*.star override/extend the built-ins
	// The attach (dial dund, create or reuse the session, subscribe) runs
	// behind the running program, from Init: a new engine names its session
	// only once it is up, and the screen must not stay blank until then
	// (live: 54 s of a blank pane while poly-lsp indexed). The first frame
	// says what it waits for; ctrl+c works; a message typed meanwhile is
	// kept and sent at ready. An attach that fails is NOT a reason to refuse
	// to open either: the UI is where the error is legible and where
	// /reconnect lives. Init retries from inside.
	m := newTUIModel(nil, o.workspace)
	m.startingStart = time.Now()
	m.model, m.url, m.keySet = o.model, o.url, o.key != "" // for /config
	m.provider = hostName(o.url)
	if p, ok := loadConfig().providerAt(o.url); ok {
		m.provider = p.Name
	}
	m.disableExit = o.disableExit
	m.lc = lc
	m.opts = o // for respawning the engine (see eofMsg)
	m.checkUpdates = true
	// Alt screen, mouse mode and keyboard enhancements are View fields (View).
	// Bubble Tea v2 asks for the kitty keyboard protocol (flags=1) and xterm
	// modifyOtherKeys itself, decodes both, and puts the terminal back at exit.
	fm, err := tea.NewProgram(m).Run()
	if tm, ok := fm.(tuiModel); ok {
		tm.saveUI()     // the draft and sent history, for this user's next client
		tm.proc.close() // the engine it ENDED with, which may not be the one it started
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
			switch {
			case tm.sessionID != "":
				args = append(args, "--resume", tm.sessionID)
			case tm.proc != nil && tm.proc.session != "":
				// Re-exec'd before the session event (a new build seen on
				// the first attach): the connection names it.
				args = append(args, "--resume", tm.proc.session)
			case tm.opts.cont:
				args = append(args, "--continue")
			}
			e = syscall.Exec(exe, args, os.Environ())
		}
		if e != nil {
			fmt.Fprintf(os.Stderr, "dun: could not restart into %s: %v — run dun again\n", exe, e)
		}
	}
	if tm, ok := fm.(tuiModel); ok && !tm.reloadReq && err == nil {
		noteBackground(os.Stderr) // the first exit: what stays running (notices.go)
	}
	return err
}

// ── styles ─────────────────────────────────────────────────────────

// answerFG is the foreground of an answer's text: glamour's dark document
// colour, which every reply is rendered in.
var answerFG = *styles.DarkStyleConfig.Document.Color

var (
	stHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	// stDim is chrome: rules, arrows, key hints, labels. Not for text the
	// person reads to follow what happened (240 is 2.5:1 on a dark background).
	stDim = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
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
	stTool = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	stErr  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
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
		return stEdge.Render(strings.Repeat("▁", half-bump)+strings.Repeat("▄", bump)) +
			stDim.Render(strings.Repeat("▁", w-half))
	}
	bump = min(bump, w-half)
	return stDim.Render(strings.Repeat("▔", half)) +
		stEdge.Render(strings.Repeat("▀", bump)+strings.Repeat("▔", w-half-bump))
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

// convoEntry is one conversation block. A block with a full form opens and
// closes (enter, →/←); a relevant-docs summary carries a docsBlock (nested
// navigation); a thinking block or a run of tool calls is a box (box.go),
// and enter opens it in the pager.
type convoEntry struct {
	collapsed string
	full      string
	state     viewState   // minimized | expanded
	docs      *docsBlock  // proactive-RAG summary (nil for normal blocks)
	think     *thinkBlock // a round's reasoning (thinking.go) → enter opens the pager
	run       *toolRun    // a run of tool calls and thinking (toolrun.go) → enter opens the pager
	// box draws a thinking block or a run bounded, scrolling inside itself
	// (box.go). nil = not boxed.
	box *boxState
	// maxH is the most rows this block has drawn under maxKey (its wrap width
	// and view state): it is never drawn shorter, because the conversation's
	// height never goes down while a session runs (noshrink_test.go). A width
	// or a state change — a resize, the person opening or closing it — starts
	// it over.
	maxH   int
	maxKey int
	// askText is the question of an ask_user row; askOpen while it waits for
	// its answer (askpark.go).
	askText string
	askOpen bool
	// provisional marks a message that was typed mid-turn and is queued for
	// delivery. It appears in the conversation with a dimmed style so the user
	// knows it landed but hasn't been delivered to the model yet. When the turn
	// ends and the messages are delivered, they lose the provisional marker and
	// render as normal user messages.
	provisional     bool
	provisionalText string // original text, used to restore normal style on delivery
	userText        string // raw user message text (non-empty for user messages only)
	// stats is the cost of the turn this message started (turnstats.go):
	// shown under it when it is opened.
	stats      string
	needLogin  bool   // the "not logged in" line (onboarding.go): said once, where it was last needed
	adding     string // the server an "adding …" line is for: the result replaces it (noteServerMsg)
	splash     bool   // the splash or a /status block (splash.go): client-only, never in the session log
	liveSplash bool   // the splash at the top: drawn again from live state at every refresh
	startKept  bool   // a message typed before ready, shown until it is sent (startqueue.go)
	// hang: a line that wraps continues at column 2, under its text, not
	// under its mark (⚠ …, the kept-question line).
	hang bool

	// blank marks the empty row turnGap() appends when a turn ends, so
	// consecutive turns read as blocks. It renders as nothing.
	blank bool

	// mdSource is the RAW markdown an assistant block was rendered from. The
	// rendered string bakes in the word-wrap width that was current when it was
	// produced, and a pre-wrapped line has no break opportunity left for
	// cellbuf to find on a narrower pane — so a resize down leaves over-width
	// lines that the viewport silently truncates. Keeping the source lets
	// refresh() re-render through the (rebuilt) markdown renderer when the
	// width changes, the way userText already does for user messages. Empty
	// for blocks that are not rendered markdown.
	mdSource string
	// wide is the answer's sideways scroll, when a code block or table in it
	// is wider than the pane (wide.go). nil until one is.
	wide *wideState

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

func (e convoEntry) expandable() bool { return e.full != "" || e.docs != nil || e.stats != "" }

// deeper is the next view state that shows something NEW, for →: expanded,
// when the block has a full form. Next() wraps, which is right for enter (a
// cycle) and wrong for → (a direction): → must not close what it opened.
func (e convoEntry) deeper() (viewState, bool) {
	if e.state == viewMinimized && (e.full != "" || e.docs != nil || e.stats != "") {
		return viewExpanded, true
	}
	return e.state, false
}

// shallower is the inverse, for ←: one level out, stopping at minimized.
func (e convoEntry) shallower() (viewState, bool) {
	if e.state == viewExpanded {
		return viewMinimized, true
	}
	return e.state, false
}

// contextStats accumulates context usage data across the session for /context.
type contextStats struct {
	// Money and limits as the gateway reported them (cmd/dun/spend.go). nil
	// until the first usage or spend event.
	spend *proto.Spend

	// Token usage (cumulative across all turns)
	totalTokens     int
	activeTokens    int // last reported active window size
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
	toolResults      int // total tool results seen
	resultsTruncated int // how many were LOD-truncated

	// How the window is divided, and how well that division is understood. See
	// dun.WindowBudget: /context used to itemise the pre-conversation cost in
	// five rows and be unable to say how big the window was, so a reader could
	// see WHAT was expensive and never whether it was close to the wall.
	window         int
	windowReserved int
	windowCap      int
	promptBudget   int
	promptTokens   int
	promptOverhead int
	charsPerToken  float64
	ratioMeasured  bool
	ratioRounds    int
	windowCuts     int
	windowFolds    int

	// System prompt + tool schemas. systemExact says whether these were counted
	// by the model's own tokenizer or estimated at ~4 chars/token — the two must
	// never render alike.
	systemTokens int
	systemExact  bool
	systemPrompt int
	systemShared int
	systemParts  []dun.ContextPart
	// The project rules' share of systemPrompt and their /rules mode ("" =
	// no rules).
	systemRules     int
	systemRulesMode string
	// systemFiles are the context files in systemRules, each estimated.
	systemFiles []dun.ContextPart

	// Out-of-band delivery: messages typed mid-turn that were queued into
	// a running turn rather than starting a new one.
	oobMessages int

	// Notifications delivered via the queue (smuggled into tool results
	// rather than triggering their own turn).
	notificationsSmuggled int

	// Tool calls injected by the host (e.g. /ship) that appear as if the
	// model made them.
	forcedToolCalls int

	// Side calls — the LLM calls that are not the turn: /suggest predictions,
	// /rephrase rewrites, commit messages, rescue compactions. Per-kind running
	// totals carried on the usage event; the engine sends the whole map each
	// turn, so the read replaces rather than accumulates.
	sideCalls map[string]sideCallStats

	// Proactive RAG's cost, from the usage event's "surfacing" map (a session
	// total — the read replaces). nil until the first pass.
	surfacing map[string]float64

	// The prompt cache and prefix churn, from the usage event's "cache" map
	// (a session total — the read replaces). nil until the first build.
	cache map[string]any
}

// sideCallStats is one /context row: the per-kind totals for the calls that
// are not the conversation turn.
type sideCallStats struct {
	calls     int
	latencyMS int64
	lastMS    int64
	processed int
	cached    int
	generated int
	avgMS     int64
	tokPerS   float64
}

func (e convoEntry) view() string {
	if e.docs != nil {
		return e.docs.render(e.state > viewMinimized)
	}
	if e.state == viewExpanded && e.full != "" {
		return e.full
	}
	if e.state == viewExpanded && e.stats != "" {
		return e.collapsed + "\n" + e.stats
	}
	return e.collapsed
}

// docNode is one surfaced document inside a docsBlock; open shows its snippet.
type docNode struct {
	title, line string
	score       float64
	open        bool
	// source and why come from the engine ("" from an older one): where the
	// document came from, and why it won.
	source, why string
	kind        string
}

// docsBlock is a collapsed proactive-RAG summary ("N relevant · M surfaced").
// Expanding (enter) reveals the surfaced docs; → descends into the list, where
// ↑/↓ move between docs, enter expands a doc's snippet, ← / esc ascends.
type docsBlock struct {
	found, surfaced int
	tokens          int // estimated size of the appended entry; 0 from an older engine
	docs            []docNode
	descended       bool // focus is inside the doc list
	cur             int  // selected doc when descended
}

// docsBriefMax is the most the collapsed row adds for the documents; a
// longer list is left to the expanded block.
const docsBriefMax = 60

// brief names the surfaced documents on the collapsed row, by kind, each
// lone candidate marked: " · skill docs (only candidate), memory stash";
// by kind alone when the titles do not fit (" · skill, memory · 1 only
// candidate"). "" when neither fits, or from an older engine that sends no
// kind.
func (d *docsBlock) brief() string {
	var long, kinds []string
	lone := 0
	for _, doc := range d.docs {
		if doc.kind == "" {
			return ""
		}
		p := doc.title
		if !strings.HasPrefix(p, doc.kind+" ") {
			p = doc.kind + " " + p
		}
		if strings.HasPrefix(doc.why, "only candidate") {
			p += " (only candidate)"
			lone++
		}
		long, kinds = append(long, p), append(kinds, doc.kind)
	}
	short := strings.Join(kinds, ", ")
	if lone > 0 {
		short += fmt.Sprintf(" · %d only candidate", lone)
	}
	for _, s := range []string{strings.Join(long, ", "), short} {
		if s != "" && len([]rune(s)) <= docsBriefMax {
			return " · " + s
		}
	}
	return ""
}

func (d *docsBlock) render(open bool) string {
	glyph := "▸ "
	if open {
		glyph = "▾ "
	}
	label := fmt.Sprintf("%s🔎 %d relevant doc(s) · %d surfaced", glyph, d.found, d.surfaced)
	if d.tokens > 0 {
		label += commaf(" · ~%d tok", d.tokens)
	}
	if !open {
		label += d.brief()
	}
	head := stNote.Render(label)
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
		why := doc.why
		if why == "" {
			why = fmt.Sprintf("score %.2g", doc.score)
		}
		lines = append(lines, fmt.Sprintf("   %s%s %s  %s", cursor, dg, title, stDim.Render("("+why+")")))
		if doc.source != "" {
			lines = append(lines, stDim.Render("        "+doc.source))
		}
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
	convo       []convoEntry // finalized conversation blocks
	pendingTool int          // index of a tool call awaiting its result; -1 = none
	// activeTool is the in-flight tool call rendered into the status line so
	// "working…" says WHAT is running, not just that something is. Set on
	// tool_call, cleared on tool_result. Empty while the model is thinking
	// (no tool in flight) — then the line falls back to plain "working…".
	activeTool string
	cur        string // streaming assistant text (not yet finalized); string, not
	//                    strings.Builder — Bubble Tea copies the model each Update.
	tools         []string
	branch        string       // worktree branch (from the `workspace` event)
	starting      bool         // spawning servers, before `ready`
	startingStart time.Time    // when the starting phase began
	busy          bool         // a turn in flight
	interrupting  bool         // an interrupt was sent; the turn has not ended yet
	confirm       confirmState // a pending y/n question (jobkill.go)
	busyStart     time.Time    // when the current busy turn started
	asking        bool         // agent is waiting on an ask_user answer
	askOptions    []string     // the offered options; a trailing "custom" row is implicit
	askRec        []string     // the options the model recommends (marked, preselected)
	askSel        int          // highlighted answer row (== len(askOptions) → the custom row)
	askNote       string       // optional detail attached to the chosen option ("n")
	askMulti      bool         // multi-select: space toggles, enter submits the checked set
	askChecked    []bool       // per-option checked state (multi mode; len == len(askOptions))
	// askEdit is the option the text field edits in multi mode: an index, or
	// len(askOptions) for a new one (the custom row); enter keeps it,
	// checked, instead of sending (USER 2026-10-27).
	askEdit      int
	noting       bool           // capturing a detail for the selected option
	customAnswer bool           // capturing a free-text / chat answer
	askDraft     multilineInput // the composer as it was when the question took the keys
	askDraftSet  bool           // askDraft holds a draft to give back (restoreDraft)
	convoHold    int            // the convo pane keeps at least this many rows (holdConvo)
	convoHoldW   int            // the pane width convoHold was measured at
	paneTermH    int            // the terminal height the pane was last fitted at (a resize, not a panel)
	holdSkip     bool           // the next refresh replaces the conversation: no contract hold (dropHold)
	// panelHeld: a panel below (palette, form, picker) shrank the pane and
	// the pinned view keeps panelYOff while it is open and the conversation
	// is no longer than panelRows (applyScroll).
	panelHeld   bool
	panelYOff   int
	panelRows   int
	panelFollow bool                  // new rows arrived with a panel open, this update: a shrink keeps them in view
	md          *glamour.TermRenderer // markdown renderer for assistant replies
	mdWidth     int                   // width md was built for (rebuild only on change)
	history     []string              // sent inputs, recalled with ctrl+↑/↓
	histIdx     int                   // cursor into history (== len when not browsing)
	mouseMode   string                // "all" | "cell" | "off" — what /mouse last set
	focus       int                   // focusInput | focusConvo | focusActivity
	task        string                // the last user message, for the task line
	agents      []agentRow            // live sub-agents (activity.go)
	jobs        []jobRow              // live background jobs (activity.go)
	actLevel    int                   // actCollapsed | actList — → descends, ← ascends
	actSel      actKey                // selected activity row, by identity not index
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
	frame        frame           // the last computed layout snapshot — the single geometry truth
	inspecting   bool            // the tool inspector overlay is open (owns all keys)
	insp         inspector       // the overlay (valid while inspecting)
	paging       bool            // the pager has the convo pane (pager.go; owns the keys)
	pulse        int             // the selection band's pulse step (band.go)
	drag         mouseSel        // a left-button click or drag in the convo pane (mouse.go)
	curMaxH      int             // maxH for the streaming reply (see convoEntry.maxH)
	curMaxKey    int
	askParked    bool           // a question is open but its picker was put away (askpark.go)
	flashText    string         // a short status-line note (askpark.go)
	flashUntil   time.Time      // when it goes
	pulsePending bool           // a pulse tick is scheduled
	bandRaw      []string       // the selected block's rows before the band, for a pulse step
	bandAt       int            // the pane row they start at
	pg           pager          // the pager (valid while paging)
	scr          *paneScreen    // a pushed screen over the pager (panes.go), or nil
	extWant      bool           // /ext asked for the list (extview.go)
	ctxSettings  map[string]any // /context config's settings in force, from the settings event
	effort       string         // /effort's level, from the settings event
	jobViewing   bool           // a job's view has the convo pane (jobview.go; owns the keys)
	jv           jobView        // the job view (valid while jobViewing)
	jvGen        int            // which opening of the job view its read timer belongs to
	dumpSig      chan os.Signal // SIGUSR1 → dump the rendered screen to a debug file
	paletteSel   int            // highlighted row in the "/" command palette
	model, url   string         // this session's LLM settings (for /config)
	keySet       bool           // whether an API key is configured
	// First run (onboarding.go): a message sent before any login is held,
	// not sent, and comes back in the composer once a model is picked;
	// switchNote is the line the restart after a pick confirms it with;
	// quietReplay drops that restart's "resumed" marker; hintShown is the
	// off-servers hint, said once per TUI. noLogin: the engine's settings
	// said it has no AI service.
	noLogin bool
	heldMsg string
	// startQueue is what was typed before the session was ready, sent at
	// its settings (startqueue.go). srvSince is when each server still
	// starting began, on this clock, for the status line's elapsed time.
	startQueue  []keptMsg
	srvSince    map[string]time.Time
	idxSince    map[string]time.Time // when each running server's index began building, on this clock
	srvOrder    []string             // server ids in the order first seen: the splash keeps them there (noteServerOrder)
	restoredMsg string               // the held message, put back in the composer: the status line says whose it is while it is unchanged
	switchNote  string
	quietReplay bool
	hintShown   bool
	disableExit bool          // --disable-exit: ctrl+c/esc don't quit (use /exit)
	lc          *launcherConn // launcher registration (nil = no launcher)
	reloadVer   string        // a newer build the launcher announced ("" = none)
	reloadReq   bool          // /reload requested → runTUI re-execs after quit
	upgrade     *upgradeState // a pending dund upgrade (upgrade.go); nil = none
	// checkUpdates: a release build's daily check runs behind the first
	// frame (release.go); set by runTUI only.
	checkUpdates bool
	// Engine supervision: the TUI outlives its engine. opts is what respawning
	// one takes, sessionID reattaches it to the same conversation, and the
	// restart counters stop a crash loop from spinning forever.
	opts         tuiOpts
	sessionID    string
	restarts     int
	restartStart time.Time
	// A reconnect that fails (dund not back yet) is retried with backoff
	// until reconnectFor has passed since reconnSince; reconnTries counts
	// the attempts, for the status line.
	reconnTries int
	reconnSince time.Time
	skipHistory bool      // a respawned engine replays what is already on screen
	ctrlCAt     time.Time // the last ctrl+c that interrupted a turn (a second one quits)
	// lastClickAt/lastClickLine: the last plain click in the conversation,
	// to tell a double click (mouse.go).
	lastClickAt   time.Time
	lastClickLine int
	offlineShown  bool // the saved conversation was shown from disk while dund was unreachable
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
	// shown is the last frame View returned, and evlog the last messages
	// Update handled: what /repaint captures (repaint.go). Pointers, for the
	// reason vpc is one.
	shown *shownFrame
	evlog *eventRing
	// reportDir is the capture a /repaint report is waiting on the model for.
	reportDir string

	renderDue   bool              // streamed text arrived; a tick will draw it
	tickPending bool              // a render tick is already scheduled
	picking     bool              // the session picker owns the keys
	sessions    []dun.SessionInfo // what it is listing
	sessWS      map[string]string // id → workspace, for sessions dund has elsewhere
	sessLive    map[string]bool   // id → its engine is running in dund
	pickSel     int               // highlighted session
	// Model picker (/model): fetch available models, let the user switch.
	modelPicking bool       // the model picker owns the keys
	models       pickList   // its search box and highlighted row
	modelRows    []modelRow // every provider's models fetched so far
	modelErrs    []string   // providers that returned no list
	modelPending int        // providers not yet answered
	// The login's service picker (/login): custom, then the templates.
	loginPicking bool
	logins       pickList
	loginProbe   map[string]int // local template id → models it answered with; absent: not answering
	// The add-provider form (/model add, ctrl+a in the picker).
	providerForm     bool
	providerFields   [3]textinput.Model // address, name, key (fieldAddr, fieldName, fieldKey)
	providerStep     int
	providerErr      string
	providerAuto     string     // the name the form filled in from the address; replaced while it is unedited
	providerEdit     string     // the provider the form edits; "" adds one
	providerChecking string     // the service being checked, asked for its models before it is saved (saveProvider)
	providerPending  providerOp // what is saved once the check passes
	providerTmpl     *loginTmpl // the template the form was opened from; nil: custom, or again
	providerEnvKey   string     // the template's key variable that is set; "" none
	providerBack     bool       // esc goes back to the service picker
	provider         string     // name of the provider the engine runs on
	// /raglit setup (ragsetup.go); nil when it is not open.
	rag *ragSetup
	// The add-MCP-server form (/mcp add).
	mcpForm      bool
	mcpFields    [mcpRows]textinput.Model // name, url or command, token or env, then oauth (see mcpform.go)
	mcpStep      int                      // the field with the cursor
	mcpErr       string
	mcpAuto      string // the name filled in from the server row; follows it while unedited (mcpFollowName)
	mcpToolsWant string // the server whose /mcp tools this TUI asked for (mcptools.go)
	switchesWant string // the kind whose switch list this TUI asked for (switchlist.go)
	// mcpServersWant: this TUI asked for the /mcp table (mcpview.go).
	mcpServersWant bool
	engineCmds     []engineCmd       // the engine's slash commands (commands event)
	skills         []catalogSkill    // from the engine's catalog event
	serverIDs      []string          // every MCP server's id (ready/servers events), for /mcp completion
	servers        []dun.ServerState // the last server states (ready/servers events), for the splash
	// The splash (splash.go): splashWant shows it at the next settings event
	// (a fresh session, or a model pick that restarted under one);
	// splashKeep keeps it through that restart's history; splashDone: a
	// session already had one.
	splashWant, splashKeep, splashDone bool
	prompts                            []catalogPrompt // MCP prompts, each a slash command
	replaying                          bool            // driven by a trace (--replay), not an engine
	liveTurnDone                       bool            // a turn finished while this TUI watched: idle suggestions may ask
	quitting                           bool            // the user is leaving; do not respawn
	exitAnnounced                      bool            // the engine said it was going; it did not crash
	everUp                             bool            // an engine reached `session` once; a retry may reattach
	suggestions                        []suggestion    // predicted next messages (idle-only picker)
	suggestMode                        string          // "on" | "off" | "auto" — /suggest controls this
	suggestSel                         int             // highlighted suggestion in the selector
	// Idle debounce for the suggestion request. The engine cannot see whether
	// anyone is typing, so the decision lives here: the clock restarts on every
	// keystroke and at the end of every turn, and the request goes out only once
	// the person has actually stopped. See idleSuggestDelay.
	lastKeyAt         time.Time // last keystroke OR turn end — the idle clock
	idleTickPending   bool      // a debounce tick is already scheduled
	idleWantTick      bool      // an event asked for one (handleEvent has no tea.Cmd)
	suggestedThisIdle bool      // already asked during this idle; do not ask twice
	retry             string    // live retry banner ("" = not waiting on the provider)
	retryDue          time.Time // when the next attempt is due, for the countdown
	retrySeen         int       // retries this outage; the first one also lands in scrollback
	queuedMsgs        int       // messages typed mid-turn, buffered for the running turn
	w, h              int
	fatalErr          string
	scrollPinned      bool     // true when viewport should auto-follow (at bottom)
	traceFile         *os.File // /trace on: recording events+scroll to this file
	tracePrevYOff     int      // last recorded YOffset (avoid duplicates)
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
	return tuiModel{proc: proc, workspace: workspace, vpc: &viewportCache{}, input: in, search: se, spin: sp, dumpSig: sig, starting: true, sel: -1, pendingTool: -1, scrollPinned: true, suggestMode: "auto", mouseMode: "all", shown: &shownFrame{}, evlog: newEventRing()}
}

func (m tuiModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick, m.input.BlinkTick(), waitForDump(m.dumpSig), waitReload(m.lc)}
	if m.checkUpdates {
		cmds = append(cmds, func() tea.Msg { return updateNoteMsg(releaseUpdate()) })
	}
	if m.proc != nil {
		cmds = append(cmds, waitEvent(m.proc))
	} else {
		// No session yet: attach from here, behind the first frame (runTUI).
		cmds = append(cmds, restartEngine(m.opts, "", false, resumeAt{}))
	}
	return tea.Batch(cmds...)
}

// reloadMsg carries a newer build version the launcher announced.
type reloadMsg string

// updateNoteMsg is the release check's line ("" for none).
type updateNoteMsg string

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
	msg = normalizeKey(msg)
	kind := msgKind(msg)
	m.evlog.add(start, kind, msg) // for /repaint (repaint.go)
	nm, cmd := m.update(msg)
	if frames.observeMsg(stageUpdate, time.Since(start), kind) {
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
		switch msg.(type) {
		case tea.KeyPressMsg, tea.PasteMsg:
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
			resized := t.h != t.paneTermH // the terminal, not a panel below the pane
			t.paneTermH = t.h
			if got := t.convoHeight(); got != t.vp.Height {
				selMode := t.focus == focusConvo && !t.asking && len(t.convo) > 0
				// ANCHOR THE BOTTOM across a height change.
				//
				// YOffset is the TOP visible row, so leaving it alone across a
				// resize pins the top and lets the bottom move by the height
				// delta. On a phone the soft keyboard opening and closing IS a
				// height change, and it fires constantly — so a reader parked on
				// the last few done messages watched them slide out from under
				// the fold every time the keyboard appeared.
				//
				// Pinned readers are handled by applyScroll below (it follows the
				// bottom already), and a selection owns its own positioning; this
				// is for the reader who has scrolled somewhere deliberately and
				// expects to still be looking at it.
				//
				// A pane that GROWS because something below it closed — a
				// panel, a form, the palette — keeps its top row instead
				// (holdTop): the rows on screen stay where they are and blank
				// rows take the new space at the bottom until content fills
				// it. A terminal resize (the keyboard) keeps the bottom.
				//
				// A pane that SHRINKS while it holds blank rows at the bottom
				// (a palette narrowed, then a form opened) gives up those
				// blank rows first: the top row stays and the hold is cut to
				// the new bottom of the view. Anchoring the bottom there
				// pushed the text up and out of the view and left the pane
				// blank (48×24 walk, 2026-10-23).
				//
				// A pane that SHRINKS because a panel opened below it (the
				// palette, a form, a picker) keeps its top row too: the panel
				// takes the blank rows at the bottom first, and where there
				// are not enough, the newest rows go under it — the header and
				// the splash stay (USER 2026-10-23, run 4 at 49×21: the
				// conversation moved up six rows and the header never came
				// back). panelHold keeps the pinned view there until the panel
				// closes or new rows arrive (applyScroll).
				if got < t.vp.Height && t.panelOpen() && t.scrollPinned && !selMode && !resized {
					yoff := t.vp.YOffset
					if t.panelFollow {
						// Rows that came in this update are followed to the
						// bottom of the smaller pane, not covered.
						yoff = max(0, len(t.vp.lines)-got)
					}
					before := len(t.vp.lines)
					if t.convoHold > 0 {
						t.convoHold = yoff + got
					}
					t.vp.setHeight(got)
					if t.convoHold > 0 {
						t.refresh()
					}
					t.vp.SetYOffset(yoff)
					if !t.panelHeld {
						t.panelHeld, t.panelYOff = true, yoff
						t.panelRows = len(t.vp.lines)
					} else {
						// Held already (one panel gave way to another): only the
						// hold's blank rows are not new. A line that came with
						// the change — the login notice, as the form became the
						// picker — is new, and is followed (applyScroll); taking
						// it into panelRows put it under the picker (run 5,
						// 49×21: "· key set").
						t.panelRows += len(t.vp.lines) - before
					}
				} else if got > t.vp.Height && t.vp.YOffset > 0 && !selMode && !resized {
					t.holdTop(got)
				} else if got < t.vp.Height && t.convoHold > 0 && !selMode && !resized {
					yoff := t.vp.YOffset
					t.convoHold = yoff + got
					t.vp.setHeight(got)
					t.refresh()
					t.vp.SetYOffset(yoff)
				} else if !t.scrollPinned && !selMode {
					bottom := t.vp.YOffset + t.vp.Height
					t.vp.setHeight(got)
					t.vp.SetYOffset(bottom - got)
				} else {
					t.vp.setHeight(got)
				}
				t.applyScroll(selMode)
			}
			// A panel that closed has given its rows back by now (holdTop
			// above kept the top row): its hold is done.
			if t.panelHeld && !t.panelOpen() {
				t.panelHeld = false
				// Rows that came with the close (an "adding …" line) are
				// followed now, as far as they reach and no further.
				t.applyScroll(t.focus == focusConvo && !t.asking && len(t.convo) > 0)
			}
		}
		t.panelFollow = false // for this update only (applyScroll)
		// The block being paged may have grown, and the pane may have moved.
		t.syncPager()
		t.syncJobView()
		// Out of the conversation, out of any box in it (however focus moved).
		if t.focus != focusConvo && t.sel >= 0 && t.sel < len(t.convo) && t.convo[t.sel].box != nil && t.convo[t.sel].box.inside {
			leaveBox(t.convo[t.sel].box)
		}
		if t.focus != focusConvo && t.sel >= 0 && t.sel < len(t.convo) && t.convo[t.sel].wide != nil {
			t.convo[t.sel].wide.inside = false
		}
		// The pulse clock runs while a band is on screen, and only then.
		if t.pulseWanted() && !t.pulsePending {
			t.pulsePending = true
			cmd = tea.Batch(cmd, pulseTick())
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
		m.search.SetWidth(msg.Width - 4)
		// Only when the WIDTH actually changed: a renderer is word-wrap state
		// and nothing else, and rebuilding one is not free. A height-only
		// resize (the common one — an ask panel growing) rebuilt it for nothing.
		if m.md == nil || m.mdWidth != msg.Width {
			// Prose wraps at the question band's edge (bandCap): a 170-column
			// line on a wide screen is hard to follow back to its start.
			m.md, m.mdWidth = newMarkdown(min(msg.Width, bandCap)-2), msg.Width
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
		m.vp.setHeight(m.convoHeight())
		m.refresh()
		return m, nil

	case loginProbeMsg:
		if m.loginPicking {
			m.loginProbe[msg.id] = msg.n
			m.refresh()
		}
		return m, nil

	case modelFetchMsg:
		if m.modelPicking && !m.providerForm && !m.loginPicking {
			m.addModelRows(msg)
			m.refresh()
		}
		return m, nil

	case loginCheckedMsg:
		return m, m.loginChecked(msg)

	case ragLoadedMsg:
		m.ragLoaded(msg)
		return m, nil
	case ragModelsMsg:
		m.ragModels(msg)
		return m, nil
	case ragSavedMsg:
		m.ragSaved(msg)
		return m, nil

	case tea.KeyPressMsg:
		// Any key clears a mouse highlight; esc does only that.
		if m.drag.on {
			shown := m.drag.shown()
			m.drag = mouseSel{}
			if shown && msg.String() == "esc" {
				return m, nil
			}
		}
		// ctrl+g while a turn runs interrupts it, whatever has the keys. Idle it
		// does nothing; ctrl+k stays the input's kill-to-end-of-line, busy or not.
		if msg.String() == interruptKey && m.turnRunning() {
			return m.interrupt()
		}
		// ctrl+c while a turn runs interrupts it too; a second ctrl+c within
		// quitAgain falls through to the quit below (USER 2026-10-22).
		if msg.String() == "ctrl+c" && m.turnRunning() && time.Since(m.ctrlCAt) > quitAgain {
			m.ctrlCAt = time.Now()
			return m.interrupt()
		}
		// A y/n question (kill job N?) takes the next key, whatever it is.
		if m.confirm.q != "" {
			return m.updateConfirm(msg)
		}
		// The tool inspector overlay owns every key while open.
		if m.inspecting {
			open, cmd := m.insp.update(msg)
			m.inspecting = open
			return m, cmd
		}
		// So does a job's view, all but ctrl+c (jobview.go).
		if m.jobViewing && msg.String() != "ctrl+c" {
			open, cmd := m.jv.key(msg)
			if !open {
				m.jobViewing = false
				m.refresh()
			}
			return m, cmd
		}
		// A pushed screen (panes.go) has every key but ctrl+c: tab moves
		// between its panes, the focused one takes the rest.
		if s := m.activeScreen(); s != nil && msg.String() != "ctrl+c" {
			s.key(&m, msg)
			if m.paging {
				m.syncPager()
			}
			return m, nil
		}
		m.scr = nil
		// So does the pager, all but ctrl+c: it reads one block in the convo
		// pane's place, and esc brings the conversation back.
		if m.paging && msg.String() != "ctrl+c" {
			if v := m.toolsPaging(); v != nil && (msg.String() == "space" || m.toolKey(v, msg.String()) || m.filterKey(v, msg.String(), msg.Text)) {
				if msg.String() == "space" {
					m.toggleMCPTool(v)
				}
				return m, nil
			}
			if v := m.switchesPaging(); v != nil && m.switchKey(v, msg.String(), msg.Text) {
				return m, nil
			}
			if v := m.serversPaging(); v != nil && m.serverKey(v, msg.String()) {
				m.refresh()
				return m, nil
			}
			// "i" on a call opens the inspector over the pager; esc there comes
			// back here.
			if n := m.pg.curNode(); msg.String() == "i" && m.pg.tree() && n != nil && n.insp != nil {
				m.insp = newInspector(n.insp.name, n.insp.input, n.insp.output)
				m.insp.setSize(m.w, m.h)
				m.inspecting = true
				return m, nil
			}
			if !m.pg.key(msg.String()) {
				m.paging = false
				m.refresh()
			}
			return m, nil
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
		// So does the add-MCP-server form.
		if m.mcpForm {
			return m.updateMCPForm(msg)
		}
		// So does /raglit setup.
		if m.rag != nil {
			return m.updateRagSetup(msg)
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
		// Descended into a box, the keys scroll it (box.go); into an answer's
		// wide blocks, they scroll them sideways (wide.go).
		if m.insideKeys(msg.String()) || m.insideWideKeys(msg.String()) {
			m.refresh()
			return m, nil
		}
		// y yanks the selected block's text to the clipboard (vim's yank).
		if msg.String() == "y" && m.focus == focusConvo && m.sel >= 0 && m.sel < len(m.convo) {
			if text := yankText(m.convo[m.sel]); text != "" {
				return m, copyCmd(text)
			}
			return m, nil
		}
		// l descends into the selected box, like →.
		if msg.String() == "l" && m.selBox() != nil {
			m.selBox().inside = true
			m.refresh()
			return m, nil
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
			// The conversation's selection is a level too: esc drops it and
			// gives the keys back to the input (USER 2026-10-22 — the backout
			// after a click or ←). Inner levels (a box, docs, search) closed
			// above.
			if m.focus == focusConvo && m.insideBox() == nil {
				m.setFocus(focusInput)
				m.sel = -1
				m.refresh()
				return m, textinput.Blink
			}
			// Agent scope is the outermost thing esc can be inside. Quitting dun
			// from within a child's conversation is not "one level out" by any
			// reading, and it was what esc did.
			if m.scopeAgent != 0 {
				return m, m.leaveAgentScope()
			}
			// Nothing else wanted it, and a turn runs: esc stops the turn.
			if m.turnRunning() {
				return m.interrupt()
			}
			// esc is "one level out", and there is no level out of dun: it
			// says how to leave instead of leaving.
			return m, m.exitFlash()
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
				if cmd, l, ok := m.paletteArgs(); ok {
					if len(l.choices) > 0 {
						sel := min(max(m.paletteSel, 0), len(l.choices)-1)
						m.input.SetValue("/" + cmd + " " + l.choices[sel].value + " ")
						m.input.CursorEnd()
						m.paletteSel = 0
						m.refresh()
					}
					return m, nil
				}
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
		case "ctrl+end": // end a scroll-back: newest output, following again
			m.scrollToEnd()
			return m, nil
		case "ctrl+home": // top of the last user message: the current exchange
			m.scrollToLastUser()
			return m, nil
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
				// The open question, put away earlier, comes back.
				if m.askParked && m.sel >= 0 && m.sel < len(m.convo) && m.convo[m.sel].askOpen {
					return m.reopenAsk()
				}
				// The "not logged in" line (or the kept question's) does
				// what it says.
				if m.sel >= 0 && m.sel < len(m.convo) && m.convo[m.sel].needLogin && !m.loggedIn() {
					m.setFocus(focusInput)
					return m, m.openLogin("")
				}
				// Thinking (and anything else with a page) opens in the pager.
				if m.openPager(m.sel) {
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
			if msg.String() == "alt+enter" {
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
				return m, m.exit()
			}
			// Sending while a turn runs is allowed: the engine buffers the message
			// and lifts it into the next tool result, so the agent reads it inside
			// the turn it is already running. Before `ready` there is no engine
			// to receive it: it is kept, and sent at ready (startqueue.go).
			if v == "" {
				// Not logged in, enter on an empty composer is the login
				// the screen asks for (George, run 5: "make Enter open the
				// login form").
				if !m.loggedIn() && !m.starting && len(m.input.Images()) == 0 {
					return m, m.openLogin("")
				}
				return m, nil
			}
			if m.starting && m.scopeAgent == 0 {
				return m.keepUntilReady(v, m.input.Images()), nil
			}
			return m.sendUserImages(v, m.input.Images()), nil
		case "ctrl+up":
			// Ctrl+Up: navigate history (previous entry). The only way to reach
			// history — plain ↑ scrolls the conversation (see the ↑ case).
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
					m.sel = m.selSkipUp(m.sel - 1)
				}
				m.refresh()
				return m, nil
			}
			// One rule everywhere on this axis: if ↑ has somewhere to go INSIDE
			// the thing you are in, it goes there; at the edge it falls through
			// and scrolls the conversation. So the picker and the palette move
			// their selection until it reaches the top and then stop capturing
			// the key — reading the options is exactly when you want to scroll
			// back through what was said.
			if m.suggestActive() && m.suggestSel > 0 {
				m.suggestSel--
				m.refresh()
				return m, nil
			}
			if m.paletteActive() && m.paletteSel > 0 {
				m.paletteSel--
				m.refresh()
				return m, nil
			}
			// ↑ with messages queued for the running turn takes them back into
			// the composer, merged, to fix and send again (USER 2026-10-27).
			// Once: the queue is then empty, and the next ↑ is history or the
			// scroll, as before.
			if m.queuedMsgs > 0 && m.input.AtFirstLine() && !m.paletteActive() {
				if m.proc.controlCmd("unqueue", "") {
					m.queuedMsgs = 0
				}
				return m, nil
			}
			// ↑ on an EMPTY composer recalls the last thing sent, and on a
			// recalled one the one before — the shell's ↑ (USER 2026-10-23).
			// A composer with text of the person's own keeps the rule below.
			if m.histBrowsing() && m.histIdx > 0 {
				m.histIdx--
				m.input.SetValue(m.history[m.histIdx])
				m.input.CursorEnd()
				m.refresh()
				return m, nil
			}
			// Plain up moves within the composer until it runs out of buffer;
			// from the FIRST row it scrolls the CONVERSATION. History lives on
			// ctrl+↑/↓ instead, because of Termux: with mouse tracking inactive
			// on the alt screen — the state that lets a tap raise the soft
			// keyboard, see the mouse note in runTUI and TerminalView.doScroll —
			// a finger-scroll arrives as a RUN of plain ↑/↓ keys. Walking
			// history on those made every swipe stomp the composer; one viewport
			// row per key is exactly what the terminal is describing.
			if m.input.AtFirstLine() {
				m.vp.SetYOffset(m.vp.YOffset - 1)
				m.updateScrollPin()
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
					m.sel = m.selSkipDown(m.sel + 1)
				}
				m.refresh()
				return m, nil
			}
			// …and ↓ mirrors it: move within the picker/palette to the LAST
			// entry, then fall through and scroll (see the ↑ case).
			if m.suggestActive() && m.suggestSel < len(m.suggestions)-1 {
				m.suggestSel++
				m.refresh()
				return m, nil
			}
			if m.paletteActive() && m.paletteSel < m.paletteRows()-1 {
				m.paletteSel++
				m.refresh()
				return m, nil
			}
			// On a recalled entry, ↓ goes forward through history, past the
			// newest to an empty composer again.
			if m.histIdx < len(m.history) && m.histBrowsing() {
				m.histIdx++
				if m.histIdx == len(m.history) {
					m.input.SetValue("")
				} else {
					m.input.SetValue(m.history[m.histIdx])
					m.input.CursorEnd()
				}
				m.refresh()
				return m, nil
			}
			// …and from the LAST row, ↓ scrolls the conversation forward (see
			// the ↑ note above).
			if m.input.AtLastLine() {
				m.vp.SetYOffset(m.vp.YOffset + 1)
				m.updateScrollPin()
				return m, nil
			}
			m.input = m.input.HandleKey(msg)
			m.refresh()
			return m, nil
		case "right":
			// Inside the conversation → means DESCEND, uniformly. `▸` already
			// means "there is something inside this" on the activity tree and on
			// a docs list, and both are opened with →; a message wearing the same
			// glyph that could only be opened with enter was the odd one out.
			//
			// A block with nothing left to open stops →: arrows never change the
			// focus zone, only tab/shift+tab do (USER 2026-10-23 — "the movement
			// between panes is incredibly obnoxious"). → used to hand the keys
			// back to the input here, and ← at the input's front hopped to the
			// conversation, so an arrow held one press too long left the pane.
			if m.focus == focusConvo {
				if m.descendSel() {
					m.refresh()
				}
				return m, nil
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
				// Nothing to ascend out of: the edge. ← stops here rather than
				// moving to another zone (USER 2026-10-23, see the → case).
				return m, nil
			}
			// focusInput: left at the FRONT of the input, in a child's scope,
			// goes back to the session — scope is where the user lands on
			// descending, so it is where they press ← to undo it. That is a
			// level of the conversation, not a zone: the keys stay in the input.
			// Outside a scope the front of the input is an edge and ← stops.
			if m.input.Position() == 0 {
				if m.scopeAgent != 0 {
					return m, m.leaveAgentScope()
				}
				return m, nil
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
			// ctrl+v: an image on the clipboard goes in as an image token.
			// Text is pasted by the terminal, as before.
			if msg.String() == "ctrl+v" {
				return m, pasteImageCmd()
			}
			// ctrl+p: the clipboard's text, read by dun (USER 2026-10-27: no
			// paste key beside ctrl+k and ctrl+u); a terminal's own paste is
			// a PasteMsg, as before.
			if msg.String() == "ctrl+p" {
				return m, pasteTextCmd()
			}
			m.input = m.input.HandleKey(msg)
			m.paletteSel = 0 // typing re-filters the palette; start at the top
			m.refresh()
			return m, nil
		}

	case tea.PasteMsg:
		return m.paste(msg)

	case updateNoteMsg:
		// The release check's one line: a new build in place, or a refused one.
		if msg != "" {
			m.append(stNote.Render(string(msg)))
			m.refresh()
		}
		return m, nil

	case reloadMsg:
		m.reloadVer = string(msg) // header shows "↻ …"; /reload restarts
		m.refresh()
		return m, waitReload(m.lc) // re-arm

	case repaintNowMsg:
		return m, m.repaintNow(msg)

	case repaintDoneMsg:
		m.repaintDone(msg)
		return m, nil

	case dumpMsg:
		m.writeDump()
		return m, waitForDump(m.dumpSig) // re-arm for the next signal

	case procMsg:
		if msg.p != m.proc {
			// A connection this TUI has moved on from: none of its events are
			// this conversation's. Keep draining it, so its reader can finish,
			// until it ends.
			if _, end := msg.msg.(eofMsg); end {
				return m, nil
			}
			return m, waitEvent(msg.p)
		}
		return m.update(msg.msg)

	case evMsg:
		if msg["type"] == "self_reexec" && !m.replaying {
			// This binary on disk is the build dund runs now: quit and
			// re-exec into it on this session, as /reload does.
			m.reloadReq = true
			return m, tea.Quit
		}
		m.traceEvent(msg)
		nm := m.handleEvent(msg)
		cmds := []tea.Cmd{waitEvent(nm.proc)}
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

	case jobTailMsg:
		return m, m.jobTail(msg)

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
		// One this TUI closed itself (a session switch): the connection
		// ending was the point, even before its replacement is up.
		if msg.proc != nil && msg.proc.closed.Load() {
			return m, nil
		}
		return m.engineGone()

	case engineUpMsg:
		if msg.err != nil {
			// dund may be on its way back (a restart into a new build takes a
			// second; a crashed service is restarted by systemd after a few):
			// keep trying, more slowly, for reconnectFor.
			if m.reconnSince.IsZero() {
				m.reconnSince = time.Now()
			}
			// Nothing on screen yet (dund failed before the first attach): the
			// conversation being resumed is on disk — show it, read-only,
			// rather than an empty screen while this retries.
			if !m.everUp && !m.offlineShown {
				m.showSavedConversation()
			}
			m.reconnTries++
			if time.Since(m.reconnSince) >= reconnectFor {
				m.fatalErr = fmt.Sprintf("cannot reach the session service (dund) after %d attempts: %v", m.reconnTries, msg.err)
				m.append(stErr.Render(m.fatalErr + "\n/reconnect to try again · your conversation is saved: dun --continue"))
				m.reconnTries, m.reconnSince = 0, time.Time{}
				m.refresh()
				return m, nil
			}
			m.fatalErr = fmt.Sprintf("reconnecting… (attempt %d): %v", m.reconnTries+1, msg.err)
			if !m.everUp {
				m.fatalErr = fmt.Sprintf("starting session… (attempt %d): %v", m.reconnTries+1, msg.err)
			}
			m.refresh()
			return m, tea.Tick(reconnectDelay(m.reconnTries), func(time.Time) tea.Msg { return retryEngineMsg{} })
		}
		m.proc = msg.proc
		m.upgrade = nil // the dund reached says again if one is pending
		m.fatalErr = ""
		m.reconnTries, m.reconnSince = 0, time.Time{}
		// A reattach may replay from the middle of a session whose engine is
		// long up: no ready is coming. An engine that is starting says so
		// (engine "started"), and that is what waits for ready.
		if !msg.reattach {
			m.starting = true
			m.startingStart = time.Now()
		}
		m.refresh()
		return m, waitEvent(m.proc)

	case retryEngineMsg:
		return m, restartEngine(m.opts, m.sessionID, m.everUp, m.seen())

	case tea.MouseMsg:
		if m.paging {
			m.pg.mouse(msg)
			return m, nil
		}
		if m.jobViewing {
			m.jv.mouse(msg)
			return m, nil
		}
		// Clicks and drags (mouse.go); everything below is the wheel.
		if _, wheel := msg.(tea.MouseWheelMsg); !wheel {
			return m.mouseLeft(msg)
		}
		// The wheel scrolls a box only once you are inside it.
		if b := m.insideBox(); b != nil {
			switch msg.Mouse().Button {
			case tea.MouseWheelUp:
				b.scroll(-wheelDelta)
			case tea.MouseWheelDown:
				b.scroll(wheelDelta)
			}
			m.refresh()
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg) // wheel scrolls the conversation viewport
		m.updateScrollPin()
		return m, cmd

	case copiedMsg:
		return m, m.flash(copiedNote(msg))

	case upgradeSelfMsg:
		if msg.err != nil {
			m.append(stErr.Render("upgrade: " + msg.err.Error()))
		}
		return m, nil

	case clipTextMsg:
		if msg.err != nil {
			return m, m.flash(msg.err.Error())
		}
		return m.paste(tea.PasteMsg{Content: msg.text})

	case clipImageMsg:
		switch {
		case msg.err != nil:
			return m, m.flash(msg.err.Error())
		case msg.ok:
			m.input.InsertImage(msg.path, msg.name, msg.w, msg.h)
			m.refresh()
		}
		return m, nil

	case flashMsg:
		return m, nil // the redraw is the point: an expired note leaves the status line

	case pulseMsg:
		m.pulsePending = false
		if m.pulseWanted() {
			m.pulse++
			m.repaintBand()
		}
		return m, nil

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
		// /mcp's rows still starting turn with it.
		if v := m.serversPaging(); v != nil && v.starting() {
			v.spin = m.spin.View()
			m.syncPager()
		}
		if v := m.extPaging(); v != nil && v.starting() {
			v.spin = m.spin.View()
			m.syncPager()
		}
		// A server still starting: the live splash counts its seconds. The
		// row changes in place; refresh only when its words did.
		if len(m.srvSince)+len(m.idxSince) > 0 && len(m.convo) > 0 && m.convo[0].liveSplash &&
			m.splashView(m.splashW()) != m.convo[0].collapsed {
			m.refresh()
		}
		return m, cmd
	}
	return m, nil
}

// updateAsking drives the answer picker: ↑/↓ highlight an option (the last row
// is a custom/chat free-text answer), enter selects, `n` attaches a detail to
// the highlighted option, tab or → opens the option as a custom answer to edit.
// While typing a detail or a custom answer, keys go to the input; esc backs
// out of that sub-mode.
func (m tuiModel) updateAsking(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
		// One level out: from a detail or a custom answer back to the options,
		// and from the options out of the question — which stays open.
		if m.noting || (m.customAnswer && len(m.askOptions) > 0) {
			m.noting, m.customAnswer = false, false
			m.input.Reset()
			m.input.Blur()
			m.refresh()
			return m, nil
		}
		return m.parkAsk()
	case "enter":
		switch {
		case m.noting:
			m.askNote = strings.TrimSpace(m.input.Value())
			m.noting = false
			m.input.Reset()
			m.input.Blur()
			m.refresh()
			return m, nil
		case m.customAnswer && m.askMulti:
			// Multi: the field edits an option or adds one; enter keeps it,
			// checked, and goes back to the list. Done sends the checked set.
			v := strings.TrimSpace(m.input.Value())
			if v != "" {
				if m.askEdit >= 0 && m.askEdit < len(m.askOptions) {
					m.askOptions[m.askEdit], m.askChecked[m.askEdit] = v, true
					m.askSel = m.askEdit
				} else {
					m.askOptions = append(m.askOptions, v)
					m.askChecked = append(m.askChecked, true)
					m.askSel = len(m.askOptions) - 1
				}
			}
			m.customAnswer = false
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
			m.customAnswer, m.askEdit = true, custom
			m.input.Reset()
			m.input.placeholder = "type your answer, or chat…"
			if m.askMulti {
				m.input.placeholder = "another option: enter adds it, checked"
			}
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
			return m.sendAnswer(m.askAnswerAt(m.askSel)), nil
		}
	case "ctrl+end":
		m.scrollToEnd()
		return m, nil
	case "ctrl+home":
		m.scrollToLastUser()
		return m, nil
	// ↑/↓ follow the same rule here as everywhere else: move within the thing
	// you are in — the option list, or the caret when typing a detail/custom
	// answer — and at its edge fall through and scroll the conversation. A
	// question you cannot read the backlog behind is a question you cannot
	// answer, and the options are exactly when you want to look back.
	case "up":
		if m.noting || m.customAnswer {
			if !m.input.AtFirstLine() {
				break // let the caret move (handled below)
			}
		} else if m.askSel > 0 {
			m.askSel--
			m.refresh()
			return m, nil
		}
		m.vp.SetYOffset(m.vp.YOffset - 1)
		m.updateScrollPin()
		return m, nil
	case "down":
		if m.noting || m.customAnswer {
			if !m.input.AtLastLine() {
				break // let the caret move (handled below)
			}
		} else if m.askSel < maxRow {
			m.askSel++
			m.refresh()
			return m, nil
		}
		m.vp.SetYOffset(m.vp.YOffset + 1)
		m.updateScrollPin()
		return m, nil
	case "tab", "right":
		// Edit the highlighted answer before sending it: the custom field,
		// holding what enter would send from here. Esc goes back to the
		// options; nothing is sent.
		if !m.noting && !m.customAnswer && m.askSel < custom {
			m.customAnswer, m.askEdit = true, m.askSel
			m.input.Reset()
			if m.askMulti {
				// Edit this option's own text; enter keeps it, checked.
				m.input.SetValue(m.askOptions[m.askSel])
				m.input.CursorEnd()
				m.input.placeholder = "this option, as you want it"
				m.input.Focus()
				m.refresh()
				return m, textinput.Blink
			}
			m.input.SetValue(m.askAnswerAt(m.askSel))
			m.input.CursorEnd()
			m.input.placeholder = "type your answer, or chat…"
			m.input.Focus()
			m.refresh()
			return m, textinput.Blink
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
	// The custom row IS a text field, so typing on it answers — no enter first.
	// A no-options ask already drops straight into text entry
	// (TestTUI_AskNoOptionsFreeText); this makes the row that offers the
	// same thing behave the same way, and the key that opened the field is the
	// key that types into it. Enter still opens it empty, and every other row
	// keeps its bindings (`n` types an n HERE, but still starts a detail there).
	if !m.noting && !m.customAnswer && m.askSel == custom && typedText(msg) {
		m.customAnswer, m.askEdit = true, custom
		m.input.Reset()
		m.input.placeholder = "type your answer, or chat…"
		if m.askMulti {
			m.input.placeholder = "another option: enter adds it, checked"
		}
		m.input.Focus()
		m.input = m.input.HandleKey(msg)
		m.refresh()
		return m, textinput.Blink
	}
	if m.noting || m.customAnswer { // typing into the detail / custom field
		m.input = m.input.HandleKey(msg)
		return m, nil
	}
	return m, nil
}

// paste routes a bracketed paste to the text field that has the keys, the way
// a key is routed in update — but only ever as text: a paste is never a
// command, whatever it spells.
func (m tuiModel) paste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	m.drag = mouseSel{}
	var cmd tea.Cmd
	switch {
	case m.confirm.q != "", m.paging, m.picking:
		return m, nil
	case m.inspecting:
		if m.insp.searching {
			m.insp.search, cmd = m.insp.search.Update(msg)
		}
		return m, cmd
	case m.jobViewing:
		if m.jv.searching {
			m.jv.search, cmd = m.jv.search.Update(msg)
		}
		return m, cmd
	case m.asking:
		if !m.noting && !m.customAnswer {
			if m.askSel != len(m.askOptions) {
				return m, nil
			}
			// The custom row is a text field: pasting on it answers, as typing does.
			m.customAnswer = true
			m.input.Reset()
			m.input.placeholder = "type your answer, or chat…"
			m.input.Focus()
			cmd = textinput.Blink
		}
		m.input = m.input.HandlePaste(msg.Content)
		m.refresh()
		return m, cmd
	case m.mcpForm:
		return m.typeMCPField(msg)
	case m.rag != nil:
		return m.typeRagQuery(msg)
	case m.modelPicking && m.loginPicking:
		return m.typeLoginQuery(msg)
	case m.modelPicking && m.providerForm:
		return m.typeProviderField(msg)
	case m.modelPicking:
		return m.typeModelQuery(msg)
	case m.searching:
		return m.typeSearch(msg)
	case m.focus == focusConvo: // keys don't type into a blurred input
		return m, nil
	}
	m.input = m.input.HandlePaste(msg.Content)
	m.paletteSel = 0 // typing re-filters the palette; start at the top
	m.refresh()
	return m, nil
}

// askAnswerAt is the answer the option at row i stands for: the option and
// its detail, or in multi mode the checked options with this one among them.
func (m tuiModel) askAnswerAt(i int) string {
	if !m.askMulti {
		ans := m.askOptions[i]
		if m.askNote != "" {
			ans += " — " + m.askNote
		}
		return ans
	}
	var picked []string
	for j, opt := range m.askOptions {
		if j == i || (j < len(m.askChecked) && m.askChecked[j]) {
			picked = append(picked, opt)
		}
	}
	return strings.Join(picked, ", ")
}

// typedText reports whether a key is the user typing a character, as opposed to
// driving the UI. Alt- and ctrl-modified keys are commands, not text — except
// ctrl+tab, which is how a tab is typed (multiline_input.go).
func typedText(msg tea.KeyPressMsg) bool {
	if msg.String() == "ctrl+tab" {
		return true
	}
	return msg.Text != "" && !msg.Mod.Contains(tea.ModAlt) && !msg.Mod.Contains(tea.ModCtrl)
}

// normalizeKey folds the spellings of shift+enter into ctrl+j, the composer's
// "insert a newline" key, so it adds a spelling rather than a meaning.
//
// Terminals report shift+enter three ways. CSI 13;2u (kitty, foot, wezterm)
// and CSI 27;2;13~ (xterm modifyOtherKeys) decode as "shift+enter". The common
// one, SS3 M, is the keypad-enter code, which Bubble Tea names "enter" — left
// alone it would SEND the message. Bubble Tea never turns on application
// keypad mode, so a real keypad enter sends CR and arrives as plain enter.
func normalizeKey(msg tea.Msg) tea.Msg {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return msg
	}
	if (k.Code == tea.KeyKpEnter && k.Mod == 0) || (k.Code == tea.KeyEnter && k.Mod == tea.ModShift) {
		return tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}
	}
	return msg
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
// Not before a turn has finished in this TUI: opening or resuming a session
// and pressing a key is not "the human's turn now", and must not spend a
// model call (USER 2026-10-24).
func (m tuiModel) idleSuggestPossible() bool {
	return m.suggestMode != "off" && !m.quitting && m.liveTurnDone && m.asked()
}

// asked reports whether the conversation on screen holds a user message.
// Before one there is nothing to predict a next message from: a new session
// (or one just cleared) asks for no suggestions and shows none.
func (m tuiModel) asked() bool {
	for _, e := range m.convo {
		if e.userText != "" {
			return true
		}
	}
	return false
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
	if !m.asked() || !m.liveTurnDone { // nothing to predict from, or nothing new
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
	if m.suggestMode == "off" || !m.asked() {
		return false
	}
	return m.focus == focusInput && !m.busy && !m.starting && !m.asking &&
		len(m.suggestions) > 0 &&
		(m.suggestMode == "on" || strings.TrimSpace(m.input.Value()) == "")
}

// sendUser ships a user message to the engine (from the input or a suggestion)
// and clears transient UI (suggestions, input). The message is rendered when
// the engine's `user` event comes back, as it is on every other client.
func (m tuiModel) sendUser(v string) tuiModel { return m.sendUserImages(v, nil) }

// sendUserImages is sendUser for a message with the composer's images.
func (m tuiModel) sendUserImages(v string, imgs []proto.Image) tuiModel {
	// In agent scope the input steers that CHILD instead of the session. Same
	// gesture the parent's agent_monitor(tell:) makes, same code path in the
	// engine — there is one definition of what telling a child means.
	if m.scopeAgent != 0 && len(imgs) > 0 {
		// A child's tell is text: sent without its images, the message would
		// claim images the child never got. The draft stays.
		m.append(stErr.Render("images cannot be sent to a sub-agent — not sent"))
		m.refresh()
		return m
	}
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
	m.history = append(m.history, v)
	m.histIdx = len(m.history)
	m.input.Reset()
	m.restoredMsg = ""
	m.suggestions = nil
	m.saveUI()
	if m.replaying {
		m.append(stDim.Render("replaying a trace — there is no engine to send to"))
		m.refresh()
		return m
	}
	// Nothing to send to yet: held, and put back once a model is picked.
	if !m.loggedIn() && len(imgs) == 0 {
		m.holdUnsent(v)
		return m
	}
	if !m.proc.send(v, imgs) {
		m.append(stErr.Render("no engine right now — not sent. /reconnect, then send it again"))
		m.refresh()
		return m
	}
	m.busy = true
	m.scrollPinned = true // user just sent a message — show it
	m.refresh()
	return m
}

// sendAnswer resolves the ask: ships the answer to the engine and resets to
// the input pane.
func (m tuiModel) sendAnswer(v string) tuiModel {
	if !m.proc.answer(v) {
		m.append(stErr.Render("no engine right now — answer not sent"))
	}
	m = m.clearAsk()
	m.refresh()
	return m
}

// clearAsk returns to the input pane with no question open. The answer is
// rendered from the engine's `answered` event.
func (m tuiModel) clearAsk() tuiModel {
	m.focus = focusInput
	m.dropAsk()
	m.askOptions, m.askSel, m.askNote = nil, 0, ""
	m.askMulti, m.askChecked = false, nil
	return m
}

// updateSearch drives the "/" query box: keys type into it (matches recompute
// live and the selection jumps to the first hit), enter commits to match-scroll
// mode (↑/↓ step between hits), esc cancels.
func (m tuiModel) updateSearch(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
		return m.typeSearch(msg)
	}
}

// typeSearch is a key or a paste in the "/" query box.
func (m tuiModel) typeSearch(msg tea.Msg) (tea.Model, tea.Cmd) {
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
// them back. Only ones with a command (/raglit, /poly-lsp) — the rest are the config's
// business, and the new engine reads the same config.
func (m *tuiModel) noteServers(ev evMsg) {
	list, ok := ev["servers"].([]any)
	if !ok {
		return
	}
	// Every server's id, for /mcp's completion.
	m.serverIDs = m.serverIDs[:0]
	m.servers, _ = serverStates(list)
	m.noteServerOrder()
	m.noteStarting(list)
	for _, s := range list {
		if st, ok := s.(map[string]any); ok && str(st["id"]) != "" {
			m.serverIDs = append(m.serverIDs, str(st["id"]))
		}
	}
	m.noteServerView(list)
	m.noteExtServers()
	if len(m.convo) > 0 && m.convo[0].liveSplash {
		m.refresh() // the splash's servers and extras rows say it now
	}
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
		if !m.splashDone && evNum(ev["resumed"]) == 0 {
			m.splashWant = true
		}
		m.proc.uiGet() // this user's UI state in the session, kept by dund
	case "ready":
		m.starting = false
		m.tools = nil
		if ts, ok := ev["tools"].([]any); ok {
			for _, t := range ts {
				m.tools = append(m.tools, fmt.Sprint(t))
			}
		}
		// No "ready" line: the status bar says ready and the header counts
		// the tools. A session with nothing to send to says so when its
		// settings arrive (the settings case).
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
		// What did not start, and a docs index's ingest summary, once per
		// TUI: a restart (a model pick) does not repeat it. Which optional
		// extras are off is in the splash and /status, not a line here.
		if hint := strings.TrimSpace(str(ev["hint"])); hint != "" && !m.hintShown {
			m.append(stDim.Render(hint))
			m.hintShown = true
		}
		m.afterSwitch()
		// No servers are turned back on here: a respawned engine reattaches
		// to this session, whose switches it keeps itself (dun mcpscope.go).
		m.noteServers(ev)
		return m
	case "server":
		list, _ := ev["servers"].([]any)
		opened := str(ev["id"]) == allServers && m.openServerTable(list)
		if msg := strings.TrimSpace(str(ev["message"])); msg != "" && !opened && !m.splashSays(ev) {
			m.noteServerMsg(str(ev["action"]), msg)
			if m.serversPaging() != nil {
				m.flashText, m.flashUntil = oneLine(msg), time.Now().Add(flashFor)
			}
		}
		// The tool list changed; keep /help's count and the status bar honest.
		if ts, ok := ev["tools"].([]any); ok {
			m.tools = nil
			for _, t := range ts {
				m.tools = append(m.tools, fmt.Sprint(t))
			}
		}
		m.noteServers(ev)
	case "catalog":
		m.noteCatalog(ev)
		m.refresh() // the splash's skills row
	case "mcp_tools":
		m = m.noteMCPTools(ev)
	case "switches":
		m = m.noteSwitches(ev)
	case "extensions":
		m = m.noteExtensions(ev)
	case "mcp_login":
		// Whole, not oneLine'd or clipped: the URL is to be copied.
		m.append(stNote.Render("🔑 " + loginLine(str(ev["server"]), str(ev["url"]))))
	case "prompt":
		// An MCP prompt the user ran as a slash command, rendered by the
		// engine: it goes in as their message.
		if text := strings.TrimSpace(str(ev["text"])); text != "" {
			m = m.sendUser(text)
		}
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
		// The queued messages taken back: out of the conversation's
		// "(queued)" lines and into the composer, ahead of what is there.
		if str(ev["id"]) == "unqueue" {
			var texts []string
			_ = json.Unmarshal([]byte(str(ev["action"])), &texts)
			if len(texts) == 0 {
				m.flashText, m.flashUntil = "nothing to take back: the turn already took them", time.Now().Add(flashFor)
				break
			}
			for _, t := range texts {
				for i := len(m.convo) - 1; i >= 0; i-- {
					if m.convo[i].provisional && m.convo[i].provisionalText == t {
						m.convo = append(m.convo[:i], m.convo[i+1:]...)
						break
					}
				}
			}
			merged := strings.Join(texts, "\n")
			if cur := m.input.Value(); strings.TrimSpace(cur) != "" {
				merged += "\n" + cur
			}
			m.input.SetValue(merged)
			m.input.CursorEnd()
			m.focus = focusInput
			m.refresh()
			break
		}
		// /repaint report's answer: the TUI finishes it (repaintreport.go).
		if str(ev["id"]) == "repaint" {
			m.reportEvent(str(ev["action"]), str(ev["message"]))
			break
		}
		if msg := strings.TrimSpace(str(ev["message"])); msg != "" {
			m.append(replyLine(msg))
		}
		if str(ev["id"]) == "interrupt" { // there was no turn to interrupt
			m.interrupting = false
		}
		// /close is terminal: the session it was editing no longer exists, so
		// staying attached to it would be showing a conversation that cannot be
		// resumed. Quit only once the engine has confirmed the work is done.
		if str(ev["id"]) == "close" {
			m.quitting = true
			m.proc.forget() // and the service lets the session go
		}
	case "reasoning":
		m.busy = true
		if m.busyStart.IsZero() {
			m.busyStart = time.Now()
		}
		m.suggestions = nil
		m.addThinking(str(ev["text"]))
	case "token":
		m.busy = true // a turn is active (incl. autonomous background-completion turns)
		if m.busyStart.IsZero() {
			m.busyStart = time.Now()
		}
		m.suggestions = nil
		m.endRun() // the reply began: the run takes nothing more
		if m.liveThinking() >= 0 {
			// The reply has begun, so the round's thinking is over. Folding
			// changes the block's height, so this frame is drawn now, not on
			// the tick.
			m.endThinking()
			m.refresh()
		}
		m.cur += str(ev["text"])
		// Do NOT render here. A provider streaming 60 tokens/s would drive 60
		// full frames a second through the same Update loop that reads the
		// keyboard, and a frame is not free even with the wrap cache. Mark the
		// screen dirty; a tick renders at most renderHz. See renderTickMsg.
		m.renderDue = true
	case "reused":
		// A plain dun resumed an idle session (attach.go): say which.
		m.append(stDim.Render(str(ev["text"])))
	case "suggestions":
		// Not before anything was asked: there is nothing to predict from,
		// and a session that was just cleared must not show the old ones.
		if !m.asked() {
			break
		}
		m.suggestions = parseSuggestionItems(ev["items"])
		m.suggestSel = 0
		m.refresh()
	case "tool_call":
		m.busy = true
		if m.busyStart.IsZero() {
			m.busyStart = time.Now()
		}
		m.suggestions = nil
		m.endThinking()
		m.flushCur()
		args, _ := ev["args"].(map[string]any)
		// Into the run's box (toolrun.go): appended, nothing above it moves.
		m.pendingTool = m.runAddCall(str(ev["tool"]), args)
		m.activeTool = callText(str(ev["tool"]), args, 40)
		m.refresh()
	case "tool_result":
		result := str(ev["result"])
		idx := m.pendingTool
		if idx < 0 || idx >= len(m.convo) || m.convo[idx].run == nil {
			idx = m.runAddCall(str(ev["tool"]), nil) // a result with no call seen
		}
		m.runSetResult(idx, str(ev["tool"]), result)
		m.pendingTool = -1
		m.activeTool = ""
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
		// Delivered messages are worth bringing into view; a tool result on its
		// own is not, and pinning on every one dragged a reader who had
		// scrolled up back to the bottom once per call.
		if m.queuedMsgs > 0 {
			m.scrollPinned = true
		}
		m.queuedMsgs = 0
		m.refresh()
	case "history":
		if m.skipHistory {
			// A respawned engine replays the conversation it just resumed. It is
			// already on screen — rendering it again would double the scrollback.
			m.skipHistory = false
			break
		}
		items, _ := ev["items"].([]any)
		if len(items) > 0 && !m.splashKeep {
			m.splashWant = false // a conversation to show, not a fresh one
		}
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
		switch str(ev["kind"]) {
		case "docs":
			m.convo = append(m.convo, convoEntry{docs: docsFromEvent(ev)})
			m.refresh()
		case "budget":
			// The turn budget ran out and the grace started: the machinery
			// speaking, so dim, not a bell.
			m.append(stDim.Render("⏱ " + oneLine(str(ev["text"]))))
		case "peer":
			// Another dun session's dun_msg: not the user, so dim, with the
			// sender first (the text is "<name> (<id>): <message>").
			m.append(stDim.Render("✉ dun msg from " + oneLine(str(ev["text"]))))
		default:
			m.append(stNote.Render("🔔 " + oneLine(str(ev["text"]))))
		}
	case "ask":
		m.endThinking()
		m.flushCur()
		m.paging = false // the question needs the screen
		m.jobViewing = false
		if !m.asking {
			m.keepDraft() // the composer is the user's draft; the question borrows it
		}
		m.asking, m.noting, m.customAnswer = true, false, false
		m.askSel, m.askNote = 0, ""
		m.input.Blur()
		m.settleAsk() // a new question replaces any parked one
		q := convoEntry{askText: str(ev["question"]), askOpen: true}
		q.renderAsk(false)
		m.convo = append(m.convo, q)
		m.refresh()
		m.askOptions = nil
		if opts, ok := ev["options"].([]any); ok {
			for _, o := range opts {
				m.askOptions = append(m.askOptions, fmt.Sprint(o))
			}
		}
		m.askMulti, _ = ev["multi"].(bool)
		m.askChecked = make([]bool, len(m.askOptions))
		// recommended_all lists them all (several only for multi); an older
		// engine sends just recommended.
		m.askRec = nil
		if all, ok := ev["recommended_all"].([]any); ok {
			for _, r := range all {
				m.askRec = append(m.askRec, fmt.Sprint(r))
			}
		} else if r := str(ev["recommended"]); r != "" {
			m.askRec = []string{r}
		}
		for i, o := range m.askOptions {
			switch {
			case !slices.Contains(m.askRec, o):
			case m.askMulti:
				m.askChecked[i] = true // checked to start with; done takes them
			default:
				m.askSel = i // enter takes the recommendation
			}
		}
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
	case "upgrade":
		// dund is upgrading (to every connection, no session): a status row
		// of its own until the reconnect clears it.
		m.upgrade = parseUpgrade(ev)
		if m.upgrade.state == "failed" {
			// Nothing was replaced and dund runs on: said once, no row.
			m.append(stNote.Render(upgradeFailed(m.upgrade)))
			m.upgrade = nil
		}
		m.refresh()
	case "persist":
		// dund's answer to /persist.
		on, changed := ev["on"] == true, ev["changed"] == true
		switch {
		case on && changed:
			m.append(stDim.Render("persist: on — this session keeps running after /exit · /persist off to undo"))
		case on:
			m.append(stDim.Render("persist: on — /exit leaves this session running · /persist off to let /exit stop it"))
		case changed:
			m.append(stDim.Render("persist: off — /exit stops this session's engine (the conversation is kept)"))
		default:
			m.append(stDim.Render("persist: off — /exit stops this session's engine · /persist to keep it running"))
		}
	case "user":
		// The engine took a user message — from this client or another. It is
		// rendered here, not when sent, so every client shows the same
		// conversation. One buffered into the RUNNING turn is marked
		// provisional so it shows where it belongs — not stuck above the
		// divider; the marks clear when the turn ends.
		text := str(ev["content"])
		if ev["queued"] == true {
			m.convo = append(m.convo, convoEntry{
				collapsed:       stDim.Render("› " + text + " (queued)"),
				provisional:     true,
				provisionalText: text,
				userText:        text,
			})
		} else {
			m.task = text
			m.appendUser(text)
		}
		m.scrollPinned = true
		m.refresh()
	case "image_refused":
		// A message with images the engine did not send (images.go): why,
		// and the message back in the composer, ahead of anything typed since.
		m.append(stErr.Render("not sent: " + str(ev["message"])))
		var imgs []proto.Image
		if list, ok := ev["images"].([]any); ok {
			for _, it := range list {
				if o, ok := it.(map[string]any); ok {
					imgs = append(imgs, proto.Image{Path: str(o["path"]), Label: str(o["label"])})
				}
			}
		}
		m.input.Remember(imgs)
		draft := str(ev["content"])
		if cur := m.input.Value(); strings.TrimSpace(cur) != "" {
			draft += "\n" + cur
		}
		m.input.SetValue(draft)
		m.input.CursorEnd()
		m.refresh()
	case "turn":
		m.busy = true
		if m.busyStart.IsZero() {
			m.busyStart = time.Now()
		}
		m.suggestions = nil
	case "answered":
		// The open question is settled, whoever answered it.
		// The client that answered has already closed it; on another, it is
		// still open and closes now.
		m.settleAsk()
		m.appendUser(str(ev["value"]))
		if m.asking {
			m = m.clearAsk()
		}
		m.refresh()
	case "commands":
		m.engineCmds = engineCmdsFromEvent(ev)
	case "settings":
		m.model, m.url = str(ev["model"]), str(ev["url"])
		if c, ok := ev["context"].(map[string]any); ok {
			m.ctxSettings = c
		}
		m.effort = str(ev["effort"])
		if p := str(ev["provider"]); p != "" {
			m.provider = p
		}
		// The engine's word on whether it has anything to send to: the first
		// screen's one line, when it has not.
		m.noLogin = m.url == ""
		if m.splashWant {
			m.showSplash()
			m.splashWant, m.splashKeep, m.splashDone = false, false, true
		}
		if m.noLogin && !slices.ContainsFunc(m.convo, func(e convoEntry) bool { return e.needLogin }) {
			m.sayNeedLogin()
		}
		if !m.starting {
			m = m.sendKept()
		}
		if mode := str(ev["suggest"]); mode != "" {
			m.suggestMode = mode
		}
	case "queued":
		// The message itself arrived as a `user` event (queued); this is the
		// count of what the running turn has buffered.
		m.queuedMsgs = int(evNum(ev["count"]))
		// Track OOB messages for /context.
		m.ctxStats.oobMessages++
		m.refresh()
	case "context_cost":
		// The pre-conversation cost, delivered when it is measured rather than
		// waiting for a turn to finish. Arrives twice: estimates, then exact.
		m.ctxStats.readSystemCost(ev)
		m.ctxStats.readWindow(ev)
		m.refresh()
	case "spend":
		// A key-budget read that landed between turns (dun/spend.go).
		m.ctxStats.readSpend(ev)
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
		m.ctxStats.readSystemCost(ev)
		m.ctxStats.readWindow(ev)
		m.ctxStats.readSideCalls(ev)
		m.ctxStats.readSurfacing(ev)
		m.ctxStats.readCache(ev)
		m.ctxStats.readSpend(ev)
		m.noteTurnStats(ev)
		if v := evNum(ev["forced_calls"]); v > 0 {
			m.ctxStats.forcedToolCalls = int(v)
		}
		if v := evNum(ev["notifications_lifted"]); v > 0 {
			m.ctxStats.notificationsSmuggled = int(v)
		}
	case "done":
		cut := str(ev["reason"]) == "interrupted"
		if cut {
			m.markInterrupted()
		}
		m.endThinking()
		m.endRun()
		m.flushCur()
		// A question the turn asked ends with it, however the turn ended.
		if m.asking {
			m = m.clearAsk()
		}
		m.settleAsk()
		if cut {
			m.append(stDim.Render(interruptedRow))
			// The jobs the cut turn started keep running: say which, and how
			// to stop them.
			if d := strings.TrimSpace(str(ev["detail"])); d != "" {
				m.append(stNote.Render("  " + d))
			}
		}
		m.interrupting = false
		// Clear any remaining provisional markers — the turn is done, so
		// everything was delivered to the LLM. After an interrupt they were
		// not: they are the next turn's message, which the engine starts at
		// once, and its done settles them.
		for i := range m.convo {
			if !cut && m.convo[i].provisional {
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
		// The agent is idle: separate this turn's output from whatever the
		// human sends next with a blank row.
		m.turnGap()
		m.liveTurnDone = true
		// The turn is done, so the human's idle starts NOW. This is the only
		// place a suggestion request can begin — `done` is what "the LLM is
		// marked done" means on the wire, so nothing can land between a tool
		// call and its result.
		m.armIdleSuggest()
		m.refresh()
	case "error":
		m.endThinking()
		m.endRun()
		// A setup step not taken is the next step, not a failure: said as
		// the one action, with no retry advice — retrying cannot help.
		setup := false
		switch str(ev["error"]) {
		case errNoLogin.Error():
			m.sayNeedLogin()
			setup = true
		case errNoModel.Error():
			m.append(warnLine(errNoModel.Error()))
			setup = true
		default:
			m.append(stErr.Render("error: " + str(ev["error"])))
		}
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
		} else if !setup {
			// The conversation is on disk, so the next message pairs off whatever
			// was interrupted and picks up where this stopped.
			m.append(stDim.Render("the session is intact — send a message to retry from here"))
		}
		m.turnGap()
		m.refresh()
	case "reset":
		// The conversation was cleared — by this client's /clear or another's.
		m.convo = nil
		m.cur = ""
		m.pendingTool = -1
		m.activeTool = ""
		m.sel = -1
		m.busy = false
		m.dropAsk()
		m.dropHold()                         // nothing left on screen to hold in place
		m.suggestions, m.suggestSel = nil, 0 // they were about the conversation just cleared
		m.settleAsk()
		m.append(stDim.Render("session cleared — fresh start"))
		m.refresh()
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
	case "engine":
		// dund supervises the engine now; this is what it reports.
		switch str(ev["state"]) {
		case "started":
			m.fatalErr, m.exitAnnounced = "", false
			// A new engine: input waits for its ready.
			if !m.starting {
				m.starting, m.startingStart = true, time.Now()
			}
		case "restarting":
			m.busy = false
			m.dropAsk()
			m.settleAsk()
			m.busyStart = time.Time{}
			m.restarts = int(evNum(ev["restarts"]))
			m.skipHistory = true // it will replay what is already on screen
			m.append(stErr.Render(str(ev["reason"]) + " — restarting it; the session is kept"))
		case "gave_up":
			m.busy, m.starting = false, false
			m.dropAsk()
			m.settleAsk()
			m.fatalErr = str(ev["reason"]) + " — gave up restarting it"
			m.append(stErr.Render(m.fatalErr + "\n/reconnect to try again · your conversation is saved"))
		case "stopped":
			m.busy = false
			m.dropAsk()
			m.settleAsk()
		}
		m.refresh()
	case "ui_state":
		m = m.applyUI(ev["state"])
	case "snapshot":
		// Too far behind to replay: the conversation, then the latest state.
		// On a reconnect the conversation is already on screen; the history a
		// respawned engine sends next is skipped by the same flag.
		if !m.skipHistory {
			items, _ := ev["items"].([]any)
			m.replay(items)
		}
		state, _ := ev["state"].([]any)
		for _, st := range state {
			if sev, ok := st.(map[string]any); ok && sev["type"] != "history" {
				m = m.handleEvent(evMsg(sev))
			}
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
		if len(m.convo) > 0 && m.convo[len(m.convo)-1].run == nil && m.convo[len(m.convo)-1].collapsed != "" && strings.HasPrefix(m.convo[len(m.convo)-1].collapsed, "⏳ ") {
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
		if len(m.convo) > 0 && m.convo[len(m.convo)-1].run == nil && m.convo[len(m.convo)-1].collapsed != "" && strings.HasPrefix(m.convo[len(m.convo)-1].collapsed, "⏳ ") {
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
			if replyShown(m.cur) {
				// Its text goes (it is being regenerated), its rows do not: a
				// marker holds the height it had.
				m.convo = append(m.convo, convoEntry{collapsed: stDim.Render("↺ the reply was cut off — regenerating"),
					maxH: m.curMaxH, maxKey: m.curMaxKey})
			}
			m.cur, m.curMaxH, m.curMaxKey = "", 0, 0
		}
		// Whatever was thought before the failure stays, folded; the retried
		// round thinks in a block of its own.
		m.endThinking()
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
				e.collapsed = stDim.Render("▸ ") + stNote.Render("⏳ "+str(ev["reason"]))
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
		lines = append(lines, stNote.Render("reason:  ")+reason)
	}
	if detail := str(ev["detail"]); detail != "" {
		lines = append(lines, stDim.Render("detail:  ")+detail)
	}
	if cap := evNum(ev["capacity"]); cap > 0 {
		lines = append(lines, stDim.Render(fmt.Sprintf("capacity:  %d/%d busy  (%d ahead)",
			int(evNum(ev["in_flight"])), int(cap), int(evNum(ev["waiting"])))))
	}
	if queue := str(ev["queue"]); queue != "" {
		lines = append(lines, stDim.Render("queue:   ")+queue)
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
	// Read the geometry from the frame — the single snapshot refresh last wrote
	// — not m.blockH / e.rowOffset directly. Same numbers (refresh writes the
	// entries from the frame), one value every reader agrees on.
	bh, ro := m.frame.blockH, m.frame.rowOffset
	var closestText string
	closestBottom := -1
	for i, e := range m.convo {
		if e.userText == "" {
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
	if overlay := m.scrollOverlay(); overlay != "" {
		return overlay
	}
	// Minimal: the project's folder and the model in use, where they can be
	// checked at any time. The build, the full path, the tools and the
	// servers are in the splash and /status (splash.go).
	head := stBrand.Render("dun")
	if m.workspace != "" {
		head += stDim.Render(" · " + filepath.Base(m.workspace))
	}
	if m.branch != "" {
		head += stDim.Render(" ⎇ " + m.branch)
	}
	if m.model != "" && m.url != "" && !m.splashModelShown() {
		head += stDim.Render(" · " + m.model)
	}
	if n := m.liveAgents(); n > 0 {
		head += stNote.Render(fmt.Sprintf("  · %d agents", n))
	}
	if m.reloadVer != "" {
		head += "  " + stNote.Render("↻ "+m.reloadVer+" (/reload)")
	}
	return head
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

	var status string
	switch {
	case m.fatalErr != "":
		status = stErr.Render(m.fatalErr)
	case m.starting:
		status = m.spin.View() + stDim.Render(" "+m.startingWhat()+m.startingElapsed()+m.startQueuedHint())
	case m.confirm.q != "":
		status = stAsk.Render("❓ " + m.confirm.q)
	case m.flashing():
		status = stNote.Render(m.flashText)
	case m.activeScreen() != nil:
		status = m.scr.hint(&m)
	case m.jobViewing:
		status = stDim.Render(m.jv.hint())
	case m.paging:
		if v := m.serversPaging(); v != nil {
			status = v.hint() // styled itself: its hotkeys are marked
		} else if v := m.switchesPaging(); v != nil {
			status = stDim.Render(v.hint())
		} else {
			status = stDim.Render(m.pg.hint())
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
		status = m.search.View() + stDim.Render(fmt.Sprintf("  (%d matches · enter to navigate)", len(m.matches)))
	case m.searchActive:
		status = stDim.Render(fmt.Sprintf("match %d/%d  ·  ↑/↓ prev/next · / new search · esc exit", m.matchPos+1, len(m.matches)))
	// A form or picker shows its own keys here, never the composer's: two
	// meanings of enter on one screen was the fault.
	case m.modelPicking && m.loginPicking:
		status = stDim.Render(loginHint())
	case m.modelPicking && m.providerForm:
		status = stDim.Render(m.providerHint())
	case m.modelPicking:
		status = stDim.Render(modelHint())
	case m.mcpForm:
		status = stDim.Render(m.mcpHint())
	case m.rag != nil:
		status = stDim.Render(m.ragSetupHint())
	case m.paletteActive():
		what := "command"
		if _, _, ok := m.paletteArgs(); ok {
			what = "argument"
		}
		status = stDim.Render(what + "  ·  ↑/↓ select · enter run · tab complete · esc close")
	case m.askParked && m.focus != focusConvo:
		status = stAsk.Render("❓ a question is waiting") + stDim.Render(" · tab, select it, enter to answer · "+m.exitHint())
	case m.retry != "":
		// The countdown first: a row too wide for the window loses its end.
		cd := strings.TrimPrefix(m.retryCountdown(), " · ")
		if cd != "" {
			cd += " · "
		}
		status = stNote.Render("⏳ "+cd+m.retry) + stDim.Render("  ("+m.busyHint()+")")
	case m.busy && m.interrupting:
		status = m.spin.View() + " " + stDim.Render("interrupting…"+m.busyElapsed()+m.queuedHint())
	case m.busy:
		what := ""
		if m.activeTool != "" {
			what = " " + m.activeTool
		}
		status = m.spin.View() + " " + m.spendStatus(time.Now()) + stDim.Render("working…"+what+m.busyElapsed()+m.queuedHint()+" · "+m.busyHint())
	case m.focus == focusActivity && m.actLevel == actCollapsed:
		status = stDim.Render("activity  ·  → open · tab input · " + m.exitHint())
	case m.focus == focusActivity:
		status = stDim.Render("activity  ·  ↑/↓ select · →/enter open (agent = its view) · x kill job · tab next zone · esc input")
	case m.scopeAgent != 0:
		status = stDim.Render(fmt.Sprintf("agent #%d  ·  type to tell it · tab activity · ←/esc back to the session", m.scopeAgent))
	case m.focus == focusConvo:
		if d := m.selDocs(); d != nil && d.descended {
			status = stDim.Render("docs  ·  ↑/↓ doc · → open · ← close, then out · ctrl+c quit")
		} else {
			switch {
			case m.insideBox() != nil:
				status = stDim.Render("inside  ·  ↑/↓ pgup/pgdn g/G scroll · ← leave · enter open")
			case m.insideWide() != nil:
				w := m.insideWide()
				status = stDim.Render(fmt.Sprintf("sideways  ·  ←/→ scroll · home/end edge · esc leave · col %d of %d", w.off+1, w.max+m.vp.Width-wideIndicatorRoom))
			case m.selWide() != nil && m.selWide().max > 0:
				status = stDim.Render("convo  ·  ↑/↓ select · → scroll sideways · y copy · tab input")
			case m.selBox() != nil:
				status = stDim.Render("convo  ·  ↑/↓ select · → scroll inside · enter open · tab input")
			default:
				status = stDim.Render("convo  ·  ↑/↓ select · →/← open/close · y copy · tab input")
			}
		}
	case m.suggestActive():
		status = stDim.Render(fmt.Sprintf("next?  ·  ↑/↓ cycle · 1–%d pick · enter/right accept · or type · "+m.exitHint(), len(m.suggestions)))
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
			status = stDim.Render(enter + " · / commands · " + m.exitHint())
			break
		}
		if m.restoredMsg != "" && m.input.Value() == m.restoredMsg {
			// The composer holds the question asked before the login: say
			// it is that, not something just typed. The text sent is only
			// the question.
			status = m.spendStatus(time.Now()) + stDim.Render("your earlier question  ·  enter asks it · or change it first")
			break
		}
		// Four hints at most: the rest are in /help. The way out is never
		// the one a narrow row drops (fitHints).
		status = m.spendStatus(time.Now()) + stDim.Render("ready"+m.serversStartingTail()+"  ·  enter send · alt+enter newline · ↑ history · "+m.exitHint())
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
	out := append([]string{m.headView()}, top...)
	out = append(out, convo, div, lower)
	if s := m.activeScreen(); s != nil { // it takes the rows the base screen had
		out = []string{m.headView(), s.view(&m, m.w, m.screenRows())}
	}
	if u := m.upgradeView(); u != "" {
		out = append(out, u) // persistent while dund upgrades
	}
	out = append(out, fitHints(status, m.w))
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
			label += stDim.Render("  (recommended)")
		}
		rows = append(rows, gut(label, sel(i)))
	}
	// The affordance belongs on the row itself: highlighted and not yet
	// capturing is exactly the moment someone wonders whether to press enter.
	customLabel := "✎ custom answer / chat…"
	if sel(custom) && !m.customAnswer {
		customLabel += "   (just type)"
	}
	rows = append(rows, gut(stDim.Render(customLabel), sel(custom) || m.customAnswer))
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

// selSkippable reports the entry at i as one the message selection must never
// land on — a blank turn-gap row has nothing to select.
func (m *tuiModel) selSkippable(i int) bool {
	return i >= 0 && i < len(m.convo) && m.convo[i].blank
}

// selSkipDown / selSkipUp step from i to the nearest non-skippable entry in
// the given direction, clamping at the edge. The selection never rests on a
// blank turn-gap row.
func (m *tuiModel) selSkipDown(i int) int {
	for i < len(m.convo) && m.selSkippable(i) {
		i++
	}
	if i >= len(m.convo) {
		i = len(m.convo) - 1
	}
	if i < 0 {
		i = 0
	}
	return i
}

func (m *tuiModel) selSkipUp(i int) int {
	for i >= 0 && m.selSkippable(i) {
		i--
	}
	if i < 0 {
		i = len(m.convo) - 1
	}
	if i >= len(m.convo) {
		i = len(m.convo) - 1
	}
	return i
}

// turnGap appends the blank row that separates one turn's output from the
// next, so the conversation reads in blocks. Called when the agent goes idle.
func (m *tuiModel) turnGap() {
	// An ask answered without a done in between would otherwise stack two
	// blank rows; the second is only the first one over.
	if n := len(m.convo); n > 0 && m.convo[n-1].blank {
		return
	}
	m.convo = append(m.convo, convoEntry{collapsed: "", blank: true})
}

// showSavedConversation puts the session being resumed on screen from its
// transcript on disk, read-only, while the session service cannot be reached.
// When dund answers, its replay of the same conversation is skipped.
func (m *tuiModel) showSavedConversation() {
	m.offlineShown = true
	id := m.opts.resume
	if id == "" && m.opts.cont {
		id = dun.LatestSession(m.opts.workspace)
	}
	if id == "" {
		return
	}
	hist := dun.SessionHistory(dun.SessionFile(m.opts.workspace, id))
	if len(hist) == 0 {
		return
	}
	b, err := json.Marshal(hist)
	if err != nil {
		return
	}
	var items []any
	if json.Unmarshal(b, &items) != nil {
		return
	}
	m.replay(items)
	m.append(stDim.Render("── " + id + ", read-only from disk: the session service (dund) cannot be reached — retrying ──"))
	m.skipHistory = true
	m.refresh()
}

// exit is /exit: tell dund this client is leaving — it stops the engine
// unless the session persists or another client is attached — then quit.
// ctrl+c only detaches.
func (m *tuiModel) exit() tea.Cmd {
	m.proc.leave()
	m.quitting = true
	return tea.Quit
}

// appendUser adds a user message and stores the raw text for full-width re-rendering.
func (m *tuiModel) appendUser(text string) {
	m.convo = append(m.convo, convoEntry{collapsed: userRow(text), userText: text})
	m.refresh()
}

func (m *tuiModel) flushCur() {
	// A reply of only whitespace is no reply. Reasoning models open with
	// "\n\n" after their thinking, and a round that then calls a tool leaves
	// nothing else — finalizing it would leave a blank row in the log.
	if replyShown(m.cur) {
		src := strings.TrimRight(m.cur, "\n")
		// Finalize the streamed reply as rendered markdown (headers, lists, code).
		// The final render can come out shorter than a partial one did (an
		// unclosed fence, a list mid-item): it keeps the streamed height.
		m.convo = append(m.convo, convoEntry{collapsed: renderMarkdown(m.md, src), mdSource: src,
			maxH: m.curMaxH, maxKey: m.curMaxKey})
	}
	m.cur, m.curMaxH, m.curMaxKey = "", 0, 0
}

// heightKey is what a block's held height is valid for: one wrap width, one
// view state.
func heightKey(w int, st viewState) int { return w*4 + int(st) + 1 }

// holdHeight pads rendered block w to the most rows it has had under key,
// and records a new most.
func holdHeight(w string, maxH, maxKey *int, key int) string {
	if *maxKey != key {
		*maxH, *maxKey = 0, key
	}
	n := strings.Count(w, "\n") + 1
	if n < *maxH {
		return w + strings.Repeat("\n", *maxH-n)
	}
	*maxH = n
	return w
}

// replyShown says whether the streaming reply has anything to draw. Until it
// does it takes no row: a block that renders to nothing is still one row,
// and it sat between the message just sent and whatever came next.
func replyShown(cur string) bool { return strings.TrimSpace(cur) != "" }

// replay rebuilds scrollback from a `history` event (a resumed session), reusing
// the same rendering as live events: user echo, assistant markdown, folded tool
// call/result blocks, notifications. No turn runs — this is pure display. A
// trailing dim marker delimits the resumed history from new activity.
func (m *tuiModel) replay(items []any) {
	n := 0
	run := -1 // the run the next tool_result belongs to
	for _, it := range items {
		im, _ := it.(map[string]any)
		switch str(im["kind"]) {
		case "user":
			m.endRun()
			m.task = str(im["content"]) // the task line survives a resume
			m.convo = append(m.convo, convoEntry{collapsed: userRow(str(im["content"])), userText: str(im["content"])})
			n++
		case "assistant":
			m.endRun()
			src := strings.TrimRight(str(im["content"]), "\n")
			m.convo = append(m.convo, convoEntry{collapsed: renderMarkdown(m.md, src), mdSource: src})
			n++
		case "tool_call":
			// The same path as a live call: into the run's box (toolrun.go).
			args, _ := im["args"].(map[string]any)
			run = m.runAddCall(str(im["tool"]), args)
		case "tool_result":
			if run < 0 || run >= len(m.convo) || m.convo[run].run == nil {
				run = m.runAddCall(str(im["tool"]), nil) // a result with no call seen
			}
			m.runSetResult(run, str(im["tool"]), str(im["content"]))
			n++
		case "notification":
			m.endRun()
			m.convo = append(m.convo, convoEntry{collapsed: stNote.Render("🔔 " + oneLine(str(im["content"])))})
			n++
		case "reasoning":
			m.addThought(&thinkBlock{text: str(im["content"])})
		}
	}
	m.endRun()
	// A model pick restarts the engine on the same conversation: nothing was
	// resumed as far as the person is concerned, so there is no marker.
	if n > 0 && !m.quietReplay {
		m.convo = append(m.convo, convoEntry{collapsed: stDim.Render(commaf("── resumed %d entries ──", n))})
	}
	m.quietReplay = false
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
	// A box: → descends into it, to scroll it (box.go). Inside, → stays.
	if e.boxed() {
		e.box.inside = true
		return true
	}
	// An answer with a block cut at the edge: → descends into it, to
	// scroll it sideways (wide.go).
	if e.wide != nil && e.wide.max > 0 {
		e.wide.inside = true
		return true
	}
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
	bh := m.frame.blockH
	for i := 0; i < m.sel && i < len(bh); i++ {
		top += bh[i]
	}
	if m.sel >= 0 && m.sel < len(bh) {
		h = bh[m.sel]
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
	d := &docsBlock{found: intOf(ev["found"]), surfaced: intOf(ev["surfaced"]), tokens: intOf(ev["tokens"])}
	if arr, ok := ev["docs"].([]any); ok {
		for _, it := range arr {
			dm, _ := it.(map[string]any)
			d.docs = append(d.docs, docNode{title: str(dm["title"]), line: str(dm["line"]), score: floatOf(dm["score"]),
				source: str(dm["source"]), why: str(dm["why"]), kind: str(dm["kind"])})
		}
	}
	return d
}

func (m *tuiModel) refresh() {
	start := time.Now()
	defer func() { frames.observe(stageRefresh, time.Since(start)) }()
	m.liveSplashNow()
	blocks := make([]string, 0, len(m.convo)+1)
	for _, e := range m.convo {
		blocks = append(blocks, e.view())
	}
	curAt := -1
	if replyShown(m.cur) {
		curAt = len(blocks)
		blocks = append(blocks, "") // rendered below, once the width is known
	}
	// Suggestions live inside the viewport so they don't steal height from the
	// conversation — the user can still see the last messages above them.
	if m.suggestActive() {
		// Predictions, not conversation: dimmed, the highlighted one a step
		// brighter (suggestStyle), so they never read as something said.
		for i, s := range m.suggestions {
			body := stSel.Render(fmt.Sprintf("%d", i+1)) + " " + suggestStyle(i == m.suggestSel).Render(s.text) + "  " +
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
	// The gutter's column is reserved in every focus, so moving focus never
	// re-wraps: reserved only in convo focus, a row that filled the pane
	// broke in two when the conversation took the keys (USER 2026-10-24).
	width := m.vp.Width - 1
	wrapW := max(1, width)
	if curAt >= 0 {
		// The streaming reply renders the way it will once finished: its
		// wide blocks cut at the pane, not wrapped (wide.go).
		blocks[curAt], _ = renderAnswer(m.md, m.proseW(), m.cur, wrapW, 0)
	}
	rendered := make([]string, 0, len(m.vp.lines)) // display rows, not blocks
	frm := frame{
		// Sized by BLOCKS, not by convo: blocks is the convo plus the streaming
		// block (m.cur) plus any suggestion rows, and the loop below writes one
		// entry per block. Sizing by len(m.convo) panicked the moment anything
		// streamed. Convo-indexed readers are unaffected — the extra trailing
		// entries are simply never asked for.
		rowOffset: make([]int, len(blocks)),
		blockH:    make([]int, len(blocks)), // cache block heights for scroll math
		height:    m.convoHeightBudget(),
		width:     m.vp.Width,
	}

	// Render all blocks. For user messages, re-render with full-width
	// background style (ignoring the pre-styled collapsed from e.view()).
	cumRow := 0 // running row offset for scrollOverlay mapping
	m.bandRaw = nil
	for i, b := range blocks {
		var w string
		if i < len(m.convo) && m.convo[i].blank {
			// The turn-gap row: exactly one empty line, nothing to measure.
			w = ""
		} else if i < len(m.convo) && m.convo[i].boxed() {
			// A bounded box: its own cache, so a token re-wraps only the box
			// that grew (box.go).
			w = renderBox(&m.convo[i], wrapW, m.h)
		} else if i < len(m.convo) && m.convo[i].docs == nil {
			e := &m.convo[i]
			// Rendered markdown bakes in the wrap width it was produced at, and a
			// pre-wrapped line has no break opportunity left for cellbuf to find
			// on a narrower pane — so re-render from the source when the width
			// moves. The key is wrapMaxOK, not wrapW: the default path below
			// leaves wrapW at 0 whenever the block already fits (the common case
			// after a re-render), so wrapW cannot say "measured at this width".
			// wrapMaxOK is set once per source — invalidateWrap() clears it here
			// after a re-render, and nothing else does between refreshes — so it
			// is true exactly when the current render matches the current width.
			// A token therefore never pays for a glamour re-render.
			if e.mdSource != "" && (e.wrapW == 0 || e.wrapW != width) {
				e.collapsed = m.renderEntry(e, wrapW)
				b = e.view()
				e.invalidateWrap()
			}
			switch {
			case e.userText != "":
				if e.wrapped == "" || e.wrapW != width || e.wrapState != e.state {
					e.wrapped, e.wrapW, e.wrapState = userRowAt(e.userText, wrapW), width, e.state
					if e.state == viewExpanded && e.stats != "" {
						for _, l := range strings.Split(e.stats, "\n") {
							e.wrapped += "\n" + hangWrap(l, wrapW)
						}
					}
				}
				w = e.wrapped
			default:
				if !e.wrapMaxOK || e.wrapState != e.state {
					// New or changed source: measure it once. Cheaper than wrapping
					// it, and it answers the question for every future width.
					e.wrapMax, e.wrapMaxOK, e.wrapState = maxLineWidth(b), true, e.state
					e.wrapped, e.wrapW = "", 0
				}
				// Record the width this measurement is valid at, even when the
				// block fits and no wrap happens: a block that fits at 96 also
				// fits at any wider width, so the fast path below stays correct —
				// but wrapW must say WHERE it was measured, or a consumer cannot
				// tell "fits here" from "never looked".
				e.wrapW = width
				switch {
				case e.wrapMax <= wrapW:
					// Already fits — no wrapping at this width or any wider.
					w = b
				case e.wrapped != "" && e.wrapW == width:
					w = e.wrapped
				default:
					if e.hang {
						e.wrapped, e.wrapW = hangWrap(b, wrapW), width
					} else {
						e.wrapped, e.wrapW = cellbuf.Wrap(b, wrapW, ""), width
					}
					w = e.wrapped
				}
			}
		} else {
			w = cellbuf.Wrap(b, wrapW, "")
		}
		// Never shorter than it has been (convoEntry.maxH). Docs blocks change
		// only when the person opens or closes something in them.
		switch {
		case i < len(m.convo) && m.convo[i].docs == nil && !m.convo[i].blank:
			e := &m.convo[i]
			w = holdHeight(w, &e.maxH, &e.maxKey, heightKey(wrapW, e.state))
		case i == len(m.convo) && replyShown(m.cur):
			w = holdHeight(w, &m.curMaxH, &m.curMaxKey, heightKey(wrapW, viewMinimized))
		}
		if selMode && i == m.sel {
			w = addGutter(w, "▎", stSel)
		} else {
			w = addGutter(w, " ", lipgloss.NewStyle())
		}
		if selMode && i == m.sel && band.on() {
			// The band is drawn over the finished rows, and they are kept as
			// they were so a pulse step can redraw just these (repaintBand).
			m.bandRaw, m.bandAt = strings.Split(w, "\n"), len(rendered)
			rendered = append(rendered, band.rows(m.bandRaw, m.vp.Width, m.pulse)...)
		} else {
			rendered = append(rendered, strings.Split(w, "\n")...)
		}
		h := len(rendered) - cumRow
		frm.blockH[i] = h
		frm.rowOffset[i] = cumRow
		if i < len(m.convo) {
			// The entry keeps its own copy for the trace/replay path and for
			// scrollOverlay's row mapping; the frame is the authoritative one
			// that View and applyScroll read.
			m.convo[i].rowOffset = cumRow
		}
		cumRow += h
	}
	// A block that got shorter (closed, collapsed, a line taken away) does
	// not pull the view back: the top row stays and blank rows fill in
	// below, but never past the bottom of the view as it is now, so the
	// furthest scroll is where the view already is (holdShrink).
	if m.holdSkip {
		m.holdSkip = false
	} else {
		m.holdShrink(len(rendered))
	}
	// A closed question's rows stay in the pane until new rows take them
	// (holdConvo): blank rows at the bottom, so nothing above them moves.
	if m.convoHold > 0 {
		if m.vp.Width != m.convoHoldW || len(rendered) >= m.convoHold {
			m.convoHold = 0
		} else {
			for len(rendered) < m.convoHold {
				rendered = append(rendered, "")
			}
		}
	}
	// Hand the pane the rows themselves. Joining them into one string only for
	// it to split them back was ~12ms of every refresh on a long conversation —
	// and every streamed token is a refresh.
	m.vp.SetLines(rendered)
	m.contentGen++
	// The frame is now the single geometry truth. refresh is the ONLY writer:
	// it populated frm above and stored it, so every reader (View, convoHeight,
	// applyScroll, scrollOverlay) sees the same snapshot instead of re-deriving
	// a copy that could drift.
	m.frame = frm

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
	// The block geometry below (selGeom, the last-user-row scan) reads the
	// frame — the same snapshot refresh just wrote. The frame is the single
	// value every reader agrees on, so a resize or a content change can never
	// leave this decision looking at a different geometry than View did.
	if selMode && m.sel >= 0 && m.sel < len(m.frame.blockH) {
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
	// A panel below took rows from the pane: the view stays where it was
	// while the panel is open and nothing new has arrived (see the shrink in
	// Update). New rows, or the panel gone, end the hold.
	if m.panelHeld {
		switch {
		case !m.panelOpen():
			// Just closed — with a line added or not (the /mcp add
			// "adding …" line comes with the close): stay until the pane
			// has grown back (Update's funnel keeps this top row and then
			// ends the hold); after that, new rows move the view only as
			// far as they need.
			m.vp.SetYOffset(m.panelYOff)
			return
		case len(m.vp.lines) <= m.panelRows:
			m.vp.SetYOffset(m.panelYOff) // still open, nothing new: stay
			return
		}
		m.panelHeld = false
		// New rows with a panel still open: they are followed, and if the
		// panel grows in the same update (a form giving way to a taller
		// picker), the shrink keeps them in view (Update's funnel).
		m.panelFollow = true
	}
	// Auto-follow new content. When there's a streaming reply (m.cur), don't
	// GotoBottom() blindly — that scrolls past the user's last message to the
	// bottom of the growing reply. Instead keep that message visible.
	lastUserRow := -1
	if replyShown(m.cur) {
		ro := m.frame.rowOffset
		for i := range m.convo {
			if m.convo[i].userText != "" && i < len(ro) {
				lastUserRow = ro[i]
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

// scrollToEnd jumps to the newest output and re-pins, so the view follows the
// stream again — the way ctrl+end ends a scroll-back everywhere else.
func (m *tuiModel) scrollToEnd() {
	m.vp.GotoBottom()
	m.scrollPinned = true
	m.traceScroll()
}

// scrollToLastUser jumps to the top of the most recent user message: the start
// of the current exchange, which is what you want after reading back — "show me
// what I asked and everything since". Unpins, because it is a position the
// stream would otherwise scroll away from.
func (m *tuiModel) scrollToLastUser() {
	ro := m.frame.rowOffset
	row := -1
	for i := range m.convo {
		if m.convo[i].userText != "" && i < len(ro) {
			row = ro[i]
		}
	}
	if row < 0 {
		return
	}
	m.vp.SetYOffset(row)
	m.updateScrollPin()
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

// mouseSlash switches mouse reporting at runtime.
//
// It exists because Termux forces a choice dun cannot make for you. All-motion
// (1003, the default, and what claude runs) leaves Termux's mouse tracking
// INACTIVE, so a tap raises the soft keyboard — and a finger-scroll arrives as
// plain ↑/↓ keys, which scroll the conversation a row at a time. Cell motion
// (1002) makes Termux track the pointer, so the wheel scrolls the log directly
// but a tap never opens the keyboard. On a desktop terminal the difference is
// only whether motion events are reported at all.
func mouseSlash(m *tuiModel, args []string) tea.Cmd {
	if len(args) == 0 {
		m.append(stDim.Render("mouse: " + m.mouseMode + "  (all|cell|off)\n" +
			"  click selects a block or focuses the input · drag copies the text (OSC 52; tmux's buffer too)\n" +
			"  the terminal's own selection: shift+drag (iTerm2: option+drag)\n" +
			"  tmux taking drags for copy-mode? bind -n MouseDrag1Pane if -F '#{mouse_any_flag}' 'send -M' 'copy-mode -M'"))
		return nil
	}
	switch args[0] {
	case "all":
		m.mouseMode = "all"
		m.append(stDim.Render("mouse: all-motion — tap-to-type on Termux; ↑/↓ scroll the log"))
		return nil
	case "cell":
		m.mouseMode = "cell"
		m.append(stDim.Render("mouse: cell-motion — wheel scrolls the log; no tap-to-type on Termux"))
		return nil
	case "off":
		m.mouseMode = "off"
		m.append(stDim.Render("mouse: off — the terminal keeps the pointer (native selection)"))
		return nil
	}
	m.append(stErr.Render("mouse: want all|cell|off, got " + args[0]))
	return nil
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

// callText is the one-line form of a tool call — `server · tool  k=v, …` — for the
// collapsed block and the status line.
//
// exec is the exception: a shell command reads as a shell command, so it comes
// out as `$ go test ./...` and the `exec(command=` wrapper, which tells a reader
// nothing they did not already know, is dropped. Any OTHER arg (timeout) still
// shows, in parentheses after the command.
func callText(tool string, args map[string]any, max int) string {
	if line, ok := execCallText(args, 40); ok && tool == "exec" {
		return clip(oneLine(line), max)
	}
	if line, ok := evalCallText(args, max); ok && tool == "eval" {
		return line
	}
	if line, ok := fileCallText(tool, args, max); ok {
		return line
	}
	line := toolLabel(tool)
	if p := argPreview(args, max); p != "" {
		line += "  " + p
	}
	return clip(line, max)
}

// toolLabel is a tool's name as a person reads it: "notes__project_notes"
// is "notes · project notes", "node_query" is "node query". The one place a
// tool name is turned into words for the screen; the model, the logs and the
// inspector keep the real name.
func toolLabel(tool string) string {
	words := func(s string) string {
		return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '_' }), " ")
	}
	if srv, name, ok := strings.Cut(tool, "__"); ok && srv != "" && name != "" {
		return srv + " · " + words(name)
	}
	if w := words(tool); w != "" {
		return w
	}
	return tool
}

// execCallText renders exec's args as a shell prompt line. ok=false when there
// is no usable command, so the caller falls back to the generic form.
func execCallText(args map[string]any, restMax int) (string, bool) {
	cmd, _ := args["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return "", false
	}
	rest := make(map[string]any, len(args))
	for k, v := range args {
		if k != "command" {
			rest[k] = v
		}
	}
	line := "$ " + cmd
	if p := argPreview(rest, restMax); p != "" {
		line += "  (" + p + ")"
	}
	return line, true
}

// callFull is the expanded call line: the tool name over its args, or — for
// exec — the command verbatim, newlines and all, since a multi-line script is
// exactly what you expanded the block to read.
func callFull(tool string, args map[string]any) string {
	if line, ok := execCallText(args, 200); ok && tool == "exec" {
		return stTool.Render(line)
	}
	if tool == "eval" && evalCode(args) != "" {
		return evalCallFull(args)
	}
	full := stTool.Render(toolLabel(tool))
	if af := argFull(args); af != "" {
		full += "\n" + af
	}
	return full
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
		{"login", "[name]", "connect to an AI service: its address, and a key if it needs one", loginSlash},
		{"model", "[provider] [name] [--session] | use <provider> <id>", "pick the model: a searchable list from every service you are logged in to", modelSlash},
		// /mcp add is a subcommand, listed on its own so a person who was
		// given a server finds it in the first screen of the list (run 4:
		// it was only in /mcp's description).
		{"mcp add", "", "connect a server you were given — its command or address", func(m *tuiModel, args []string) tea.Cmd {
			return engineSlash("mcp")(m, append([]string{"add"}, args...))
		}},
		{"help", "[command|--debug]", "list these commands; /help <command> says one in full", func(m *tuiModel, args []string) tea.Cmd {
			switch {
			case len(args) > 0 && args[0] == "--debug":
				m.showHelp(true)
			case len(args) > 0:
				m.showCommandHelp(args[0])
			default:
				m.showHelp(false)
			}
			return nil
		}},
		{"config", "", "show this session's model settings (change with `dun --setup`)", func(m *tuiModel, _ []string) tea.Cmd { m.showConfig(); return nil }},
		{"status", "", "this session: build, workspace, model, tools and servers", func(m *tuiModel, _ []string) tea.Cmd { m.statusSlash(); return nil }},
		{"logout", "<provider>", "log out of an AI service: forget its address and key", logoutSlash},
		{"context", "[config [--max N] [--compact-buffer N] [--tool-lod N] [--recap-nudge N]]", "inspect context usage: tokens, compaction, recaps, truncation; config: the window, the reply's buffer, tool-result stubs and the recap reminder, saved for this model", func(m *tuiModel, args []string) tea.Cmd {
			if len(args) > 0 && strings.EqualFold(args[0], "config") {
				// Bare: the picker (the flags with their values, contextChoices).
				if len(args) == 1 {
					m.input.SetValue("/context config ")
					m.input.CursorEnd()
					m.paletteSel = 0
					return nil
				}
				if !m.proc.controlCmd("ctxconfig", strings.Join(args[1:], " ")) {
					m.append(stErr.Render("no engine right now — /reconnect first"))
				}
				return nil
			}
			m.showContext()
			return nil
		}},
		{"reload", "", "restart into the latest build (the launcher rebuilds on source change)", func(m *tuiModel, _ []string) tea.Cmd {
			m.reloadReq = true
			return tea.Quit
		}},
		{"upgrade", "[now|off|on]", "dund's pending upgrade: what it waits for; now restarts it at once (cut work resumes after); off|on: a release build's daily update check", func(m *tuiModel, args []string) tea.Cmd {
			if len(args) > 0 && (args[0] == "off" || args[0] == "on") {
				line, err := upgradeSwitch(args[0] == "on")
				if err != nil {
					m.append(stErr.Render("updates: not saved: " + err.Error()))
					return nil
				}
				m.append(stDim.Render(line))
				return nil
			}
			if m.upgrade == nil {
				// A source build: nothing tells dund there is a newer build
				// (the launcher reloads the TUI only), so dund and its
				// engines keep the one they started with. now moves them to
				// the build this TUI runs (USER 2026-10-27: /upgrade on a
				// source build was a dead end).
				if len(args) > 0 && args[0] == "now" {
					m.append(stDim.Render("asking dund to restart into this build (" + version + ") — running turns are interrupted and resume after"))
					return upgradeIntoSelfCmd(m.proc)
				}
				line := "no dund upgrade is pending"
				if !isRelease() {
					line += " · dund and its engines keep the build they started with; /upgrade now restarts them into this one (" + version + ")"
				}
				m.append(stDim.Render(line))
				return nil
			}
			if len(args) == 0 || args[0] != "now" {
				m.append(stNote.Render(upgradeDetail(m.upgrade, m.sessionID)))
				return nil
			}
			if !m.proc.upgradeNow() {
				m.append(stErr.Render("no session service right now — nothing restarted"))
				return nil
			}
			m.append(stDim.Render("asked dund to restart now — running turns are interrupted and resume after"))
			return nil
		}},
		{"perf", "", "redraw timings for this session (and how to get a profile)", func(m *tuiModel, _ []string) tea.Cmd {
			m.append(stHeader.Render("render performance") + "\n" + frames.report())
			return nil
		}},
		{"repaint", "[report]", "clear and redraw the whole screen, after saving what dun drew and what the terminal shows to ~/.dun/bugreports/; report also has the model redact the capture and explain what went wrong (asks first if the model is not local)", repaintSlash},
		{"mouse", "[all|cell|off]", "mouse reporting mode (bare shows it) — `cell` if the wheel should scroll the log; click selects, drag copies, shift+drag (iTerm2: option+drag) is the terminal's own selection", mouseSlash},
		{"scrolldebug", "", "debug: dump scroll overlay state (rowOffset, blockH, YOffset)", func(m *tuiModel, _ []string) tea.Cmd {
			var lines []string
			lines = append(lines, fmt.Sprintf("YOffset=%d pinned=%v vpH=%d convo=%d blockH=%d",
				m.vp.YOffset, m.scrollPinned, m.vp.Height, len(m.convo), len(m.frame.blockH)))
			for i, e := range m.convo {
				if e.userText == "" {
					continue
				}
				h := 0
				if i < len(m.frame.blockH) {
					h = m.frame.blockH[i]
				}
				lines = append(lines, fmt.Sprintf("  [%d] user=%q rowOffset=%d h=%d bottom=%d offScreen=%v",
					i, e.userText, e.rowOffset, h, e.rowOffset+h, e.rowOffset+h <= m.vp.YOffset))
			}
			m.append(stDim.Render(strings.Join(lines, "\n")))
			return nil
		}},
		{"resume", "[id]", "switch to another saved session (bare opens the picker)", resumeSlash},
		{"new", "", "start a fresh session in this workspace", newSlash},
		{"trace", "[on|off|file]", "record events+scroll to a file for debugging (default: dun-trace.jsonl in the temp dir)", func(m *tuiModel, args []string) tea.Cmd {
			if len(args) == 0 || args[0] == "on" {
				// Not the workspace by default: dun writes nothing into a repo
				// it was not asked to. A path given is the human's choice.
				path := filepath.Join(os.TempDir(), "dun-trace.jsonl")
				if len(args) >= 2 {
					path = args[1]
					if !filepath.IsAbs(path) {
						path = filepath.Join(m.workspace, path)
					}
				}
				f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
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
					if i < len(m.frame.blockH) {
						h = m.frame.blockH[i]
					}
					kind := "assistant"
					if e.userText != "" {
						kind = "user"
					} else if e.run != nil {
						kind = "tool"
					}
					fmt.Fprintf(f, "layout %d kind=%s userText=%q rowOffset=%d h=%d\n",
						i, kind, e.userText, e.rowOffset, h)
				}
				m.append(stDim.Render("tracing → " + filepath.Join(m.workspace, path)))
				return nil
			}
			if args[0] == "off" {
				if m.traceFile != nil {
					m.traceFile.Close()
					m.traceFile = nil
				}
				m.append(stDim.Render("tracing off"))
				return nil
			}
			m.append(stDim.Render("usage: /trace [on|off [file]]"))
			return nil
		}},
		{"reconnect", "", "restart the engine (after it gave up), keeping this session", func(m *tuiModel, _ []string) tea.Cmd {
			m.restarts, m.restartStart = 0, time.Now()
			m.reconnTries, m.reconnSince = 0, time.Time{}
			m.fatalErr = ""
			m.exitAnnounced = false
			m.skipHistory = m.everUp
			m.append(stDim.Render("reconnecting…"))
			return restartEngine(m.opts, m.sessionID, m.everUp, m.seen())
		}},
		{"skill", "[name] [text] | on|off|unset <name|glob> [--global|--project|--session] | mode listed|surfaced|unset [--scope]", "skills: bare opens the list (~/.dun/skills, .dun/skills, ~/.claude/skills, MCP servers) — type to filter, each one's cost, on/off at each scope, and the mode (listed in every request, or surfaced when a turn fits); on/off/unset and mode change it directly; with a name, asks the model to use it; with part of a name, lists the matches", skillSlash},
		{"exit", "", "exit dun: the session's engine stops too, unless /persist is on or another client is attached", func(m *tuiModel, _ []string) tea.Cmd {
			return m.exit()
		}},
		// /splash: a clean screen with the splash on it (USER 2026-10-27:
		// "it's fun"). The conversation is the engine's and stays; only what
		// this screen shows is cleared, and a /resume shows it again.
		{"splash", "", "clear the screen and show the splash again (the conversation is kept)", func(m *tuiModel, _ []string) tea.Cmd {
			m.convo, m.sel, m.paging, m.scr = nil, -1, false, nil
			m.showSplash()
			return tea.ClearScreen
		}},
		// /close exits, as /exit (USER 2026-10-27); discarding a session for
		// good is /discard.
		{"close", "", "exit dun, as /exit (to discard the session for good: /discard)", func(m *tuiModel, _ []string) tea.Cmd {
			return m.exit()
		}},
		{"persist", "[on|off|status]", "keep this session running after /exit (bare: on, or the status if already on)", func(m *tuiModel, args []string) tea.Cmd {
			mode := ""
			if len(args) > 0 {
				mode = strings.ToLower(args[0])
			}
			switch mode {
			case "", "on", "off", "status":
			default:
				m.append(stErr.Render("usage: /persist [on|off|status]"))
				return nil
			}
			if !m.proc.persist(mode) {
				m.append(stErr.Render("no session service right now — /reconnect, then try again"))
			}
			return nil
		}},
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
	b.WriteString("\n  " + stDim.Render("provider: ") + m.provider)
	b.WriteString("\n  " + stDim.Render("url:   ") + m.url)
	b.WriteString("\n  " + stDim.Render("model: ") + stTool.Render(m.model))
	b.WriteString("\n  " + stDim.Render("key:   ") + key)
	b.WriteString("\n  " + stDim.Render("saved: ") + configPath())
	b.WriteString("\n" + stDim.Render("use /model to switch models, or `dun --setup` to reconfigure"))
	m.append(b.String())
}

// sideCallBlock renders the per-kind stats for the LLM calls that are not the
// conversation turn. An empty map means no side calls this session (or an
// engine that predates the accounting), and the section says nothing at all —
// a "0 calls" row would be a fact about the absence of facts.
//
// Each row names what the kind IS in plain words, because the kind string is
// an engine identifier and a reader of /context should not have to know which
// helper "rescue" names. Latency shows the average and the last call — the
// average hides a slow one, the last one hides a fast streak; the two
// disagreeing is the interesting case. Tokens are per-kind running totals:
// processed/cached is the prompt the call re-read, generated is what it wrote.
// ctxLabelW is the label column of /context's aligned rows.
const ctxLabelW = 22

// ctxRow is one aligned /context row: the label padded to its column, the
// value, and a dim note after it. Labels longer than the column are cut, so
// a long server name cannot push its number out of line.
func ctxRow(label, value, note string) string {
	if lipgloss.Width(label) > ctxLabelW-1 {
		label = clip(label, ctxLabelW-1)
	}
	pad := strings.Repeat(" ", ctxLabelW-lipgloss.Width(label))
	return "\n  " + stDim.Render(label+pad) + value + stDim.Render(note)
}

// ctxNum is a count for a /context row: grouped, right-aligned, so a column
// of numbers lines up on its last digit.
func ctxNum(n int) string { return fmt.Sprintf("%9s", grouped(n)) }

func sideCallBlock(s *contextStats) string {
	if len(s.sideCalls) == 0 {
		return ""
	}
	// Stable order: map iteration is random and a re-render of /context must
	// not shuffle its own rows.
	kinds := make([]string, 0, len(s.sideCalls))
	for k := range s.sideCalls {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	what := map[string]string{
		"suggest":  "next-message predictions (/suggest)",
		"rephrase": "prompt rewrites (/rephrase)",
		"commit":   "commit messages (/worktree commit)",
		"rescue":   "context rescue compaction",
		"repaint":  "bug report redaction (/repaint report)",
	}

	var b strings.Builder
	b.WriteString("\n\n  " + stHeader.Render("side calls"))
	b.WriteString(stDim.Render(" — LLM calls that are not the turn"))
	for _, k := range kinds {
		r := s.sideCalls[k]
		label := k
		if w, ok := what[k]; ok {
			label = k + " (" + w + ")"
		}
		b.WriteString("\n  " + stDim.Render(commaf("%-12s", label)))
		b.WriteString(plural(r.calls, "call"))
		if r.calls > 0 {
			b.WriteString(stDim.Render(commaf(" · avg %d ms · last %d ms", r.avgMS, r.lastMS)))
		}
		tok := stDim.Render(commaf(" · %d proc / %d cached / %d gen", r.processed, r.cached, r.generated))
		if r.tokPerS > 0 {
			tok += commaf(" · %.0f tok/s", r.tokPerS)
		}
		b.WriteString(tok)
	}
	return b.String()
}

// windowBlock renders how the context window is divided.
//
// The section /context was missing, and the reason the failure it describes went
// unnoticed for a whole session: the old view could itemise the pre-conversation
// cost across five rows and could not say how big the window was, so a reader
// could see WHAT was expensive without ever seeing whether it was near the wall.
//
// Every figure here says where it came from. A token count that is measured and
// one that is the 4-chars-per-token default must never render alike — that
// equivalence is precisely what let a 31%-low estimate pass for a measurement
// until the endpoint refused to generate.
func windowBlock(s *contextStats) string {
	var b strings.Builder
	b.WriteString("\n\n  " + stHeader.Render("window"))
	if s.window <= 0 {
		// Not an absence of data — a STATE, and the one the yscr session ran in.
		// With no window there is nothing to shape against and no cap to send,
		// so the model generates until the endpoint stops it.
		b.WriteString("\n  " + stDim.Render("size:       ") + "not stated" +
			stDim.Render(" — no shaping, and no cap on a response"))
		b.WriteString("\n  " + stDim.Render("set DUN_CONTEXT_TOKENS, or use a server that states one."))
		return b.String()
	}

	pct := func(n int) string { return commaf(" (%d%%)", n*100/s.window) }
	b.WriteString("\n  " + stDim.Render("size:       ") + commaf("%d tokens", s.window))
	if s.promptTokens > 0 {
		b.WriteString("\n  " + stDim.Render("prompt:     ") +
			commaf("%d of %d budget", s.promptTokens, s.promptBudget) + stDim.Render(pct(s.promptTokens)))
	} else {
		b.WriteString("\n  " + stDim.Render("prompt:     ") +
			commaf("%d budget", s.promptBudget) + stDim.Render(" (nothing built yet)"))
	}
	if s.promptOverhead > 0 {
		// Charged once per REQUEST and invisible in any message, which is why it
		// gets its own row rather than being folded into the prompt: it is the
		// term a single chars-per-token ratio smears across the conversation.
		b.WriteString("\n  " + stDim.Render("  overhead: ") + commaf("%d", s.promptOverhead) +
			stDim.Render(" (schemas + template, once)"))
	}
	b.WriteString("\n  " + stDim.Render("response:   ") + commaf("%d reserved", s.windowReserved) +
		stDim.Render(commaf(" (cap %d + margin)", s.windowCap)))
	if s.promptTokens > 0 {
		free := s.window - s.promptTokens
		row := "\n  " + stDim.Render("room now:   ") + commaf("%d", free)
		if free < s.windowReserved {
			// The prompt has grown into the response's reservation. This is the
			// state a cut comes out of, so it is called out rather than left for
			// the reader to subtract.
			row += stNote.Render("  ← below the reservation")
		}
		b.WriteString(row)
	}

	how := stDim.Render(commaf(" (default — nothing measured yet; %d rounds)", s.ratioRounds))
	if s.ratioMeasured {
		how = stDim.Render(commaf(" (measured over %d rounds)", s.ratioRounds))
	}
	b.WriteString("\n  " + stDim.Render("ratio:      ") + commaf("%.2f chars/token", s.charsPerToken) + how)

	if s.windowCuts > 0 {
		// A cut is the budget having been WRONG, so it belongs here and not with
		// the compaction counters.
		what, plural := commaf("%d", s.windowCuts), " replies stopped for room"
		if s.windowCuts == 1 {
			plural = " reply stopped for room"
		}
		if s.windowFolds > 0 {
			what += commaf(" (%d folded)", s.windowFolds)
		}
		b.WriteString("\n  " + stDim.Render("cut short:  ") + what + stDim.Render(plural))
	}
	b.WriteString("\n  " + stDim.Render("the size is the server's when it states one; DUN_CONTEXT_TOKENS overrides it (the startup log names the source)"))
	return b.String()
}

// showContext appends a detailed breakdown of context usage to the conversation.
func (m *tuiModel) showContext() {
	s := &m.ctxStats
	var b strings.Builder
	b.WriteString(stHeader.Render("context"))

	b.WriteString("\n\n  " + stHeader.Render("tokens"))
	b.WriteString("\n  " + stDim.Render("processed:  ") + commaf("%d", s.processedTokens))
	b.WriteString("\n  " + stDim.Render("generated:  ") + commaf("%d", s.generatedTokens))
	b.WriteString("\n  " + stDim.Render("cached:     ") + commaf("%d", s.cachedTokens))
	b.WriteString("\n  " + stDim.Render("total:      ") + commaf("%d", s.totalTokens))
	b.WriteString("\n  " + stDim.Render("active:     ") + commaf("%d", s.activeTokens) + stDim.Render(" (last turn's window)"))
	b.WriteString("\n  " + stDim.Render("turns:      ") + commaf("%d", s.turns))

	b.WriteString(sideCallBlock(s))
	b.WriteString(surfacingBlock(s))
	b.WriteString(cacheBlock(s))
	b.WriteString(costBlock(s, time.Now()))
	b.WriteString(windowBlock(s))

	b.WriteString("\n\n  " + stHeader.Render("conversation"))
	b.WriteString("\n  " + stDim.Render("blocks:     ") + commaf("%d", len(m.convo)))
	b.WriteString("\n  " + stDim.Render("tools:      ") + commaf("%d", len(m.tools)))

	b.WriteString("\n\n  " + stHeader.Render("context shaping"))
	if s.compactions > 0 {
		b.WriteString("\n  " + stDim.Render("compactions: ") + commaf("%d", s.compactions) + stDim.Render(" · saved ") + commaf("%d tokens", s.tokensCompacted))
	} else {
		b.WriteString("\n  " + stDim.Render("compactions: ") + "0")
	}
	if s.recaps > 0 {
		b.WriteString("\n  " + stDim.Render("recaps:      ") + commaf("%d", s.recaps) + stDim.Render(" · ") + commaf("%d entries, %d chars → disk", s.entriesRecapped, s.charsRecapped))
	} else {
		b.WriteString("\n  " + stDim.Render("recaps:      ") + "0")
	}

	b.WriteString("\n\n  " + stHeader.Render("tool results"))
	b.WriteString("\n  " + stDim.Render("total:      ") + commaf("%d", s.toolResults))
	if s.resultsTruncated > 0 {
		b.WriteString("\n  " + stDim.Render("truncated:  ") + commaf("%d", s.resultsTruncated) + stDim.Render(" (LOD-capped at 4000 chars)"))
	} else {
		b.WriteString("\n  " + stDim.Render("truncated:  ") + "0")
	}

	b.WriteString("\n\n  " + stHeader.Render("system"))
	if s.systemTokens > 0 {
		how := " (estimated)"
		if s.systemExact {
			how = " (counted by the model's tokenizer)"
		}
		b.WriteString(ctxRow("prompt + schemas", ctxNum(s.systemTokens), " tokens"+how))
		// The breakdown is the point: one number says the total is large, and
		// these rows say which part to go and fix.
		if s.systemPrompt > 0 {
			b.WriteString(ctxRow("  system prompt", ctxNum(s.systemPrompt), ""))
		}
		// Part of the system prompt, not beside it: the project rules are the
		// one piece of it a user can change (/rules), so they get a row.
		if s.systemRulesMode != "" {
			b.WriteString(ctxRow("    project rules", ctxNum(s.systemRules), " (/rules "+s.systemRulesMode+")"))
		}
		for _, p := range s.systemParts {
			b.WriteString(ctxRow("  "+p.Name, ctxNum(p.Tokens), ""))
		}
		// Cost no single row owns — the provider's tool-use preamble, charged
		// once for declaring any tool at all. Shown only when there is some, and
		// named rather than folded into a server's row, because it does not go
		// away by turning one server off.
		if s.systemShared > 0 {
			b.WriteString(ctxRow("  tool-use overhead", ctxNum(s.systemShared), " (provider, shared)"))
		}
		if s.systemExact && len(s.systemParts) > 1 {
			b.WriteString("\n    " + stDim.Render("per-row figures are marginal: what you would save by removing that row."))
		}
	} else {
		b.WriteString(ctxRow("prompt + schemas", "not reported", ""))
	}

	b.WriteString("\n\n  " + stHeader.Render("out-of-band"))
	b.WriteString("\n  " + stDim.Render("queued msgs:    ") + commaf("%d", s.oobMessages) + stDim.Render(" (delivered mid-turn)"))
	if s.notificationsSmuggled > 0 {
		b.WriteString("\n  " + stDim.Render("notifications:  ") + commaf("%d", s.notificationsSmuggled) + stDim.Render(" (smuggled via queue)"))
	}
	if s.forcedToolCalls > 0 {
		b.WriteString("\n  " + stDim.Render("forced calls:   ") + commaf("%d", s.forcedToolCalls) + stDim.Render(" (host-injected)"))
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
	typed := strings.TrimPrefix(strings.TrimSpace(m.input.Value()), "/")
	var out []slashCmd
	for _, c := range m.allSlash() {
		if strings.Contains(c.name, " ") {
			// A subcommand listed on its own (/mcp add): matched by all the
			// words typed, so "/mcp tools" is not it.
			if strings.HasPrefix(c.name, typed) || strings.HasPrefix(typed, c.name+" ") {
				out = append(out, c)
			}
			continue
		}
		if debugCommands[c.name] && c.name != word {
			continue // a debug command shows once its whole name is typed
		}
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
	cmds := m.allSlash()
	for i := range cmds {
		if strings.Contains(cmds[i].name, " ") {
			continue // a subcommand's own palette row: /mcp runs /mcp
		}
		if cmds[i].name == name { // exact wins outright
			return cmds[i].run(m, fields[1:])
		}
		if strings.HasPrefix(cmds[i].name, name) && !debugCommands[cmds[i].name] {
			hit = &cmds[i]
			n++
		}
	}
	if n == 1 {
		return hit.run(m, fields[1:])
	}
	if n == 0 {
		// Not one of this TUI's: the engine has the last word (its list may
		// not have arrived yet). It answers an unknown one itself.
		return engineSlash(name)(m, fields[1:])
	}
	m.append(stDim.Render("ambiguous command: /" + name + " — try /help"))
	return nil
}

// runPaletteEnter runs the highlighted palette command (preserving any typed
// args), or the exactly-typed command.
func (m *tuiModel) runPaletteEnter(v string) tea.Cmd {
	// An argument choice highlighted: run the command with it.
	if cmd, l, ok := m.paletteArgs(); ok && len(l.choices) > 0 {
		sel := min(max(m.paletteSel, 0), len(l.choices)-1)
		m.paletteSel = 0
		// A choice that wants a value after it (a flag: --max) is filled in,
		// as tab does, not run without one (USER 2026-10-27: enter on --max
		// ran "--max" and said it needs a value).
		if l.choices[sel].more {
			m.input.SetValue("/" + cmd + " " + l.choices[sel].value + " ")
			m.input.CursorEnd()
			m.refresh()
			return nil
		}
		m.input.Reset()
		return m.runSlash("/" + cmd + " " + l.choices[sel].value)
	}
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
		if word := strings.TrimPrefix(fields[0], "/"); m.commandNamed(word) {
			return m.runSlash(v)
		}
	}
	if len(matches) == 0 {
		// None of the known ones: the engine decides (runSlash hands it on).
		return m.runSlash(v)
	}
	if c := matches[sel]; strings.Contains(c.name, " ") {
		// A subcommand's row. Typed as the command's word alone and not
		// chosen with the arrows (/mcp, enter), it is the command itself.
		first := strings.Fields(c.name)[0]
		if sel == 0 && len(fields) == 1 && strings.TrimPrefix(fields[0], "/") == first {
			return m.runSlash(v)
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(v), "/"), c.name)
		if rest == strings.TrimPrefix(strings.TrimSpace(v), "/") {
			rest = "" // the name was not typed out: no arguments
		}
		return c.run(m, strings.Fields(rest))
	}
	args := ""
	if len(fields) > 1 {
		args = " " + strings.Join(fields[1:], " ")
	}
	return m.runSlash("/" + matches[sel].name + args)
}

func (m tuiModel) commandNamed(name string) bool {
	for _, c := range m.allSlash() {
		if c.name == name {
			return true
		}
	}
	return false
}

// showHelp appends the command list to the conversation: without debug, the
// debug commands left out and counted on the last row.
func (m *tuiModel) showHelp(debug bool) {
	var b strings.Builder
	b.WriteString(stHeader.Render("commands"))
	hidden := 0
	for _, c := range m.allSlash() {
		if debugCommands[c.name] && !debug {
			hidden++
			continue
		}
		usage := "/" + c.name
		if c.args != "" {
			usage += " " + c.args
		}
		b.WriteString("\n  " + stCmd.Render(usage) + "  " + stDim.Render(c.desc))
	}
	if hidden > 0 {
		b.WriteString("\n\n  " + stDim.Render(plural(hidden, "debug command")+" hidden — /help --debug shows them"))
	}
	m.append(b.String())
}

// showCommandHelp is /help <command>: its usage, what it does, the long help
// where the command has one, and the values its first argument takes.
func (m *tuiModel) showCommandHelp(name string) {
	name = strings.TrimPrefix(name, "/")
	var c *slashCmd
	for _, s := range m.allSlash() {
		if s.name == name {
			c = &s
			break
		}
	}
	if c == nil {
		m.append(stErr.Render("no command /" + name + " — /help lists them"))
		return
	}
	var ec *engineCmd
	for i := range m.engineCmds {
		if m.engineCmds[i].name == name {
			ec = &m.engineCmds[i]
		}
	}
	usage := "/" + c.name
	if c.args != "" {
		usage += " " + c.args
	}
	rows := []string{stCmd.Render("/"+c.name) + "  " + c.desc}
	if ec != nil && ec.help != "" {
		rows = append(rows, "")
		for _, l := range strings.Split(ec.help, "\n") {
			rows = append(rows, "  "+l)
		}
	}
	if ec != nil && len(ec.choices) > 0 {
		rows = append(rows, "", stDim.Render("  "+usage))
		w := 0
		for _, ch := range ec.choices {
			w = max(w, len(ch.value))
		}
		for _, ch := range ec.choices {
			rows = append(rows, "  "+stCmd.Render(fmt.Sprintf("%-*s", w, ch.value))+"  "+stDim.Render(ch.desc))
		}
	} else if c.args != "" {
		rows = append(rows, stDim.Render("  "+usage))
	}
	m.append(strings.Join(rows, "\n"))
}

// palettePanel renders the live command list above the input (like the ask
// picker), the highlighted row gutter-marked.
func (m tuiModel) palettePanel() string {
	if cmd, l, ok := m.paletteArgs(); ok {
		return m.argPanel(cmd, l) + "\n" + m.input.View()
	}
	matches := m.paletteMatches()
	lo, hi := paletteRange(m.paletteSel, len(matches))
	rows := make([]string, 0, hi-lo+2)
	// A heading, as every panel below the conversation has one.
	rows = append(rows, stHeader.Render("commands"))
	// The descriptions in one column: the names padded to the widest one
	// shown. The arguments are on the row below the list, for the selected
	// command only — inline they pushed every description to a different
	// column.
	nameW := 0
	for _, c := range matches[lo:hi] {
		nameW = max(nameW, len(c.name)+1)
	}
	for i := lo; i < hi; i++ {
		c := matches[i]
		pad := strings.Repeat(" ", nameW-len(c.name)-1)
		name := stCmd.Render("/" + c.name)
		if i == m.paletteSel {
			name = "/" + c.name // listRow puts it in the accent
		}
		// A description too long for the row ends at a clause, so what
		// is left still says something (fitClause).
		desc := fitClause(c.desc, m.w-len(listIndent)-nameW-2)
		rows = append(rows, listRow("", name, pad+"  "+stDim.Render(desc), i == m.paletteSel))
	}
	// The count first: a usage too long for the row ends in an ellipsis.
	var foot []string
	if len(matches) == 0 {
		foot = append(foot, "no matching command · /help")
	} else if lo > 0 || hi < len(matches) {
		foot = append(foot, fmt.Sprintf("%d-%d of %d commands", lo+1, hi, len(matches)))
	}
	if m.paletteSel >= 0 && m.paletteSel < len(matches) && matches[m.paletteSel].args != "" {
		c := matches[m.paletteSel]
		foot = append(foot, "/"+c.name+" "+c.args)
	}
	if len(foot) > 0 {
		// A blank row first: the count is about the list, not a row of it
		// (run 5: list, count, input and hints stacked with no gap).
		rows = append(rows, "", stDim.Render("  "+strings.Join(foot, "  ·  ")))
	}
	return fitRows(strings.Join(rows, "\n"), m.w) + "\n" + m.input.View()
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
	b.WriteString(stripANSI(m.drawView()))
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
// reattach: it follows a session this TUI already shows.
type engineUpMsg struct {
	proc     *dunProc
	err      error
	reattach bool
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
	m.busy, m.starting = false, false
	m.dropAsk()
	m.askParked = false
	m.busyStart = time.Time{}
	// A reply cut mid-stream ends here: the next one is its own block, not
	// appended to it (seen after a dund crash and the resume).
	m.flushCur()
	reason := m.fatalErr
	if reason == "" {
		reason = "lost the session service (dund)"
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
	// We are leaving: let it go. An engine that announced its exit is not
	// a reason to stay down: the connection is to dund, and its ending means
	// dund went away (a restart into a new build stops the engines first,
	// so their exit arrives just before the connection ends).
	if m.quitting || m.reloadReq {
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
	m.reconnTries, m.reconnSince = 0, time.Now()
	m.skipHistory = true // it will replay what is already on screen
	m.append(stErr.Render(reason + " — reconnecting; the session is kept"))
	m.refresh()
	return m, restartEngine(m.opts, m.sessionID, true, m.seen())
}

// restartEngine spawns a replacement engine.
//
// reattach says whether there is a conversation to rejoin. It must be false for
// the very first engine: forcing --continue there would silently attach a fresh
// `dun -tui` to some unrelated older session, which is worse than the failure
// it is recovering from.
//
// after is the last event seq already shown: a reattach to the same session
// follows on from it rather than replaying the session from the start.
func restartEngine(o tuiOpts, sessionID string, reattach bool, after resumeAt) tea.Cmd {
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
		if !reattach {
			after = resumeAt{}
		}
		p, err := attachFn(o, after)
		if err != nil {
			return engineUpMsg{err: err, reattach: reattach}
		}
		return engineUpMsg{proc: p, reattach: reattach}
	}
}

// attachFn is attachSession; a test replaces it with a slow or failing one.
var attachFn = attachSession

// retryEngineMsg is a delayed retry after a failed spawn — an engine that
// cannot start usually cannot start a millisecond later either.
type retryEngineMsg struct{}

// A failed reconnect is retried after reconnectDelay, until reconnectFor has
// passed: long enough for dund to come back from a restart (about a second
// for a new build; systemd restarts a crashed one after 3 s), short enough
// that a service that is really down gets reported.
const (
	engineRetryDelay = time.Second
	reconnectFor     = time.Minute
)

// reconnectDelay is the wait before attempt n+1: 1 s, 2 s, 4 s, then 8 s.
func reconnectDelay(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	return engineRetryDelay << min(n-1, 3)
}

type dunProc struct {
	cmd     *exec.Cmd
	session string                   // the dund session this follows
	at      atomic.Pointer[resumeAt] // the last event seen, to resume after
	closed  atomic.Bool              // closed on purpose: its EOF is not a loss
	stdin   io.WriteCloser
	ch      chan tea.Msg
	trace   *traceWriter // DUN_TRACE: records what the engine said, for --replay
	replay  *replayStats // --replay: what the pacing did to the recording
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
	if o.noDir {
		args = append(args, "--no-dir")
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
	if o.rephrase {
		args = append(args, "--rephrase")
	}
	if o.cont {
		args = append(args, "--continue")
	}
	if o.resume != "" {
		args = append(args, "--resume", o.resume)
	}
	if o.fresh && mode == "--tui" {
		args = append(args, "--new")
	}
	return args
}

// See attach.go.

// The proc methods are nil-safe: between an engine dying and its replacement
// coming up there is no engine, and the UI stays usable in that window. They
// report whether the message actually went anywhere.
func (p *dunProc) send(content string, imgs []proto.Image) bool {
	if p == nil {
		return false
	}
	ev := map[string]any{"type": "user", "content": content}
	if len(imgs) > 0 {
		ev["images"] = imgs
	}
	return json.NewEncoder(p.stdin).Encode(ev) == nil
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
	p.closed.Store(true)
	p.trace.close()
	_ = p.stdin.Close()
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// seen is where this proc is in its session's events.
func (p *dunProc) seen() resumeAt {
	if p == nil {
		return resumeAt{}
	}
	if at := p.at.Load(); at != nil {
		return *at
	}
	return resumeAt{}
}

// seen is the last session event this TUI has shown.
func (m tuiModel) seen() resumeAt { return m.proc.seen() }

// waitEvent blocks for p's next event and delivers it tagged with p.
//
// The tag is what keeps ONE reader per connection. Every event re-arms the
// wait, and it used to re-arm on whatever m.proc was by then — so an event
// from a connection being replaced (a /reconnect, a session or model switch
// while it was still delivering) armed a SECOND reader on the new one. Two
// goroutines then took turns at one channel and Bubble Tea delivered their
// messages in whatever order they finished: streamed tokens arrived
// shuffled, and the reply was stored that way ("hasYour capture", inline
// code moved across words). Now an event says which connection it came
// from: the current one re-arms itself, an old one is drained to its end and
// ignored.
func waitEvent(p *dunProc) tea.Cmd {
	if p == nil || p.ch == nil {
		return nil
	}
	return func() tea.Msg { return procMsg{p: p, msg: <-p.ch} }
}

// procMsg is a message from one engine connection.
type procMsg struct {
	p   *dunProc
	msg tea.Msg
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

// readSystemCost decodes the pre-conversation cost off an event carrying it —
// context_cost at startup, usage after a turn. Shared by both so the two can
// never drift into disagreeing about the same numbers.
func (s *contextStats) readSystemCost(ev map[string]any) {
	v := evNum(ev["system_tokens"])
	if v <= 0 {
		return
	}
	s.systemTokens = int(v)
	s.systemExact, _ = ev["system_exact"].(bool)
	s.systemPrompt = int(evNum(ev["system_prompt"]))
	s.systemShared = int(evNum(ev["system_shared"]))
	s.systemParts = evParts(ev["system_parts"])
	s.systemRules = int(evNum(ev["system_rules"]))
	s.systemRulesMode = str(ev["system_rules_mode"])
	s.systemFiles = evParts(ev["system_files"])
}

// readWindow decodes the window division off an event carrying it — context_cost
// at startup, usage after every turn. A window of 0 IS the message ("nobody
// stated one"), so unlike readSystemCost this does not bail on a zero.
func (s *contextStats) readWindow(ev map[string]any) {
	if _, ok := ev["window"]; !ok {
		return // an older event, or one from a path that does not carry it
	}
	s.window = int(evNum(ev["window"]))
	s.windowReserved = int(evNum(ev["window_reserved"]))
	s.windowCap = int(evNum(ev["window_cap"]))
	s.promptBudget = int(evNum(ev["prompt_budget"]))
	s.promptTokens = int(evNum(ev["prompt_tokens"]))
	s.promptOverhead = int(evNum(ev["prompt_overhead"]))
	s.charsPerToken = evNum(ev["chars_per_token"])
	s.ratioMeasured, _ = ev["ratio_measured"].(bool)
	s.ratioRounds = int(evNum(ev["ratio_rounds"]))
	s.windowCuts = int(evNum(ev["window_cuts"]))
	s.windowFolds = int(evNum(ev["window_folds"]))
}

// readSurfacing decodes proactive RAG's totals off a usage event. Like
// readSideCalls, a missing field keeps the last snapshot.
func (s *contextStats) readSurfacing(ev map[string]any) {
	v, ok := ev["surfacing"].(map[string]any)
	if !ok {
		return
	}
	s.surfacing = make(map[string]float64, len(v))
	for k, x := range v {
		s.surfacing[k] = evNum(x)
	}
}

// surfacingBlock is the /context section for proactive RAG. The carried line
// is the point of it: a surfaced entry is re-sent every round until a fold
// drops it, so "appended" understates the cost and "carried" is what it has
// actually been in prompt tokens. Empty before the first pass.
func surfacingBlock(s *contextStats) string {
	if s.surfacing == nil {
		return ""
	}
	f := func(k string) int { return int(s.surfacing[k]) }
	entries := func(n int) string {
		if n == 1 {
			return "1 entry"
		}
		return commaf("%d entries", n)
	}
	var b strings.Builder
	b.WriteString("\n\n  " + stHeader.Render("surfacing"))
	b.WriteString(stDim.Render(" — proactive RAG added to the conversation"))
	b.WriteString("\n  " + stDim.Render("passes:     ") + commaf("%d", f("passes")) +
		stDim.Render(commaf(" · avg %d ms · last %d ms", f("avg_ms"), f("last_ms"))))
	b.WriteString("\n  " + stDim.Render("surfaced:   ") + commaf("%d of %d found", f("surfaced"), f("found")))
	b.WriteString("\n  " + stDim.Render("appended:   ") + commaf("%s · %d bytes · ~%d tok", entries(f("entries")), f("bytes"), f("tokens")))
	b.WriteString("\n  " + stDim.Render("in history: ") + commaf("%s · ~%d tok", entries(f("in_history_entries")), f("in_history")) +
		stDim.Render(" (re-sent every round)"))
	b.WriteString("\n  " + stDim.Render("carried:    ") + commaf("~%d tok", f("carried")) +
		stDim.Render(commaf(" over %s", plural(f("rounds"), "round"))))
	return b.String()
}

// readCache keeps the usage event's "cache" map; a missing field keeps the
// last snapshot.
func (s *contextStats) readCache(ev map[string]any) {
	if v, ok := ev["cache"].(map[string]any); ok {
		s.cache = v
	}
}

// cacheBlock is the /context section for the prompt cache. Rounds are split by
// the build that preceded them, because the two causes of a miss need opposite
// fixes: a REWRITE is dun's shaping (fold, tool change, LOD step); a low hit
// on an APPEND-ONLY round means the server lost the slot in between. Empty
// before the first build.
func cacheBlock(s *contextStats) string {
	if s.cache == nil {
		return ""
	}
	n := func(m map[string]any, k string) int { return int(evNum(m[k])) }
	var b strings.Builder
	b.WriteString("\n\n  " + stHeader.Render("cache"))
	b.WriteString(stDim.Render(" — what the server re-read, by the build before each round"))
	b.WriteString("\n  " + stDim.Render("rounds:     ") + commaf("%d · %.0f%% hit", n(s.cache, "rounds"), evNum(s.cache["hit_pct"])) +
		stDim.Render(commaf(" · %d cached / %d processed", n(s.cache, "cached"), n(s.cache, "processed"))))
	classes, _ := s.cache["classes"].(map[string]any)
	rows := []struct{ key, label string }{
		{"first", "first"},
		{"back_to_back", "append <1s"},
		{"after_gap", "append, gap"},
		{"rewrite", "rewrite"},
	}
	for _, r := range rows {
		c, ok := classes[r.key].(map[string]any)
		if !ok {
			continue
		}
		b.WriteString("\n  " + stDim.Render(commaf("%-12s", r.label)) +
			commaf("%d · %.0f%% hit", n(c, "rounds"), evNum(c["hit_pct"])) +
			stDim.Render(commaf(" · %d processed", n(c, "processed"))))
	}
	b.WriteString("\n  " + stDim.Render("builds:     ") + commaf("%d · %d append-only", n(s.cache, "builds"), n(s.cache, "append_only")))
	causes := []string{"fold", "rehead", "recap", "tools", "other"}
	rw := 0
	for _, c := range causes {
		rw += n(s.cache, "rewrites_"+c)
	}
	if rw > 0 {
		var parts []string
		for _, c := range causes {
			parts = append(parts, commaf("%s %d", c, n(s.cache, "rewrites_"+c)))
		}
		b.WriteString("\n  " + stDim.Render("rewrites:   ") + commaf("%d", rw) +
			stDim.Render(commaf(" (%s) · %d chars re-sent", strings.Join(parts, " · "), n(s.cache, "resent_chars"))))
	}
	if rh := n(s.cache, "reheads"); rh > 0 {
		b.WriteString("\n  " + stDim.Render("re-headed:  ") + commaf("%d superseded reads", n(s.cache, "rehead_entries")) +
			stDim.Render(commaf(" · %d chars · in %d folds", n(s.cache, "rehead_chars"), rh)))
	}
	if last := n(s.cache, "last_chars"); last > 0 {
		b.WriteString("\n  " + stDim.Render("last build: ") + commaf("kept %d of %d chars", n(s.cache, "last_kept_chars"), last))
	}
	return b.String()
}

// readSideCalls decodes the per-kind side-call totals off a usage event. The
// engine sends the whole map (a session total, not a delta), so the read
// REPLACES the TUI's copy. An event without the field (an older engine, or a
// turn from before the first side call) leaves the previous snapshot in
// place: the last real numbers are still the best knowledge, and a section
// that blinks away on every turn is worse than one that lags.
func (s *contextStats) readSideCalls(ev map[string]any) {
	v, ok := ev["side_calls"].(map[string]any)
	if !ok {
		return
	}
	s.sideCalls = make(map[string]sideCallStats, len(v))
	for kind, row := range v {
		rm, ok := row.(map[string]any)
		if !ok {
			continue
		}
		s.sideCalls[kind] = sideCallStats{
			calls:     int(evNum(rm["calls"])),
			latencyMS: int64(evNum(rm["latency_ms"])),
			lastMS:    int64(evNum(rm["last_ms"])),
			processed: int(evNum(rm["processed"])),
			cached:    int(evNum(rm["cached"])),
			generated: int(evNum(rm["generated"])),
			avgMS:     int64(evNum(rm["avg_ms"])),
			tokPerS:   evNum(rm["tok_per_s"]),
		}
	}
}

// evParts decodes the context breakdown rows off a usage event. A row that is
// missing or malformed is dropped rather than rendered as zero, because a
// zero-token MCP server on the /context table reads as a real measurement.
func evParts(v any) []dun.ContextPart {
	rows, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]dun.ContextPart, 0, len(rows))
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		out = append(out, dun.ContextPart{Name: name, Tokens: int(evNum(m["tokens"]))})
	}
	return out
}
