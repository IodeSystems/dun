package main

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// composerText mixes what broke the box: ASCII words, CJK, emoji (plain, with a
// variation selector, ZWJ sequences, flags), tabs, combining marks, newlines.
func composerText(rng *rand.Rand, n int) string {
	pieces := []string{
		"a", "b", "word", "x", " ", " ", " ", "日", "本語", "한", "😀", "👍🏽", "☺️",
		"👨‍👩‍👧", "🇺🇸", "\t", "\t", "é", "́", "ño", "\n", "ｆｕｌｌ",
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(pieces[rng.Intn(len(pieces))])
	}
	return b.String()
}

// Every row of the box fits it, whatever the text and wherever the cursor:
// a row one cell too wide wraps on the terminal and shifts the whole frame.
func TestMultilineInput_RowsFitTheBox(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 400; iter++ {
		in := newMultilineInput()
		in.width = 10 + rng.Intn(71)
		in.maxLines = 1000 // every row on screen
		in.SetValue(composerText(rng, 1+rng.Intn(60)))
		box := in.textWidth() + 2
		for c := 0; c <= len(in.buf); c++ {
			if !in.unitStart(c) {
				continue
			}
			in.cursor = c
			for i, row := range strings.Split(in.View(), "\n") {
				if w := ansi.StringWidth(row); w > box {
					t.Fatalf("width %d, cursor %d: row %d is %d cells, box %d\n%q\nbuffer %q",
						in.width, c, i, w, box, row, in.Value())
				}
			}
		}
	}
}

// The cursor's displayed cell maps back to the cursor, for every place it can
// stand — wide characters, tabs and clusters included.
func TestMultilineInput_CellRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for iter := 0; iter < 400; iter++ {
		in := newMultilineInput()
		in.width = 10 + rng.Intn(71)
		in.SetValue(composerText(rng, 1+rng.Intn(80)))
		for c := 0; c <= len(in.buf); c++ {
			if !in.unitStart(c) {
				continue
			}
			in.cursor = c
			row, col := in.cursorDisplayPos()
			if got := in.displayToOffset(row, col); got != c {
				t.Fatalf("width %d: offset %d → (%d,%d) → %d in %q", in.width, c, row, col, got, in.Value())
			}
		}
	}
}

// Wrapping cuts between units, never inside one: a wide character goes to the
// next row whole, and no text goes missing.
func TestMultilineInput_WideRuneMovesWhole(t *testing.T) {
	in := newMultilineInput()
	in.width = 4 // 3 cells of text per row
	in.SetValue("ab日本")
	segs := in.segments()
	if len(segs) != 2 || in.text(segs[0]) != "ab" || in.text(segs[1]) != "日" {
		var rows []string
		for _, s := range segs {
			rows = append(rows, in.text(s))
		}
		// "日本" is 4 cells; 3 fit per row, so 本 goes to a third row.
		if len(segs) != 3 || rows[0] != "ab" || rows[1] != "日" || rows[2] != "本" {
			t.Fatalf("rows %q", rows)
		}
	}
	rng := rand.New(rand.NewSource(3))
	for iter := 0; iter < 200; iter++ {
		in.width = 10 + rng.Intn(71)
		in.SetValue(composerText(rng, 60))
		var rebuilt strings.Builder
		for _, s := range in.segments() {
			if !in.unitStart(s.start) {
				t.Fatalf("a row starts inside a unit at %d: %q", s.start, in.Value())
			}
			rebuilt.WriteString(string(in.buf[s.start:s.next]))
		}
		if rebuilt.String() != string(in.buf) {
			t.Fatalf("wrapping lost text:\n want %q\n got  %q", string(in.buf), rebuilt.String())
		}
	}
}

// A tab draws as spaces to the next 4-column stop; the buffer keeps the tab.
func TestMultilineInput_TabsDrawToStops(t *testing.T) {
	in := newMultilineInput()
	in.width = 40
	in.blinkOn = false
	in.SetValue("ab\tc\n\tx")
	rows := strings.Split(in.View(), "\n")
	if got := strings.TrimRight(rows[0], " "); got != "› ab  c" {
		t.Errorf("row 0 %q", got)
	}
	if got := strings.TrimRight(rows[1], " "); got != "      x" {
		t.Errorf("row 1 %q", got)
	}
	if in.Value() != "ab\tc\n\tx" {
		t.Errorf("value %q", in.Value())
	}
	in.cursor = 3 // the c, after the tab
	if _, col := in.cursorDisplayPos(); col != 4 {
		t.Errorf("c at column %d, want 4", col)
	}
}

// The cursor steps over a cluster whole, and backspace takes it whole.
func TestMultilineInput_ClusterIsOneStep(t *testing.T) {
	in := newMultilineInput()
	in.SetValue("é👨‍👩‍👧")
	in.CursorEnd()
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyLeft})
	if in.cursor != 2 {
		t.Fatalf("left over the family emoji: cursor %d, want 2", in.cursor)
	}
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if in.Value() != "👨‍👩‍👧" || in.cursor != 0 {
		t.Fatalf("backspace over é: %q cursor %d", in.Value(), in.cursor)
	}
}

func bigText(lines int) string {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		b.WriteString("line of the pasted file\tcol\n")
	}
	return b.String()
}

func paste(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(s)}
}

// A big paste is one token in the box; Value is the full text.
func TestMultilineInput_BigPasteIsAToken(t *testing.T) {
	text := bigText(120)
	in := newMultilineInput()
	in.width = 80
	in.SetValue("see: ")
	in.CursorEnd()
	in = in.HandleKey(paste(text))
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("!")})

	if want := "see: " + text + "!"; in.Value() != want {
		t.Fatalf("Value lost the paste: %d chars, want %d", len(in.Value()), len(want))
	}
	v := in.View()
	if strings.Count(v, "\n") != 0 || !strings.Contains(v, "[pasted 120 lines · 3,360 chars]") {
		t.Fatalf("the box should show one labelled row:\n%s", v)
	}

	// Motions step over it as one unit.
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyLeft})
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyLeft})
	if in.cursor != 5 {
		t.Fatalf("two lefts from the end: cursor %d, want 5 (before the token)", in.cursor)
	}
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyRight})

	// Backspace at its end takes the whole paste.
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if in.Value() != "see: !" {
		t.Fatalf("backspace after the token: %q", in.Value())
	}
}

// ctrl+o opens the token for editing; the text is unchanged.
func TestMultilineInput_CtrlOExpandsPaste(t *testing.T) {
	text := bigText(30)
	in := newMultilineInput()
	in = in.HandleKey(paste(text))
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlO})
	if in.Value() != text || string(in.buf) != text {
		t.Fatalf("expand changed the text or left the token: buf %d runes", len(in.buf))
	}
	if in.cursor != len(in.buf) {
		t.Errorf("cursor %d, want the end of the expanded text %d", in.cursor, len(in.buf))
	}
	if in.height() != in.maxLines {
		t.Errorf("expanded text should fill the box, height %d", in.height())
	}
}

// A small paste stays inline, as typed text does.
func TestMultilineInput_SmallPasteInline(t *testing.T) {
	in := newMultilineInput()
	in = in.HandleKey(paste("one\ntwo\nthree"))
	if string(in.buf) != "one\ntwo\nthree" || len(in.pastes) != 0 {
		t.Fatalf("small paste: buf %q pastes %d", string(in.buf), len(in.pastes))
	}
}

// SetValue (history recall, draft restore) round-trips a big text, and shows
// it as a token too.
func TestMultilineInput_SetValueBigRoundTrips(t *testing.T) {
	text := strings.Repeat("{\"k\":1},", 3000) // one 24,000-char line
	in := newMultilineInput()
	in.SetValue(text)
	if in.Value() != text {
		t.Fatal("SetValue lost the text")
	}
	if len(in.buf) != 1 || !strings.Contains(in.View(), "[pasted 1 line · 24,000 chars]") {
		t.Fatalf("a big value should show as a token:\n%s", in.View())
	}
	in.Reset()
	if in.Value() != "" || len(in.pastes) != 0 {
		t.Fatal("Reset left the paste")
	}
}

// pasteVtui is a ready session recording what the TUI sends.
func pasteVtui(t *testing.T) (*vtui, *bytes.Buffer) {
	t.Helper()
	v := newVtui(80, 24)
	rec := &bytes.Buffer{}
	v.m.proc = &dunProc{stdin: bufCloser{rec}}
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	rec.Reset()
	return v, rec
}

func sentUser(rec *bytes.Buffer) []string {
	var out []string
	dec := json.NewDecoder(bytes.NewReader(rec.Bytes()))
	for {
		var m map[string]any
		if dec.Decode(&m) != nil {
			return out
		}
		if m["type"] == "user" {
			out = append(out, m["content"].(string))
		}
	}
}

// A bracketed paste with newlines in it never submits: only a real enter does.
func TestPasteWithNewlineDoesNotSubmit(t *testing.T) {
	v, rec := pasteVtui(t)
	v.m = key(v.m, paste("first line\nsecond line\n"))
	if got := sentUser(rec); len(got) != 0 {
		t.Fatalf("the paste submitted: %q", got)
	}
	if v.m.input.Value() != "first line\nsecond line\n" {
		t.Fatalf("input %q", v.m.input.Value())
	}
}

// The engine and the history get the full text of a paste token, and recall
// brings it back whole.
func TestBigPasteSendsFullText(t *testing.T) {
	v, rec := pasteVtui(t)
	text := bigText(200)
	v.m = typeStr(v.m, "review this: ")
	v.m = key(v.m, paste(text))
	v.m = key(v.m, kEnter)
	want := strings.TrimSpace("review this: " + text)
	if got := sentUser(rec); len(got) != 1 || got[0] != want {
		t.Fatalf("sent %d messages; first matches: %v", len(got), len(got) > 0 && got[0] == want)
	}
	if h := v.m.history; len(h) == 0 || h[len(h)-1] != want {
		t.Fatal("history does not hold the full text")
	}
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyCtrlUp})
	if v.m.input.Value() != want {
		t.Fatal("recall lost the text")
	}
}

// Mid-turn, a message is queued through the same send; it carries the full
// text too.
func TestBigPasteMidTurnSendsFullText(t *testing.T) {
	v, rec := busyVtui(t)
	text := bigText(50)
	v.m = key(v.m, paste(text))
	v.m = key(v.m, kEnter)
	if got := sentUser(rec); len(got) != 1 || got[0] != strings.TrimSpace(text) {
		t.Fatalf("mid-turn send: %d messages", len(got))
	}
}

// The draft other clients restore is the full text.
func TestBigPasteDraftRoundTrips(t *testing.T) {
	v, _ := pasteVtui(t)
	text := bigText(40)
	v.m = v.m.applyUI(map[string]any{"draft": text})
	if v.m.input.Value() != text {
		t.Fatal("draft restore lost the text")
	}
	if !strings.Contains(v.m.input.View(), "[pasted 40 lines") {
		t.Fatalf("a big draft should show as a token:\n%s", v.m.input.View())
	}
}

// A 50,000-char line, opened in the box, must not make each keystroke slow:
// the layout is cached between the dozen times a keystroke asks for it.
func TestMultilineInput_LongLineKeystrokeCost(t *testing.T) {
	in := newMultilineInput()
	in.width = 78
	in = in.HandleKey(paste(strings.Repeat("{\"key\":\"value\",\"n\":12345}", 2000)[:50000]))
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlO})
	if len(in.buf) != 50000 {
		t.Fatalf("precondition: expanded, %d runes", len(in.buf))
	}
	const keys = 20
	start := time.Now()
	for i := 0; i < keys; i++ {
		in = in.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
		_ = in.View()
	}
	per := time.Since(start) / keys
	t.Logf("keystroke + View on a 50,000-char line: %v", per)
	if per > 100*time.Millisecond {
		t.Errorf("a keystroke costs %v", per)
	}
}

func BenchmarkComposerLongLineKeystroke(b *testing.B) {
	in := newMultilineInput()
	in.width = 78
	in.buf = []rune(strings.Repeat("{\"key\":\"value\",\"n\":12345}", 2000)[:50000])
	in.CursorEnd()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		in = in.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
		_ = in.View()
	}
}

func BenchmarkComposerLongLineLayout(b *testing.B) {
	in := newMultilineInput()
	in.width = 78
	in.buf = []rune(strings.Repeat("{\"key\":\"value\",\"n\":12345}", 2000)[:50000])
	for i := 0; i < b.N; i++ {
		_ = in.computeLayout(in.wrapWidth())
	}
}
