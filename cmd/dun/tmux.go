package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// tmux integration (owner, 2026-10-27): the TUI marks its own tmux window
// with what it waits on, so a window list shows which dun needs you.
//
// It sets two WINDOW user options on the window that holds $TMUX_PANE:
//
//	@dun_icon   ⚙ working · ❓ question · ✓ done · empty when idle
//	@dun_state  working | question | done | idle
//
// tmux shows neither until the window-status formats ask for them; `dun
// tmux` prints the lines to add. dun never renames a window and never edits
// tmux.conf. A question also rings the terminal bell once (DUN_BELL=off
// turns it off), which tmux flags in the window list with no configuration,
// and which works in a plain terminal too.

// Attention states, for @dun_state.
const (
	attnIdle     = "idle"
	attnWorking  = "working"
	attnQuestion = "question"
	attnDone     = "done"
	attnError    = "error" // a turn failed (USER 2026-10-27: "! for error or problem")
)

// attnIcon is @dun_icon for a state.
func attnIcon(state string) string {
	switch state {
	case attnWorking:
		return "⚙"
	case attnQuestion:
		return "❓"
	case attnDone:
		return "✓"
	case attnError:
		return "!"
	}
	return ""
}

// attnStateOf maps the model to the state its window shows. A question
// first: it is the one state that stops the agent until the person acts.
func attnStateOf(m tuiModel) string {
	switch {
	// Anything waiting on the person is a question: ask_user, a login, a
	// yes/no confirmation.
	case m.asking || m.confirm.q != "" || slices.ContainsFunc(m.convo, func(e convoEntry) bool { return e.needLogin }):
		return attnQuestion
	case m.busy:
		return attnWorking
	case m.attnErr:
		return attnError
	case m.attnDone:
		return attnDone
	}
	return attnIdle
}

// syncAttn is called once per Update, after the message is handled: it
// notes a turn that ended (prev is the model before the message), clears
// the done mark on the person's return, and on a change of state tells tmux
// and the iTerm2 tab (iterm.go), and rings the bell when a question arrives.
func syncAttn(prev, t tuiModel, msg tea.Msg) (tuiModel, tea.Cmd) {
	switch msg.(type) {
	case tea.FocusMsg:
		t.attnFocus = 1
		t.attnDone = false
	case tea.BlurMsg:
		t.attnFocus = -1
	case tea.KeyPressMsg, tea.PasteMsg:
		t.attnDone, t.attnErr = false, false
	}
	if t.busy && !prev.busy {
		t.attnErr = false // a new turn: the last one's failure is old news
	}
	// A turn that ended without a question: done, unless the terminal has
	// said the person is looking at this pane right now.
	if prev.busy && !t.busy && !t.asking && t.attnFocus != 1 {
		t.attnDone = true
	}
	state := attnStateOf(t)
	if state == t.attnState {
		return t, nil
	}
	t.attnState = state
	t.tmux.show(state)
	ring := (state == attnQuestion || state == attnError) && bellOn()
	out := t.iterm.show(state, ring)
	if ring {
		out = bel + out
	}
	if out == "" {
		return t, nil
	}
	return t, tea.Raw(out)
}

// bellOn is DUN_BELL: anything but off/0/false/no rings.
func bellOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DUN_BELL"))) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// tmuxRunner runs one tmux command. Injected in tests.
type tmuxRunner func(ctx context.Context, args ...string) error

func execTmux(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, "tmux", args...).Run()
}

// tmuxSync sets the window options off the UI goroutine. show never blocks:
// it records the latest state and wakes the worker, which applies only the
// newest state it finds and then waits gap before the next call, so a burst
// of changes is a few calls, not one per change. nil (outside tmux) does
// nothing.
type tmuxSync struct {
	pane    string
	run     tmuxRunner
	gap     time.Duration // minimum time between two calls
	timeout time.Duration // per tmux call

	mu      sync.Mutex
	pending string // latest state not yet applied
	has     bool   // pending is set
	applied string // last state applied ("" none)
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// newTmuxSync is nil unless this process runs in a tmux pane.
func newTmuxSync(getenv func(string) string, run tmuxRunner) *tmuxSync {
	pane := getenv("TMUX_PANE")
	if getenv("TMUX") == "" || pane == "" {
		return nil
	}
	s := &tmuxSync{
		pane: pane, run: run, gap: 300 * time.Millisecond, timeout: time.Second,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *tmuxSync) show(state string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.pending, s.has = state, true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *tmuxSync) loop() {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
		}
		s.mu.Lock()
		state, has := s.pending, s.has
		s.has = false
		same := state == s.applied
		s.mu.Unlock()
		if !has || same {
			continue
		}
		s.apply(state)
		s.mu.Lock()
		s.applied = state
		s.mu.Unlock()
		select {
		case <-s.stop:
			return
		case <-time.After(s.gap):
		}
	}
}

// apply sets both options in one tmux call. An idle window keeps no icon:
// the option is unset, so a format testing #{?@dun_icon,…} shows nothing.
func (s *tmuxSync) apply(state string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	if state == attnIdle {
		_ = s.run(ctx, "set-option", "-wu", "-t", s.pane, "@dun_icon", ";",
			"set-option", "-w", "-t", s.pane, "@dun_state", attnIdle)
		return
	}
	_ = s.run(ctx, "set-option", "-w", "-t", s.pane, "@dun_icon", attnIcon(state), ";",
		"set-option", "-w", "-t", s.pane, "@dun_state", state)
}

// close stops the worker and unsets both options. Safe to call twice and on
// nil; runTUI calls it after the program ends and defers it for a panic.
func (s *tmuxSync) close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()
		_ = s.run(ctx, "set-option", "-wu", "-t", s.pane, "@dun_icon", ";",
			"set-option", "-wu", "-t", s.pane, "@dun_state")
	})
}

// ── dun tmux ───────────────────────────────────────────────────────

// tmuxIconPrefix goes in front of a window-status format: the icon and a
// space when the window has one, nothing otherwise.
const tmuxIconPrefix = "#{?@dun_icon,#{@dun_icon} ,}"

// tmuxDefaultFormat is tmux's own window-status-format default.
const tmuxDefaultFormat = "#I:#W#{?window_flags,#{window_flags}, }"

// tmuxQuery returns a tmux command's output.
type tmuxQuery func(args ...string) (string, error)

func execTmuxQuery(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", args...).Output()
	return strings.TrimRight(string(out), "\n"), err
}

// tmuxCmd is `dun tmux`: the tmux.conf lines that show the icon, and whether
// the running tmux shows it already. inTmux is $TMUX being set; iterm is
// LC_TERMINAL=iTerm2, for the passthrough line the tab escapes need.
func tmuxCmd(w io.Writer, inTmux, iterm bool, q tmuxQuery) int {
	fmt.Fprintln(w, "dun marks its tmux window with @dun_icon (⚙ working, ❓ question, ✓ done) and @dun_state; add these lines to ~/.tmux.conf to show the icon in the window list:")
	fmt.Fprintln(w)
	shown := 0
	for _, opt := range []string{"window-status-format", "window-status-current-format"} {
		cur, err := "", error(nil)
		if inTmux {
			cur, err = q("show-options", "-gv", opt)
		}
		if strings.Contains(cur, "@dun_icon") {
			shown++
		}
		// Built on the current format, so the user's own styling stays; a
		// format with a single quote cannot be quoted in one, so the default.
		base := cur
		if !inTmux || err != nil || cur == "" || strings.Contains(cur, "'") || strings.Contains(cur, "@dun_icon") {
			base = tmuxDefaultFormat
		}
		fmt.Fprintf(w, "  set -g %s '%s%s'\n", opt, tmuxIconPrefix, base)
	}
	if iterm {
		fmt.Fprintln(w, "  set -g allow-passthrough on   # iTerm2: lets dun colour and title the tab from inside tmux")
	}
	fmt.Fprintln(w)
	switch {
	case !inTmux:
		fmt.Fprintln(w, "not inside tmux: run dun tmux in tmux to check what it shows")
	case shown == 2:
		fmt.Fprintln(w, "this tmux shows the icon already")
	case shown == 1:
		fmt.Fprintln(w, "this tmux shows the icon in one of the two formats only")
	default:
		fmt.Fprintln(w, "this tmux does not show the icon yet; after editing, run: tmux source-file ~/.tmux.conf")
	}
	fmt.Fprintln(w, "a question also rings the terminal bell, which tmux marks in the window list; DUN_BELL=off turns it off")
	return 0
}
