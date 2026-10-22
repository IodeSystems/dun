package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

var (
	kCtrlK = tea.KeyMsg{Type: tea.KeyCtrlK}
	kCtrlG = tea.KeyMsg{Type: tea.KeyCtrlG}
)

// busyVtui is a session with a turn running, and a record of what the TUI
// writes to the engine.
func busyVtui(t *testing.T) (*vtui, *bytes.Buffer) {
	t.Helper()
	v := newVtui(80, 24)
	rec := &bytes.Buffer{}
	v.m.proc = &dunProc{stdin: bufCloser{rec}}
	v.event(map[string]any{"type": "ready", "tools": []any{"exec", "ask_user"}})
	v.event(map[string]any{"type": "user", "content": "start"})
	v.event(map[string]any{"type": "turn", "kind": "user"})
	v.event(map[string]any{"type": "reasoning", "text": "Let me think about this. It is going badly"})
	if !v.m.busy {
		t.Fatal("precondition: a turn is running")
	}
	rec.Reset()
	return v, rec
}

func sentInterrupt(rec *bytes.Buffer) bool {
	return strings.Contains(rec.String(), `"type":"interrupt"`)
}

func TestInterrupt_CtrlGWhileBusy(t *testing.T) {
	v, rec := busyVtui(t)
	v.m = key(v.m, kCtrlG)
	if !sentInterrupt(rec) {
		t.Fatalf("ctrl+g while busy sent %q, want an interrupt", rec.String())
	}
	if !strings.Contains(v.view(), "interrupting…") {
		t.Errorf("the status line should say interrupting:\n%s", v.view())
	}
	rec.Reset()
	v.m = key(v.m, kCtrlG)
	if sentInterrupt(rec) {
		t.Error("a second ctrl+g before the turn ends sent another interrupt")
	}

	v.event(map[string]any{"type": "done", "reason": "interrupted"})
	if v.m.busy || v.m.interrupting {
		t.Fatalf("busy=%v interrupting=%v after done{interrupted}", v.m.busy, v.m.interrupting)
	}
	c := v.convo()
	if !strings.Contains(c, interruptedRow) {
		t.Errorf("no interrupted row:\n%s", c)
	}
	if !strings.Contains(c, "interrupted after") {
		t.Errorf("the thought should fold as interrupted:\n%s", c)
	}
}

// ctrl+k is the composer's kill-to-end-of-line even while a turn runs: it
// used to be the interrupt key, which took the edit away mid-turn.
func TestInterrupt_CtrlKBusyStillKillsLine(t *testing.T) {
	v, rec := busyVtui(t)
	v.m = typeStr(v.m, "hello world")
	for range 6 {
		v.m = key(v.m, tea.KeyMsg{Type: tea.KeyLeft})
	}
	v.m = key(v.m, kCtrlK)
	if sentInterrupt(rec) {
		t.Fatal("ctrl+k while busy sent an interrupt")
	}
	if got := v.m.input.Value(); got != "hello" {
		t.Errorf("input %q, want %q", got, "hello")
	}
}

// Idle, ctrl+k is the composer's kill-to-end-of-line, as before.
func TestInterrupt_CtrlKIdleKillsLine(t *testing.T) {
	v := newVtui(80, 24)
	rec := &bytes.Buffer{}
	v.m.proc = &dunProc{stdin: bufCloser{rec}}
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.m = typeStr(v.m, "hello world")
	for range 6 {
		v.m = key(v.m, tea.KeyMsg{Type: tea.KeyLeft})
	}
	v.m = key(v.m, kCtrlK)
	if sentInterrupt(rec) {
		t.Fatal("ctrl+k when idle sent an interrupt")
	}
	if got := v.m.input.Value(); got != "hello" {
		t.Errorf("input %q, want %q", got, "hello")
	}
}

func TestInterrupt_EscWhileBusy(t *testing.T) {
	v, rec := busyVtui(t)
	v.m = key(v.m, kEsc)
	if !sentInterrupt(rec) {
		t.Fatal("esc while busy, with nothing else open, should interrupt")
	}
}

// Idle, esc still only says how to leave.
func TestInterrupt_EscIdleOnlyFlashes(t *testing.T) {
	v := newVtui(80, 24)
	rec := &bytes.Buffer{}
	v.m.proc = &dunProc{stdin: bufCloser{rec}}
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.m = key(v.m, kEsc)
	if sentInterrupt(rec) || v.m.quitting {
		t.Fatalf("esc idle: sent %q quitting=%v", rec.String(), v.m.quitting)
	}
	if !strings.Contains(v.m.flashText, "to exit: ctrl+c or /exit") {
		t.Errorf("flash %q", v.m.flashText)
	}
}

// Whatever esc closes first wins over the interrupt: the palette, the
// search, the ask picker, agent scope.
func TestInterrupt_EscPrecedence(t *testing.T) {
	t.Run("palette", func(t *testing.T) {
		v, rec := busyVtui(t)
		v.m.engineCmds = []engineCmd{{name: "interrupt"}}
		v.m = typeStr(v.m, "/int")
		if !v.m.paletteActive() {
			t.Fatal("precondition: the palette is open")
		}
		v.m = key(v.m, kEsc)
		if sentInterrupt(rec) || v.m.paletteActive() {
			t.Fatalf("esc in the palette: interrupt=%v palette=%v", sentInterrupt(rec), v.m.paletteActive())
		}
	})
	t.Run("search", func(t *testing.T) {
		v, rec := busyVtui(t)
		v.m = key(v.m, kTab) // the conversation, where / searches
		for v.m.focus != focusConvo {
			v.m = key(v.m, kTab)
		}
		v.m = key(v.m, kSlash)
		if !v.m.searching {
			t.Fatal("precondition: searching")
		}
		v.m = key(v.m, kEsc)
		if sentInterrupt(rec) || v.m.searching {
			t.Fatalf("esc in search: interrupt=%v searching=%v", sentInterrupt(rec), v.m.searching)
		}
	})
	t.Run("ask", func(t *testing.T) {
		v, rec := busyVtui(t)
		v.event(map[string]any{"type": "ask", "question": "Which?", "options": []any{"A", "B"}})
		v.m = key(v.m, kEsc)
		if sentInterrupt(rec) || !v.m.askParked {
			t.Fatalf("esc in the picker: interrupt=%v parked=%v", sentInterrupt(rec), v.m.askParked)
		}
		// Parked, the next esc has nothing else to close: it interrupts.
		v.m = key(v.m, kEsc)
		if !sentInterrupt(rec) {
			t.Fatal("esc after parking should interrupt the turn")
		}
		v.event(map[string]any{"type": "done", "reason": "interrupted"})
		if v.m.askParked || v.m.asking {
			t.Error("the cut turn's question should be closed")
		}
	})
	t.Run("agent scope", func(t *testing.T) {
		v, rec := busyVtui(t)
		v.m.scopeAgent = 1
		v.m = key(v.m, kEsc)
		if sentInterrupt(rec) || v.m.scopeAgent != 0 {
			t.Fatalf("esc in agent scope: interrupt=%v scope=%d", sentInterrupt(rec), v.m.scopeAgent)
		}
	})
}

// Busy, the first ctrl+c interrupts the turn and the second quits.
func TestInterrupt_CtrlCInterruptsThenQuits(t *testing.T) {
	v, rec := busyVtui(t)
	nm, _ := v.m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	v.m = nm.(tuiModel)
	if !sentInterrupt(rec) || v.m.quitting {
		t.Fatalf("first ctrl+c: interrupt=%v quitting=%v", sentInterrupt(rec), v.m.quitting)
	}
	nm, cmd := v.m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !nm.(tuiModel).quitting || cmd == nil {
		t.Fatal("a second ctrl+c should quit the TUI")
	}
}

// After quitAgain, a ctrl+c on a still-running turn interrupts again rather
// than quitting.
func TestInterrupt_CtrlCLateSecondDoesNotQuit(t *testing.T) {
	v, _ := busyVtui(t)
	v.m.ctrlCAt = time.Now().Add(-2 * quitAgain)
	nm, _ := v.m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if nm.(tuiModel).quitting {
		t.Fatal("a ctrl+c long after the last one should not quit")
	}
}

// Idle, ctrl+c quits at once.
func TestInterrupt_CtrlCIdleQuits(t *testing.T) {
	v := newVtui(80, 24)
	rec := &bytes.Buffer{}
	v.m.proc = &dunProc{stdin: bufCloser{rec}}
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	nm, cmd := v.m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if sentInterrupt(rec) || !nm.(tuiModel).quitting || cmd == nil {
		t.Fatalf("idle ctrl+c: interrupt=%v quitting=%v", sentInterrupt(rec), nm.(tuiModel).quitting)
	}
}

// The busy status line says what each key does.
func TestInterrupt_BusyHint(t *testing.T) {
	v, _ := busyVtui(t)
	if s := v.view(); !strings.Contains(s, "ctrl+c or ctrl+g interrupt · ctrl+c twice quits") {
		t.Errorf("busy status:\n%s", s)
	}
}

// An open run is marked, and an unfinished call in it fails.
func TestInterrupt_MarksOpenRun(t *testing.T) {
	v, _ := busyVtui(t)
	v.event(map[string]any{"type": "tool_call", "tool": "exec", "args": map[string]any{"command": "sleep 100"}})
	v.event(map[string]any{"type": "done", "reason": "interrupted"})
	c := v.convo()
	if !strings.Contains(c, "⏹ interrupted") || !strings.Contains(c, "1 failed") {
		t.Errorf("the run should say interrupted and fail the cut call:\n%s", c)
	}
}

// The interrupt adds rows; it never removes one.
func TestNoShrink_Interrupt(t *testing.T) {
	c := newShrinkCheck(t, 80, 24)
	for round := 0; round < 4; round++ {
		for k := 0; k < 30; k++ {
			c.ev(map[string]any{"type": "reasoning", "text": "a long and winding thought that goes nowhere. "})
		}
		c.ev(map[string]any{"type": "tool_call", "tool": "exec", "args": map[string]any{"command": "make"}})
		if round%2 == 0 {
			c.ev(map[string]any{"type": "token", "text": "Partial answer, cut"})
		}
		c.ev(map[string]any{"type": "done", "reason": "interrupted"})
		c.ev(map[string]any{"type": "user", "content": "try again"})
		c.ev(map[string]any{"type": "turn"})
	}
}
