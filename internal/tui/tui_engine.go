package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

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
		fmt.Fprintf(&b, "streaming: %q\n", textfmt.Clip(textfmt.OneLine(m.cur), 200))
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
			m.append(StDim.Render("replayed " + s))
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
		m.append(StErr.Render(m.fatalErr + "\n/reconnect to try again · your conversation is saved: dun --continue"))
		m.refresh()
		return m, nil
	}
	m.restarts++
	m.fatalErr = ""
	m.reconnTries, m.reconnSince = 0, time.Now()
	m.skipHistory = true // it will replay what is already on screen
	m.append(StErr.Render(reason + " — reconnecting; the session is kept"))
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
func restartEngine(o TuiOpts, sessionID string, reattach bool, after resumeAt) tea.Cmd {
	return func() tea.Msg {
		if reattach {
			if sessionID != "" {
				o.Resume, o.Cont = sessionID, false
			} else {
				// The engine died before naming its session; the workspace's
				// most recent one is the one that just died.
				o.Resume, o.Cont = "", true
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

// ProcArgs builds a `dun <mode>` argv from the shared flags. mode is "-p" (the
// engine, for the TUI) or "--tui" (the full TUI, for the xterm/PTY terminal
// view served at /term).
func ProcArgs(o TuiOpts, mode string) []string {
	args := []string{mode, "--workspace", o.Workspace}
	if o.DisableExit && mode == "--tui" {
		args = append(args, "--disable-exit")
	}
	if !o.Suggest {
		args = append(args, "--no-suggest") // propagate to -p (engine) and web -tui
	}
	if o.Rag != "" {
		args = append(args, "--rag="+o.Rag)
	}
	if o.NoDir {
		args = append(args, "--no-dir")
	}
	if o.Lsp != "" {
		args = append(args, "--lsp="+o.Lsp)
	}
	if o.NoTools {
		args = append(args, "--no-tools")
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Url != "" {
		args = append(args, "--url", o.Url)
	}
	if o.Key != "" {
		args = append(args, "--key", o.Key)
	}
	if o.Docker != "" {
		args = append(args, "--docker", o.Docker)
	}
	if o.DockerNetwork {
		args = append(args, "--docker-network")
	}
	if o.Worktree {
		args = append(args, "--worktree")
	}
	if o.Rephrase {
		args = append(args, "--rephrase")
	}
	if o.Cont {
		args = append(args, "--continue")
	}
	if o.Resume != "" {
		args = append(args, "--resume", o.Resume)
	}
	if o.Fresh && mode == "--tui" {
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
