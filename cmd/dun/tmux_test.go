package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestAttnStateOf(t *testing.T) {
	cases := []struct {
		name string
		m    tuiModel
		want string
	}{
		{"idle", tuiModel{}, attnIdle},
		{"working", tuiModel{busy: true}, attnWorking},
		{"question", tuiModel{busy: true, asking: true}, attnQuestion},
		{"login", tuiModel{convo: []convoEntry{{needLogin: true}}}, attnQuestion},
		{"done", tuiModel{attnDone: true}, attnDone},
		{"working over done", tuiModel{busy: true, attnDone: true}, attnWorking},
		{"confirmation", tuiModel{busy: true, confirm: confirmState{q: "kill every running job? (y/n)"}}, attnQuestion},
		{"error", tuiModel{attnErr: true}, attnError},
		{"error over done", tuiModel{attnErr: true, attnDone: true}, attnError},
		{"question over error", tuiModel{attnErr: true, asking: true}, attnQuestion},
	}
	for _, c := range cases {
		if got := attnStateOf(c.m); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// A turn that ends marks done; the next key press clears it, and so does
// a focus-in. A turn that ends while the terminal says the pane has focus
// is not marked.
func TestAttnDoneClears(t *testing.T) {
	t.Setenv("DUN_BELL", "off")
	busy := tuiModel{busy: true, attnState: attnWorking}
	ended := busy
	ended.busy = false
	m, _ := syncAttn(busy, ended, nil)
	if !m.attnDone || m.attnState != attnDone {
		t.Fatalf("turn end: done=%v state=%q, want done", m.attnDone, m.attnState)
	}
	k, _ := syncAttn(m, m, tea.KeyPressMsg{Code: 'a', Text: "a"})
	if k.attnDone || k.attnState != attnIdle {
		t.Fatalf("after a key: done=%v state=%q, want idle", k.attnDone, k.attnState)
	}
	f, _ := syncAttn(m, m, tea.FocusMsg{})
	if f.attnDone || f.attnState != attnIdle || f.attnFocus != 1 {
		t.Fatalf("after focus-in: done=%v state=%q focus=%d", f.attnDone, f.attnState, f.attnFocus)
	}
	// Focused: the end of a turn is seen.
	busy.attnFocus = 1
	ended.attnFocus = 1
	if s, _ := syncAttn(busy, ended, nil); s.attnDone {
		t.Fatal("a turn that ended in a focused pane was marked done")
	}
	// Blurred again: marked.
	b, _ := syncAttn(busy, busy, tea.BlurMsg{})
	e := b
	e.busy = false
	if s, _ := syncAttn(b, e, nil); !s.attnDone {
		t.Fatal("a turn that ended after a blur was not marked done")
	}
}

func rawOf(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		return ""
	}
	r, ok := cmd().(tea.RawMsg)
	if !ok {
		t.Fatalf("cmd is not a raw write: %T", cmd())
	}
	return r.Msg.(string)
}

// A question rings the bell once; the same question again does not;
// DUN_BELL=off does not ring.
func TestAttnBell(t *testing.T) {
	prev := tuiModel{busy: true, attnState: attnWorking}
	ask := prev
	ask.asking = true
	m, cmd := syncAttn(prev, ask, nil)
	if got := rawOf(t, cmd); got != "\a" {
		t.Fatalf("question: wrote %q, want a bell", got)
	}
	if _, cmd := syncAttn(m, m, tea.KeyPressMsg{Code: 'j'}); cmd != nil {
		t.Fatal("the same question rang again")
	}
	// Not logged in as dun opens: the tab shows the question, no bell. A
	// login asked for after a key or a turn rings.
	login := tuiModel{convo: []convoEntry{{needLogin: true}}}
	m, cmd = syncAttn(tuiModel{}, login, nil)
	if m.attnState != attnQuestion || strings.Contains(rawOf(t, cmd), "\a") {
		t.Fatalf("login at launch: state %q, wrote %q", m.attnState, rawOf(t, cmd))
	}
	typed, _ := syncAttn(tuiModel{}, tuiModel{}, tea.KeyPressMsg{Code: 'j'})
	later := typed
	later.convo = login.convo
	if _, cmd := syncAttn(typed, later, nil); rawOf(t, cmd) != "\a" {
		t.Fatalf("login after a key: wrote %q, want a bell", rawOf(t, cmd))
	}
	t.Setenv("DUN_BELL", "off")
	if _, cmd := syncAttn(prev, ask, nil); cmd != nil {
		t.Fatal("DUN_BELL=off rang")
	}
}

type recTmux struct {
	mu      sync.Mutex
	calls   [][]string
	gate    chan struct{} // when set, each call waits on it
	entered chan struct{} // when set, each call says it began
}

func (f *recTmux) run(_ context.Context, args ...string) error {
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()
	return nil
}

func (f *recTmux) snapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func tmuxEnv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestTmuxOutsideDoesNothing(t *testing.T) {
	f := &recTmux{}
	for _, env := range []map[string]string{{}, {"TMUX": "/tmp/x,1,0"}, {"TMUX_PANE": "%1"}} {
		s := newTmuxSync(tmuxEnv(env), f.run)
		if s != nil {
			t.Fatalf("env %v: a tmux sync outside tmux", env)
		}
		s.show(attnWorking) // nil-safe
		s.close()
	}
	m, _ := syncAttn(tuiModel{busy: true}, tuiModel{}, nil)
	_ = m
	if len(f.snapshot()) != 0 {
		t.Fatalf("tmux called outside tmux: %v", f.snapshot())
	}
}

// A burst of states while a call is in flight ends in two calls: the one in
// flight and the latest state, never the ones between.
func TestTmuxCoalesces(t *testing.T) {
	f := &recTmux{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	s := newTmuxSync(tmuxEnv(map[string]string{"TMUX": "/tmp/x,1,0", "TMUX_PANE": "%7"}), f.run)
	s.gap = 10 * time.Millisecond
	s.show(attnWorking)
	<-f.entered // the worker is in the first call
	for _, st := range []string{attnIdle, attnWorking, attnQuestion, attnWorking, attnDone} {
		s.show(st)
	}
	close(f.gate)
	deadline := time.Now().Add(2 * time.Second)
	for len(f.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // no third call follows
	calls := f.snapshot()
	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2: %v", len(calls), calls)
	}
	want0 := []string{"set-option", "-w", "-t", "%7", "@dun_icon", "⚙", ";", "set-option", "-w", "-t", "%7", "@dun_state", "working"}
	if !slices.Equal(calls[0], want0) {
		t.Fatalf("first call %v, want %v", calls[0], want0)
	}
	if got := calls[1]; got[5] != "✓" || got[len(got)-1] != "done" {
		t.Fatalf("second call %v, want the latest state (done)", got)
	}
	s.close()
	calls = f.snapshot()
	last := calls[len(calls)-1]
	wantU := []string{"set-option", "-wu", "-t", "%7", "@dun_icon", ";", "set-option", "-wu", "-t", "%7", "@dun_state"}
	if !slices.Equal(last, wantU) {
		t.Fatalf("close: %v, want %v", last, wantU)
	}
	s.close() // twice is fine
	if len(f.snapshot()) != len(calls) {
		t.Fatal("a second close called tmux again")
	}
}

func TestTmuxIdleUnsetsIcon(t *testing.T) {
	f := &recTmux{}
	s := newTmuxSync(tmuxEnv(map[string]string{"TMUX": "x", "TMUX_PANE": "%2"}), f.run)
	defer s.close()
	s.show(attnIdle)
	deadline := time.Now().Add(2 * time.Second)
	for len(f.snapshot()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := f.snapshot()[0]
	want := []string{"set-option", "-wu", "-t", "%2", "@dun_icon", ";", "set-option", "-w", "-t", "%2", "@dun_state", "idle"}
	if !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

func TestTmuxCmd(t *testing.T) {
	q := func(formats map[string]string) tmuxQuery {
		return func(args ...string) (string, error) { return formats[args[len(args)-1]], nil }
	}
	var b bytes.Buffer
	tmuxCmd(&b, true, false, q(map[string]string{"window-status-format": "#I #W", "window-status-current-format": "#[bold]#I #W"}))
	out := b.String()
	for _, want := range []string{
		"set -g window-status-format '#{?@dun_icon,#{@dun_icon} ,}#I #W'",
		"set -g window-status-current-format '#{?@dun_icon,#{@dun_icon} ,}#[bold]#I #W'",
		"does not show the icon yet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "allow-passthrough") {
		t.Error("passthrough line outside iTerm2")
	}
	b.Reset()
	both := tmuxIconPrefix + tmuxDefaultFormat
	tmuxCmd(&b, true, true, q(map[string]string{"window-status-format": both, "window-status-current-format": both}))
	if !strings.Contains(b.String(), "shows the icon already") || !strings.Contains(b.String(), "set -g allow-passthrough on") {
		t.Errorf("configured, in iTerm2:\n%s", b.String())
	}
	b.Reset()
	tmuxCmd(&b, false, false, func(...string) (string, error) { t.Fatal("queried tmux outside tmux"); return "", nil })
	if !strings.Contains(b.String(), "not inside tmux") || !strings.Contains(b.String(), "'"+tmuxIconPrefix+tmuxDefaultFormat+"'") {
		t.Errorf("outside tmux:\n%s", b.String())
	}
}

func TestWebEnvDropsTerminal(t *testing.T) {
	got := webEnv([]string{"PATH=/bin", "TMUX=/tmp/s,1,0", "TMUX_PANE=%1", "TERM_PROGRAM=iTerm.app", "LC_TERMINAL=iTerm2", "HOME=/h"})
	if !slices.Equal(got, []string{"PATH=/bin", "HOME=/h"}) {
		t.Fatalf("%v", got)
	}
}

// A failed turn shows "!" and rings; a key, or the next turn, clears it
// (USER 2026-10-27).
func TestAttnErrorShowsAndClears(t *testing.T) {
	if attnIcon(attnError) != "!" {
		t.Fatalf("icon %q", attnIcon(attnError))
	}
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	m.busy = true
	prev := m
	m = m.handleEvent(evMsg{"type": "error", "error": "provider: 502 bad gateway"})
	m, _ = syncAttn(prev, m, nil)
	if m.attnState != attnError {
		t.Fatalf("after a failed turn: %q", m.attnState)
	}
	prev = m
	m, _ = syncAttn(prev, m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.attnErr || m.attnState == attnError {
		t.Errorf("a key did not clear it: %q", m.attnState)
	}
}
