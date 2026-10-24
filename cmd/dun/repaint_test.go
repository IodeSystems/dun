package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// fakeTmux stands in for tmux: capture-pane answers with screen, display with
// size. It returns the calls it saw.
func fakeTmux(t *testing.T, screen, size string) *[]string {
	t.Helper()
	var calls []string
	old := runTmux
	runTmux = func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "capture-pane":
			return []byte(screen), nil
		case "display":
			return []byte(size + "\n"), nil
		}
		return nil, fmt.Errorf("unexpected tmux %v", args)
	}
	oldSize := ttySize
	ttySize = func() (int, int, error) { return 80, 15, nil }
	t.Cleanup(func() { runTmux, ttySize = old, oldSize })
	return &calls
}

func onlyDir(t *testing.T, root string) string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(root, "bugreports"))
	if err != nil || len(ents) != 1 {
		t.Fatalf("want one capture dir, got %v (%v)", ents, err)
	}
	return filepath.Join(root, "bugreports", ents[0].Name())
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// In tmux the capture holds every file, the diff names the rows junk was
// written over, and state.json has both sizes.
func TestRepaintCaptureInTmux(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	t.Setenv("TMUX", "/tmp/tmux-1/default,1,0")
	t.Setenv("TMUX_PANE", "%7")
	v := tallConvo(t)
	frame := v.m.View().Content // what the renderer was handed last
	exp := v.m.expectedRows()
	act := append([]string(nil), exp...)
	act[2] = "JUNKJUNKJUNK"
	act[5] = "\x1b[31mJUNK\x1b[0m" + act[5]
	calls := fakeTmux(t, strings.Join(act, "\n")+"\n", "80x15")
	if frame == "" {
		t.Fatal("View returned nothing")
	}
	dir, err := v.m.capture(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("capture dir mode = %v (%v), want 0700", fi.Mode().Perm(), err)
	}
	for _, f := range []string{"expected.txt", "expected.ansi", "actual.txt", "actual.ansi", "diff.txt", "state.json", "events.jsonl", "README.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if !strings.Contains(strings.Join(*calls, "|"), "capture-pane -p -e -t %7") {
		t.Errorf("tmux calls = %v, want a capture-pane of $TMUX_PANE", *calls)
	}
	diff := readFile(t, filepath.Join(dir, "diff.txt"))
	if !strings.Contains(diff, "2 differ") || !strings.Contains(diff, "row 3\n") || !strings.Contains(diff, "row 6\n") {
		t.Errorf("diff should name rows 3 and 6:\n%s", diff)
	}
	if !strings.Contains(diff, "actual:   JUNKJUNKJUNK") {
		t.Errorf("diff should show the junk as actual:\n%s", diff)
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "actual.txt")), "\x1b") {
		t.Error("actual.txt must have no escape codes")
	}
	var st captureState
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "state.json"))), &st); err != nil {
		t.Fatal(err)
	}
	if st.Size.Dun != "80x15" || st.Size.Tmux != "80x15" || st.Size.TTY != "80x15" {
		t.Errorf("sizes = %+v", st.Size)
	}
	if st.Layout.Convo == 0 || st.Layout.Input == 0 || st.Env["TMUX_PANE"] != "%7" {
		t.Errorf("state = %+v", st)
	}
	if n := strings.Count(readFile(t, filepath.Join(dir, "events.jsonl")), "\n"); n == 0 {
		t.Error("events.jsonl is empty")
	}
}

// Outside tmux there is nothing to read back: said so, and no diff.
func TestRepaintCaptureWithoutTmux(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	t.Setenv("TMUX", "")
	fakeTmux(t, "", "")
	v := tallConvo(t)
	_ = v.m.View()
	dir, err := v.m.capture(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "actual.txt")); !strings.Contains(got, "not available: not in tmux") {
		t.Errorf("actual.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "diff.txt")); err == nil {
		t.Error("no actual, so no diff.txt")
	}
	if got := readFile(t, filepath.Join(dir, "expected.txt")); !strings.Contains(got, "ask dun to do something") {
		t.Errorf("expected.txt should hold the composer row:\n%s", got)
	}
}

// The expected screen is the last frame as the renderer cuts it: exactly h
// rows, each the frame's.
func TestRepaintExpectedIsTheFrame(t *testing.T) {
	v := tallConvo(t)
	frame := strings.Split(v.m.View().Content, "\n")
	exp := v.m.expectedRows()
	if len(exp) != v.m.h {
		t.Fatalf("expected %d rows, got %d", v.m.h, len(exp))
	}
	for i := range exp {
		if i < len(frame) && exp[i] != ansi.Truncate(frame[i], v.m.w, "") {
			t.Errorf("row %d = %q, want the frame's %q cut to the width", i, exp[i], frame[i])
		}
	}
}

// /repaint drops the hold, re-asserts the terminal's modes, and then says
// where the capture went.
func TestRepaintResetsAndReports(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	t.Setenv("TMUX", "")
	fakeTmux(t, "", "")
	v := tallConvo(t)
	v.event(map[string]any{"type": "token", "text": "more"})
	_ = v.m.View()
	v.m.convoHold, v.m.convoHoldW = 99, 80
	if c := v.m.runSlash("/repaint"); c == nil || fmt.Sprintf("%T", c()) != "main.repaintNowMsg" {
		t.Fatal("/repaint should wait for the frame to settle")
	}
	if _, err := os.Stat(filepath.Join(home, "bugreports")); err == nil {
		t.Fatal("nothing is captured before the settle")
	}
	nm, cmd := v.m.Update(repaintNowMsg{})
	v.m = nm.(tuiModel)
	if cmd == nil {
		t.Fatal("the settled /repaint returned no command")
	}
	if v.m.convoHold != 0 {
		t.Fatalf("after /repaint: hold=%d", v.m.convoHold)
	}
	nm, _ = v.m.Update(repaintDoneMsg{dir: onlyDir(t, home)})
	v.m = nm.(tuiModel)
	if last := stripANSI(v.m.convo[len(v.m.convo)-1].collapsed); !strings.HasPrefix(last, "repainted · capture saved to ") {
		t.Errorf("last line = %q", last)
	}
}

// The modes /repaint writes are the ones View declares, mouse as /mouse set it.
func TestRepaintModeReset(t *testing.T) {
	for mode, want := range map[string]string{"all": "\x1b[?1003h", "cell": "\x1b[?1002h", "off": ""} {
		got := modeReset(mode)
		if !strings.Contains(got, "\x1b[?2004h") || !strings.Contains(got, "\x1b[?25l") {
			t.Errorf("%s: no bracketed paste or hidden cursor: %q", mode, got)
		}
		if want != "" && !strings.Contains(got, want) {
			t.Errorf("%s: want %q in %q", mode, want, got)
		}
		if mode == "off" && strings.Contains(got, "\x1b[?100") {
			t.Errorf("off: a mouse mode was set: %q", got)
		}
	}
}

// The ring holds the last evlogSize messages, oldest first, and no text past
// evTextMax.
func TestEventRingBound(t *testing.T) {
	r := newEventRing()
	t0 := time.Now()
	for i := 0; i < evlogSize+50; i++ {
		r.add(t0.Add(time.Duration(i)*time.Millisecond), "tea.WindowSizeMsg", tea.WindowSizeMsg{Width: i, Height: 1})
	}
	recs := r.records()
	if len(recs) != evlogSize {
		t.Fatalf("held %d, want %d", len(recs), evlogSize)
	}
	if recs[0].info != "50x1" || recs[len(recs)-1].info != fmt.Sprintf("%dx1", evlogSize+49) {
		t.Errorf("oldest %q newest %q", recs[0].info, recs[len(recs)-1].info)
	}
	if n := strings.Count(r.jsonl(), "\n"); n != evlogSize {
		t.Errorf("jsonl has %d lines", n)
	}
	long := strings.Repeat("x", 500)
	r.add(t0, "ev:user", evMsg{"type": "user", "content": long})
	if got := r.records()[evlogSize-1].info; len([]rune(got)) > evTextMax+1 {
		t.Errorf("summary carries %d runes", len([]rune(got)))
	}
	r.add(t0, "tea.KeyPressMsg", runesKey([]rune("s")))
	if got := r.records()[evlogSize-1].info; got != "rune" {
		t.Errorf("a typed key is named, not spelled: %q", got)
	}
}

func TestDiffRows(t *testing.T) {
	got := diffRows([]string{"a", "b  ", "c"}, []string{"a", "b", "X", "extra"})
	for _, want := range []string{"3 and 4 rows, 2 differ", "row 3\n  expected: c\n  actual:   X", "row 4\n  expected: \n  actual:   extra"} {
		if !strings.Contains(got, want) {
			t.Errorf("diff missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "row 2") {
		t.Error("trailing blanks are not a difference")
	}
}

// A run of ticks is one record, so they cannot push everything else out.
func TestEventRingCollapsesTicks(t *testing.T) {
	r := newEventRing()
	t0 := time.Now()
	r.add(t0, "tea.KeyPressMsg", tea.KeyPressMsg{Code: tea.KeyEnter})
	for i := 0; i < 500; i++ {
		r.add(t0.Add(time.Duration(i)*time.Millisecond), "spinner.TickMsg", struct{}{})
	}
	r.add(t0, "tea.KeyPressMsg", tea.KeyPressMsg{Code: tea.KeyEnter})
	recs := r.records()
	if len(recs) != 3 || recs[1].n != 500 || recs[0].info != "enter" {
		t.Fatalf("records = %+v", recs)
	}
	if !strings.Contains(r.jsonl(), `"n":500`) {
		t.Errorf("jsonl lacks the count:\n%s", r.jsonl())
	}
}

// Engine events come wrapped in a procMsg; the ring records the event.
func TestEventRingLooksThroughProcMsg(t *testing.T) {
	r := newEventRing()
	r.add(time.Now(), "main.procMsg", procMsg{msg: evMsg{"type": "token", "text": "hello"}})
	r.add(time.Now(), "main.procMsg", procMsg{msg: evMsg{"type": "user", "content": "my message"}})
	got := r.jsonl()
	for _, want := range []string{`"kind":"ev:token","info":"5 bytes"`, `"kind":"ev:user","info":"my message"`} {
		if !strings.Contains(got, want) {
			t.Errorf("jsonl lacks %s:\n%s", want, got)
		}
	}
}

// tmux marks line-drawing cells with SO/SI; the text rows drop them.
func TestPlainRowDropsControls(t *testing.T) {
	if got := plainRow("\x0f\x1b[1m› /repaint\x1b[0m\x0e"); got != "› /repaint" {
		t.Errorf("plainRow = %q", got)
	}
}
