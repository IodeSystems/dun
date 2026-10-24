package main

import (
	"encoding/json"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCast(t *testing.T, w, h int, evs ...[]any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.cast")
	hb, _ := json.Marshal(castHeader{Version: 2, Width: w, Height: h})
	lines := []string{string(hb)}
	for _, e := range evs {
		b, _ := json.Marshal(e)
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A marker writes a PNG of exactly cols×rows cells, with the colours the
// program asked for, and the screen's text with a wide character in it.
func TestRenderMarkPNG(t *testing.T) {
	cast := writeCast(t, 6, 2,
		[]any{0.0, "o", "\x1b[44m  \x1b[0m\x1b[38;2;255;0;0m█\x1b[0m界\r\n\x1b[1mok\x1b[0m"},
		[]any{0.1, "m", "First Screen"},
	)
	dir := t.TempDir()
	meta, err := render(cast, renderOpts{markDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Marks) != 1 || meta.Marks[0].PNG != "01-first-screen.png" {
		t.Fatalf("marks = %+v", meta.Marks)
	}
	f, err := os.Open(filepath.Join(dir, meta.Marks[0].PNG))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 6*meta.CellW || b.Dy() != 2*meta.CellH {
		t.Fatalf("size %v, want %dx%d", b, 6*meta.CellW, 2*meta.CellH)
	}
	// Cell 0 has a blue background (ANSI 4) everywhere, glyph-free.
	r, g, b, _ := img.At(meta.CellW/2, meta.CellH/2).RGBA()
	if !(b>>8 >= 0x80 && r>>8 < 0x40 && g>>8 < 0x40) {
		t.Errorf("cell 0 = %02x%02x%02x, want ANSI blue", r>>8, g>>8, b>>8)
	}
	// Cell 2 is a truecolour red full block: solid red at its centre.
	want := color.RGBA{0xff, 0, 0, 0xff}
	if got := color.RGBAModel.Convert(img.At(2*meta.CellW+meta.CellW/2, meta.CellH/2)); got != want {
		t.Errorf("cell 2 = %v, want %v", got, want)
	}
	txt, err := os.ReadFile(filepath.Join(dir, meta.Marks[0].TXT))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(txt), "  █界\nok") {
		t.Errorf("text:\n%s", txt)
	}
	if !strings.Contains(meta.Font, "px") {
		t.Errorf("font note %q", meta.Font)
	}
}

// A wide character covers two cells: its second cell is a placeholder the
// text skips and the next character lands after it.
func TestWideCharTakesTwoCells(t *testing.T) {
	tm := newTerm(6, 1, nil)
	_, _ = tm.Write([]byte("界x"))
	if c := tm.em.CellAt(0, 0); c == nil || c.Width != 2 {
		t.Fatalf("cell 0 = %+v", c)
	}
	if c := tm.em.CellAt(2, 0); c == nil || c.Content != "x" {
		t.Fatalf("cell 2 = %+v", c)
	}
	if got := tm.text(); got != "界x" {
		t.Errorf("text %q", got)
	}
}

// A GIF gets a frame per change, the long pause is cut, and an unchanged
// screen adds no frame.
func TestRenderGIF(t *testing.T) {
	cast := writeCast(t, 10, 2,
		[]any{0.0, "o", "one"},
		[]any{0.5, "o", "\r\ntwo"},
		[]any{20.0, "o", "\r\x1b[31mthree\x1b[0m"},
		[]any{20.5, "o", ""},
	)
	out := filepath.Join(t.TempDir(), "t.gif")
	meta, err := render(cast, renderOpts{gifPath: out})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g, err := gif.DecodeAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Image) != 3 {
		t.Fatalf("frames = %d, want 3", len(g.Image))
	}
	if g.Delay[1] > 150 {
		t.Errorf("the 19.5 s pause became %d cs, want ≤ 150", g.Delay[1])
	}
	if meta.Duration > 3 {
		t.Errorf("duration %v, want the idle cut", meta.Duration)
	}
}

func TestScreenLabels(t *testing.T) {
	got := screenLabels("models  type to search · ↑/↓ choose · enter switch · tab save as default\n  [ ] save as default (future sessions)\n  mode    (•) standalone   ( ) scripted\n/login to log in · saved to /tmp/x/y")
	for _, w := range []string{"[key] ↑/↓: choose", "[key] enter: switch", "[key] tab: save as default", "[checkbox] [ ] save as default (future sessions)", "[option] (•) standalone", "[command] /login"} {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in %q", w, got)
		}
	}
	for _, g := range got {
		if g == "[command] /tmp" {
			t.Errorf("a path read as a command: %q", got)
		}
	}
}

// rec drives a program from a script: waits for words, types, presses keys,
// and marks each step; the cast replays to the same screen.
func TestRecScript(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, "s.json")
	_ = os.WriteFile(sp, []byte(`{"cols": 40, "rows": 5, "typing_ms": 1, "steps": [
	  {"id": "asks", "settle_ms": 50, "actions": [{"wait_for": "name?"}]},
	  {"id": "answered", "settle_ms": 50, "actions": [{"type": "${TTYREC_T}"}, {"key": "enter"}, {"wait_for": "hi bob"}]}
	]}`), 0o644)
	t.Setenv("TTYREC_T", "bob")
	cast := filepath.Join(dir, "r.cast")
	res := filepath.Join(dir, "r.json")
	err := cmdRec([]string{"-script", sp, "-o", cast, "-result", res, "--", "sh", "-c", `printf 'name? '; read n; echo "hi $n"; sleep 5`})
	if err != nil {
		t.Fatal(err)
	}
	var rr recResult
	b, _ := os.ReadFile(res)
	if err := json.Unmarshal(b, &rr); err != nil || !rr.OK || len(rr.Steps) != 2 {
		t.Fatalf("result %s", b)
	}
	if d := rr.Steps[1].Did; len(d) != 2 || d[0] != `typed "bob"` || d[1] != "pressed Enter" {
		t.Errorf("did = %q", d)
	}
	meta, err := render(cast, renderOpts{markDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Marks) != 2 {
		t.Fatalf("marks %+v", meta.Marks)
	}
	txt, _ := os.ReadFile(filepath.Join(dir, "02-answered.txt"))
	if !strings.Contains(string(txt), "name? bob\nhi bob") {
		t.Errorf("replayed screen:\n%s", txt)
	}
}

// A failed wait still marks the step, so the picture shows where it stuck.
func TestRecWaitTimeout(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, "s.json")
	_ = os.WriteFile(sp, []byte(`{"cols": 20, "rows": 3, "steps": [
	  {"id": "never", "actions": [{"wait_for": "nope", "timeout_ms": 200}]}
	]}`), 0o644)
	cast := filepath.Join(dir, "r.cast")
	err := cmdRec([]string{"-script", sp, "-o", cast, "--", "sh", "-c", "echo here; sleep 5"})
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("err = %v", err)
	}
	f, _ := os.Open(cast)
	defer f.Close()
	_, evs, _ := readCast(f)
	marked := false
	for _, e := range evs {
		if e.Code == "m" && e.Data == "never" {
			marked = true
		}
	}
	if !marked {
		t.Error("no marker for the failed step")
	}
}
