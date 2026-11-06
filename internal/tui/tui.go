// Package tui is dun's terminal UI.
//
// The TUI is a CLIENT of the `-p` JSON event protocol: it re-execs `dun -p`,
// writes user events to its stdin, and renders the event stream. The engine
// stays headless; the UI is pure presentation.
package tui

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dunconfig"
)

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
	paneH     int      // the pane's height when it was built (scrollfold.go)
}
type TuiOpts struct {
	Workspace, Model, Url, Key, Docker string
	DockerNetwork                      bool
	Worktree                           bool
	Cont                               bool   // --continue: resume the latest session
	Resume                             string // --resume <id>: resume a specific session
	DisableExit                        bool   // --disable-exit: ctrl+c/esc don't quit (use /exit)
	Suggest                            bool   // --no-suggest=false: engine emits suggestions after each turn
	// Rag/lsp are --rag/--lsp as typed: "" (unset, use the saved setting),
	// "true" or "false". Passed through to the -p engine verbatim.
	Rag, Lsp string
	// Rephrase is --rephrase: rephrase each user prompt for specificity
	// before acting on it. Passed to the engine verbatim (a CLI flag), so it
	// survives a /reload or engine restart; /rephrase on|off flips the RUNTIME
	// copy, which wins over this at turn time.
	Rephrase bool
	// Fresh is --new (and /new): a new session even when the workspace has
	// one nobody is attached to. Without it a plain `dun` reuses that one.
	Fresh bool
	// NoDir is --no-dir: passed to the engine, whose workspace is dun's
	// directory for such sessions (dun/nodir.go).
	NoDir bool
	// NoTools is --no-tools: a plain chat. Passed to the engine, and so
	// through dund (internal/dund allowedFlags). --no-rag and --no-lsp are
	// here as rag/lsp = "false".
	NoTools bool
	// ItermTabs is --exp-iterm-tabcontrol: title and colour the iTerm2 tab
	// (iterm.go). This client's terminal only; procArgs leaves it out.
	ItermTabs bool
}

// RunTUI launches the Bubble Tea app against a re-exec'd `dun -p` subprocess.
// lc (may be nil) is the launcher registration — its reload channel drives the
// "update ready" indicator + /reload.
func RunTUI(o TuiOpts) error {
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
	m := newTUIModel(nil, o.Workspace)
	m.startingStart = time.Now()
	m.model, m.url, m.keySet = o.Model, o.Url, o.Key != "" // for /config
	m.provider = dunconfig.HostName(o.Url)
	if p, ok := dunconfig.LoadConfig().ProviderAt(o.Url); ok {
		m.provider = p.Name
	}
	m.disableExit = o.DisableExit
	m.opts = o // for respawning the engine (see eofMsg)
	m.checkUpdates = true
	// The window and tab marks (tmux.go, iterm.go). Unset at exit, and on a
	// panic through here by the defer.
	m.tmux = newTmuxSync(os.Getenv, execTmux)
	m.iterm = newItermTab(o.ItermTabs, os.Getenv, o.Workspace)
	restoreTab := func() {
		m.tmux.close()
		if r := m.iterm.restore(); r != "" {
			os.Stdout.WriteString(r) // the program has ended: nobody else writes
		}
	}
	defer restoreTab()
	// Alt screen, mouse mode and keyboard enhancements are View fields (View).
	// Bubble Tea v2 asks for the kitty keyboard protocol (flags=1) and xterm
	// modifyOtherKeys itself, decodes both, and puts the terminal back at exit.
	fm, err := tea.NewProgram(m).Run()
	restoreTab() // before a /reload's exec, which skips the defer
	if tm, ok := fm.(tuiModel); ok {
		tm.saveUI()     // the draft and sent history, for this user's next client
		tm.proc.close() // the engine it ENDED with, which may not be the one it started
	}
	// /reload: bubbletea has restored the terminal by now, so re-exec cleanly
	// into the binary on disk (after a `make install`, the new one) with the
	// same args, preserving the session so the conversation is replayed on
	// reconnect.
	if tm, ok := fm.(tuiModel); ok && tm.reloadReq && err == nil {
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
			case tm.opts.Cont:
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
	thinkFull    bool           // ctrl+x t: thoughts are drawn whole, not as their one line
	holdSkip     bool           // the next refresh replaces the conversation: no contract hold (dropHold)
	// foldedRows is the rows folded away above the view this session
	// (scrollfold.go): the conversation's height plus it never goes down.
	foldedRows int

	// panelHeld: a panel below (palette, form, picker) shrank the pane and
	// the pinned view keeps panelYOff while it is open and the conversation
	// is no longer than panelRows (applyScroll).
	panelHeld    bool
	panelYOff    int
	panelRows    int
	panelFollow  bool                  // new rows arrived with a panel open, this update: a shrink keeps them in view
	md           *glamour.TermRenderer // markdown renderer for assistant replies
	mdWidth      int                   // width md was built for (rebuild only on change)
	history      []string              // sent inputs, recalled with ctrl+↑/↓
	histIdx      int                   // cursor into history (== len when not browsing)
	mouseMode    string                // "all" | "cell" | "off" — what /mouse last set
	focus        int                   // focusInput | focusConvo | focusActivity
	task         string                // the last user message, for the task line
	agents       []agentRow            // live sub-agents (activity.go)
	jobs         []jobRow              // live background jobs (activity.go)
	actLevel     int                   // actCollapsed | actList — → descends, ← ascends
	actSel       actKey                // selected activity row, by identity not index
	actOrder     []actKey              // the open table's order, taken when it opened (activity.go)
	actStackOpen bool                  // the dismissed agents' stack is listed open (activity.go)
	// Agent scope: which child's conversation is on screen (0 = the root's),
	// and the root's own conversation, stashed while it is (activity.go).
	scopeAgent   int
	rootView     convoView
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
	askParked    bool   // a question is open but its picker was put away (askpark.go)
	flashText    string // a short status-line note (askpark.go)
	// slashLine is the slash line being run, as typed: /schedule and /loop
	// send their text with its spacing and newlines, which the fields every
	// command gets have lost. schedEditAsk is the entry a bare
	// "/schedule edit <id>" from THIS client asked for: only that client's
	// composer takes the text back (every client sees the event).
	slashLine    string
	schedEditAsk string
	flashUntil   time.Time      // when it goes
	pulsePending bool           // a pulse tick is scheduled
	bandRaw      []string       // the selected block's rows before the band, for a pulse step
	bandAt       int            // the pane row they start at
	pg           pager          // the pager (valid while paging)
	scr          *paneScreen    // a pushed screen over the pager (panes.go), or nil
	scrCmd       tea.Cmd        // what a screen's key left to run (/who asking dund again)
	extWant      bool           // /ext asked for the list (extview.go)
	planWant     bool           // /plan asked for the plan (planview.go)
	planTelling  *planTellTo    // the composer is telling a plan node's owner (planview.go)
	ctxSettings  map[string]any // /context config's settings in force, from the settings event
	effort       string         // /effort's level, from the settings event
	effortWords  string         // what that level does, from the settings event (the server's default, when it says)
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
	reloadReq   bool          // /reload requested → runTUI re-execs after quit
	upgrade     *upgradeState // a pending dund upgrade (upgrade.go); nil = none
	// checkUpdates: a release build's daily check runs behind the first
	// frame (release.go); set by runTUI only.
	checkUpdates bool
	// Engine supervision: the TUI outlives its engine. opts is what respawning
	// one takes, sessionID reattaches it to the same conversation, and the
	// restart counters stop a crash loop from spinning forever.
	opts         TuiOpts
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
	chord       string    // a chord started (ctrl+x), waiting for its next key (composerkeys.go)
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
	modelScope   string     // the provider the picker lists, "" = every one (tab moves it)
	// The login's service picker (/login): custom, then the templates.
	loginPicking bool
	logins       pickList
	loginProbe   map[string]int // local template id → models it answered with; absent: not answering
	// The add-provider form (/model add, ctrl+a in the picker).
	providerForm       bool
	providerFields     [3]textinput.Model // address, name, key (fieldAddr, fieldName, fieldKey)
	providerStep       int
	providerErr        string
	providerAuto       string               // the name the form filled in from the address; replaced while it is unedited
	providerEdit       string               // the provider the form edits; "" adds one
	providerChecking   string               // the service being checked, asked for its models before it is saved (saveProvider)
	providerPending    dunconfig.ProviderOp // what is saved once the check passes
	providerTmpl       *loginTmpl           // the template the form was opened from; nil: custom, or again
	providerKeysOpened bool                 // the keys page was opened for this form
	providerEnvKey     string               // the template's key variable that is set; "" none
	providerBack       bool                 // esc goes back to the service picker
	provider           string               // name of the provider the engine runs on
	// /rag setup (ragsetup.go); nil when it is not open.
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
	keys           keyMap            // keymap.go: the key pressed → the key the handlers match
	keysErr        error             // keys.json would not load: said once, defaults in use
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
	scrollPinned      bool // true when viewport should auto-follow (at bottom)
	// userJump is the convo index ctrl+home last parked on, and userJumpAt the
	// offset it left; a press with the view still there steps further back.
	userJump, userJumpAt int
	traceFile            *os.File // /trace on: recording events+scroll to this file
	tracePrevYOff        int      // last recorded YOffset (avoid duplicates)
	// Context stats (for /context)
	ctxStats contextStats

	// What the window or tab shows (tmux.go, iterm.go): the last state sent,
	// a turn that ended unseen, and the terminal's focus report (1 in, -1
	// out, 0 never said). tmux and iterm are nil when not in one.
	attnState string
	attnDone  bool
	attnErr   bool // the last turn failed; cleared by a key or the next turn
	attnFocus int8
	// attnLive: the person typed or a turn ran. Before that, a question on
	// screen (not logged in) is one they are looking at as dun opens, so the
	// tab shows it but the bell does not ring.
	attnLive bool
	tmux     *tmuxSync
	iterm    *itermTab
	// reqs is the last request's figures, for the top bar (reqbar.go).
	reqs reqBar
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
	keys, keysErr := loadKeyMap()
	return tuiModel{proc: proc, workspace: workspace, vpc: &viewportCache{}, input: in, search: se, spin: sp, dumpSig: sig, starting: true, sel: -1, pendingTool: -1, scrollPinned: true, userJump: -1, suggestMode: "auto", mouseMode: "all", shown: &shownFrame{}, evlog: newEventRing(), keys: keys, keysErr: keysErr}
}
func (m tuiModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick, m.input.BlinkTick(), waitForDump(m.dumpSig)}
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

// updateNoteMsg is the release check's line ("" for none).
type updateNoteMsg string

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Timed as a whole: this is what a keystroke queues behind, and it is the
	// number that decides whether dun warns about itself (see perf.go).
	start := time.Now()
	msg = normalizeKey(msg)
	if msg = m.keys.translate(m.keyScope(), msg); msg == nil {
		return m, nil // a default key whose action was bound elsewhere (keymap.go)
	}
	kind := msgKind(msg)
	m.evlog.add(start, kind, msg) // for /repaint (repaint.go)
	nm, cmd := m.update(msg)
	if frames.observeMsg(stageUpdate, time.Since(start), kind) {
		// Noticed its own stutter. Say it once, in the conversation, where the
		// person having the problem is actually looking.
		if t, ok := nm.(tuiModel); ok {
			t.append(StErr.Render(frames.slowWarning()))
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
		// The window's mark (tmux.go): after the message, for the same reason.
		var attnCmd tea.Cmd
		if t, attnCmd = syncAttn(m, t, msg); attnCmd != nil {
			cmd = tea.Batch(cmd, attnCmd)
		}
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
