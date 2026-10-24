package main

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// Bubble Tea v2's renderer is a cell diff with hard-scroll optimisation: when
// the conversation moves by a row it scrolls the terminal and writes only the
// row that came into view. dun used to do that itself with a scroll region
// (DECSTBM) beside v1's line renderer, and that was a bug farm: two owners of
// what the screen shows. These tests run the real program and renderer into
// a byte buffer, replay the bytes through a small terminal emulator, and
// compare the emulated screen with what View says it drew.

// syncBuf is the program's output: the renderer writes from its own goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

// quiet is the TUI without Init: no engine, no ticks — only what the test sends.
type quiet struct{ m tuiModel }

func (q quiet) Init() tea.Cmd { return nil }
func (q quiet) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ch, ok := msg.(peekMsg); ok { // on the program's goroutine, as View is
		ch <- snapshot{view: q.m.drawView(), pinned: q.m.scrollPinned}
		return q, nil
	}
	nm, _ := q.m.Update(msg) // commands dropped: no engine, no timers
	q.m = nm.(tuiModel)
	return q, nil
}
func (q quiet) View() tea.View { return q.m.View() }

// screenRun is a running program and the emulator its output feeds.
type screenRun struct {
	t    *testing.T
	p    *tea.Program
	out  *syncBuf
	emu  *emu
	fed  int // bytes of out already fed to emu
	done chan tea.Model
}

func startScreen(t *testing.T, m tuiModel, w, h int) *screenRun {
	t.Helper()
	r := &screenRun{t: t, out: &syncBuf{}, emu: newEmu(w, h), done: make(chan tea.Model, 1)}
	r.p = tea.NewProgram(quiet{m},
		tea.WithInput(nil), tea.WithOutput(r.out), tea.WithWindowSize(w, h),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
		tea.WithColorProfile(colorprofile.TrueColor),
		tea.WithoutSignalHandler())
	go func() {
		fm, err := r.p.Run()
		if err != nil {
			t.Errorf("run: %v", err)
		}
		r.done <- fm
	}()
	t.Cleanup(func() {
		r.p.Quit()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("program did not quit")
		}
	})
	return r
}

// settle waits for the renderer to go quiet, feeds what it wrote to the
// emulator, and returns those bytes.
func (r *screenRun) settle() []byte {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	last, stable := -1, 0
	for time.Now().Before(deadline) {
		n := len(r.out.bytes())
		if n == last && n > r.fed {
			if stable++; stable >= 5 {
				break
			}
		} else {
			stable = 0
		}
		last = n
		time.Sleep(10 * time.Millisecond)
	}
	b := r.out.bytes()[r.fed:]
	r.fed += len(b)
	r.emu.feed(r.t, b)
	return b
}

// send delivers a message and waits for the frame it causes.
func (r *screenRun) send(msg tea.Msg) []byte {
	r.p.Send(msg)
	return r.settle()
}

// expect checks the emulated screen against what View draws, row by row as
// plain text (trailing blanks ignored).
func (r *screenRun) expect(what string, s snapshot) {
	r.t.Helper()
	want := strings.Split(s.view, "\n")
	for i := 0; i < r.emu.h; i++ {
		exp := ""
		if i < len(want) {
			exp = strings.TrimRight(ansi.Strip(ansi.Truncate(want[i], r.emu.w, "")), " ")
		}
		if got := r.emu.row(i); got != exp {
			r.t.Errorf("%s: row %d\n  screen: %q\n  View:   %q", what, i, got, exp)
		}
	}
}

// snapshot is what the program's model draws, and whether it follows the tail.
type snapshot struct {
	view   string
	pinned bool
}

// peekMsg asks the program for a snapshot.
type peekMsg chan snapshot

// peek reads a snapshot back through a message round trip.
func (r *screenRun) peek() snapshot {
	ch := make(chan snapshot, 1)
	r.p.Send(peekMsg(ch))
	select {
	case s := <-ch:
		return s
	case <-time.After(5 * time.Second):
		r.t.Fatal("no answer from the program")
	}
	return snapshot{}
}

func TestScreen_ScrollingMatchesView(t *testing.T) {
	v := tallConvo(t)
	r := startScreen(t, v.m, v.w, v.h)
	full := r.settle()
	r.expect("first frame", r.peek())

	// Up from the composer's first row scrolls the conversation one row.
	up := r.send(tea.KeyPressMsg{Code: tea.KeyUp})
	s := r.peek()
	if s.pinned {
		t.Fatal("precondition: ↑ should have scrolled off the bottom")
	}
	r.expect("scrolled up one row", s)

	// A reply streaming in while pinned scrolls the other way.
	r.send(tea.KeyPressMsg{Code: tea.KeyDown})
	r.send(evMsg{"type": "token", "text": "one more line\nand another\n"})
	r.expect("streamed two rows", r.peek())

	// A taller composer moves every row above it.
	r.send(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	r.expect("composer grew a row", r.peek())

	t.Logf("first frame %d bytes, one-row scroll %d bytes", len(full), len(up))
	if len(up) > len(full)/3 {
		t.Errorf("a one-row scroll wrote %d bytes against %d for the whole screen: the renderer repainted instead of scrolling", len(up), len(full))
	}
}

// ── a terminal emulator, just big enough ───────────────────────────

// emu is a plain-text screen: cells of text, no attributes. It implements the
// sequences Bubble Tea's renderer writes, and fails the test on any CSI it
// does not know — an unknown sequence must not pass as a match.
type emu struct {
	w, h     int
	cells    [][]string
	x, y     int
	top, bot int // scroll margins, 0-based inclusive
	sx, sy   int // saved cursor
}

func newEmu(w, h int) *emu {
	e := &emu{w: w, h: h, top: 0, bot: h - 1}
	e.cells = make([][]string, h)
	for i := range e.cells {
		e.cells[i] = e.blank()
	}
	return e
}

func (e *emu) blank() []string {
	r := make([]string, e.w)
	for i := range r {
		r[i] = " "
	}
	return r
}

func (e *emu) row(i int) string {
	var b strings.Builder
	for _, c := range e.cells[i] {
		b.WriteString(c)
	}
	return strings.TrimRight(b.String(), " ")
}

func (e *emu) clamp() {
	e.x = min(max(e.x, 0), e.w-1)
	e.y = min(max(e.y, 0), e.h-1)
}

// scrollUp moves rows top+1..bot up n, blanking the bottom (SU, and LF at bot).
func (e *emu) scrollUp(top, bot, n int) {
	for ; n > 0; n-- {
		copy(e.cells[top:bot], e.cells[top+1:bot+1])
		e.cells[bot] = e.blank()
	}
}

// scrollDown moves rows top..bot-1 down n, blanking the top (SD, RI at top).
func (e *emu) scrollDown(top, bot, n int) {
	for ; n > 0; n-- {
		copy(e.cells[top+1:bot+1], e.cells[top:bot])
		e.cells[top] = e.blank()
	}
}

func (e *emu) lf() {
	if e.y == e.bot {
		e.scrollUp(e.top, e.bot, 1)
	} else if e.y < e.h-1 {
		e.y++
	}
}

func (e *emu) put(g string, width int) {
	if width <= 0 {
		return
	}
	if e.x+width > e.w {
		e.x = 0
		e.lf()
	}
	e.cells[e.y][e.x] = g
	for i := 1; i < width && e.x+i < e.w; i++ {
		e.cells[e.y][e.x+i] = ""
	}
	e.x += width // may reach e.w: a pending wrap, taken by the next put
}

func (e *emu) feed(t *testing.T, b []byte) {
	t.Helper()
	p := ansi.NewParser()
	state := byte(0)
	for len(b) > 0 {
		seq, width, n, newState := ansi.DecodeSequence(b, state, p)
		state = newState
		b = b[n:]
		switch {
		case width > 0:
			e.put(string(seq), width)
		case len(seq) == 1:
			switch seq[0] {
			case '\r':
				e.x = 0
			case '\n':
				// Without a tty to put in raw mode the renderer leaves the output
				// cooked (ONLCR): a newline also returns the carriage.
				e.x = 0
				e.lf()
			case '\b':
				e.x = max(0, min(e.x, e.w-1)-1)
			case '\t':
				e.x = min(e.w-1, (e.x/8+1)*8)
			}
		case ansi.HasCsiPrefix(seq):
			e.csi(t, p)
		case ansi.HasEscPrefix(seq) && len(seq) == 2:
			switch seq[1] {
			case 'M': // RI
				if e.y == e.top {
					e.scrollDown(e.top, e.bot, 1)
				} else if e.y > 0 {
					e.y--
				}
			case 'D':
				e.lf()
			case 'E':
				e.x = 0
				e.lf()
			case '7':
				e.sx, e.sy = e.x, e.y
			case '8':
				e.x, e.y = e.sx, e.sy
			}
		}
	}
}

func (e *emu) csi(t *testing.T, p *ansi.Parser) {
	t.Helper()
	cmd := ansi.Cmd(p.Command())
	param := func(i, def int) int {
		v, ok := p.Param(i, def)
		if !ok || v == 0 {
			return def
		}
		return v
	}
	if cmd.Prefix() != 0 { // private modes (?h/?l), kitty keyboard (>u <u), …
		return
	}
	if e.x >= e.w {
		e.x = e.w - 1
	}
	switch cmd.Final() {
	case 'm', 'h', 'l', 'n', 'q', 't', 'c', 'u':
		// attributes and modes: nothing on a plain-text screen
	case 'A':
		e.y = max(e.y-param(0, 1), 0)
	case 'B':
		e.y = min(e.y+param(0, 1), e.h-1)
	case 'C':
		e.x = min(e.x+param(0, 1), e.w-1)
	case 'D':
		e.x = max(e.x-param(0, 1), 0)
	case 'E':
		e.x, e.y = 0, min(e.y+param(0, 1), e.h-1)
	case 'F':
		e.x, e.y = 0, max(e.y-param(0, 1), 0)
	case 'G', '`':
		e.x = param(0, 1) - 1
	case 'd':
		e.y = param(0, 1) - 1
	case 'H', 'f':
		e.y, e.x = param(0, 1)-1, param(1, 1)-1
	case 'J':
		switch param(0, 0) {
		case 0:
			for i := e.x; i < e.w; i++ {
				e.cells[e.y][i] = " "
			}
			for y := e.y + 1; y < e.h; y++ {
				e.cells[y] = e.blank()
			}
		case 1:
			for y := 0; y < e.y; y++ {
				e.cells[y] = e.blank()
			}
			for i := 0; i <= e.x && i < e.w; i++ {
				e.cells[e.y][i] = " "
			}
		case 2, 3:
			for y := range e.cells {
				e.cells[y] = e.blank()
			}
		}
	case 'K':
		lo, hi := e.x, e.w
		switch param(0, 0) {
		case 1:
			lo, hi = 0, e.x+1
		case 2:
			lo, hi = 0, e.w
		}
		for i := lo; i < hi && i < e.w; i++ {
			e.cells[e.y][i] = " "
		}
	case 'X':
		for i := e.x; i < e.x+param(0, 1) && i < e.w; i++ {
			e.cells[e.y][i] = " "
		}
	case '@':
		n := param(0, 1)
		row := e.cells[e.y]
		copy(row[min(e.x+n, e.w):], row[e.x:])
		for i := e.x; i < e.x+n && i < e.w; i++ {
			row[i] = " "
		}
	case 'P':
		n := param(0, 1)
		row := e.cells[e.y]
		copy(row[e.x:], row[min(e.x+n, e.w):])
		for i := max(e.w-n, e.x); i < e.w; i++ {
			row[i] = " "
		}
	case 'L':
		if e.y >= e.top && e.y <= e.bot {
			e.scrollDown(e.y, e.bot, param(0, 1))
		}
	case 'M':
		if e.y >= e.top && e.y <= e.bot {
			e.scrollUp(e.y, e.bot, param(0, 1))
		}
	case 'S':
		e.scrollUp(e.top, e.bot, param(0, 1))
	case 'T':
		e.scrollDown(e.top, e.bot, param(0, 1))
	case 'r':
		e.top, e.bot = param(0, 1)-1, param(1, e.h)-1
		e.x, e.y = 0, 0
	case 'b': // REP: repeat the previous character
		prev := " "
		if e.x > 0 {
			prev = e.cells[e.y][e.x-1]
		}
		for i := 0; i < param(0, 1); i++ {
			e.put(prev, 1)
		}
	default:
		t.Fatalf("emulator: unhandled CSI %q (params %v)", string(rune(cmd.Final())), fmt.Sprint(p.Params()))
	}
	e.clamp()
}
