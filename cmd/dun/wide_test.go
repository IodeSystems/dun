package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The answer every test here reads: prose, a code block with a 150-column
// line, prose, and a table wider than the prose column.
var wideAnswer = "Here is the call.\n\n```go\nfunc main() { fmt.Println(\"" + strings.Repeat("x", 120) + "\") } // END\n" +
	"return\n```\n\nAnd the table:\n\n| name | what it does |\n|---|---|\n| poly-lsp | " + strings.Repeat("code navigation ", 7) + "TAIL |\n"

func TestSplitWide(t *testing.T) {
	segs := splitWide(wideAnswer)
	var kinds []string
	for _, s := range segs {
		switch {
		case s.table:
			kinds = append(kinds, "table")
		case s.wide:
			kinds = append(kinds, "code")
		default:
			kinds = append(kinds, "prose")
		}
	}
	if got := strings.Join(kinds, ","); got != "prose,code,prose,table,prose" {
		t.Errorf("segments %s", got)
	}
	if segs[1].need < 150 || segs[3].need < 120 {
		t.Errorf("needs: code %d table %d", segs[1].need, segs[3].need)
	}
	// A reply still streaming: the open fence runs to the end.
	if s := splitWide("a\n\n```\nopen line"); len(s) != 2 || !s[1].wide {
		t.Errorf("unclosed fence: %+v", s)
	}
	// Joined back, the parts are the source.
	var b []string
	for _, s := range segs {
		b = append(b, s.text)
	}
	if strings.Join(b, "\n") != wideAnswer {
		t.Error("the segments do not join back to the source")
	}
}

// wideConvo is a session at w×h with the answer finished.
func wideConvo(t *testing.T, w, h int) *vtui {
	t.Helper()
	v := newVtui(w, h)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(1)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.event(map[string]any{"type": "settings", "model": "m", "url": "http://llm/v1"})
	v.send("show me")
	v.event(map[string]any{"type": "token", "text": wideAnswer})
	v.event(map[string]any{"type": "done"})
	return v
}

func answerIndex(v *vtui) int {
	for i, e := range v.m.convo {
		if e.mdSource != "" {
			return i
		}
	}
	return -1
}

// At 100 columns the code line and the table rows are not wrapped: each is
// one row, cut at the pane with what is left said at the edge. Prose still
// wraps at the prose column.
func TestWide_CutNotWrapped(t *testing.T) {
	for _, w := range []int{49, 100} {
		v := wideConvo(t, w, 40)
		view := v.view()
		rows := strings.Split(view, "\n")
		code, table := -1, -1
		for i, r := range rows {
			if strings.Contains(r, "func main()") {
				code = i
			}
			if strings.Contains(r, "poly-lsp") {
				table = i
			}
			if ansi.StringWidth(r) > w {
				t.Errorf("%d: row wider than the pane: %q", w, r)
			}
		}
		if code < 0 || table < 0 {
			t.Fatalf("%d: no code or table row:\n%s", w, view)
		}
		if !strings.Contains(rows[code], " cols →") || !strings.HasPrefix(strings.TrimSpace(rows[code+1]), "return") {
			t.Errorf("%d: the code line wrapped or has no edge note:\n%s\n%s", w, rows[code], rows[code+1])
		}
		if !strings.Contains(rows[table], " cols →") {
			t.Errorf("%d: the table row has no edge note: %q", w, rows[table])
		}
		if strings.Contains(view, "END") || strings.Contains(view, "TAIL") {
			t.Errorf("%d: the cut part is on screen:\n%s", w, view)
		}
	}
}

// At 180 the 150-column line fits: no cut, no note — it is exempt from the
// prose column, which still wraps prose at 100.
func TestWide_FitsOnAWideScreen(t *testing.T) {
	v := wideConvo(t, 180, 40)
	view := v.view()
	if !strings.Contains(view, "// END") || strings.Contains(view, "cols →") {
		t.Errorf("at 180:\n%s", view)
	}
	e := v.m.convo[answerIndex(v)]
	if e.wide != nil && e.wide.max > 0 {
		t.Errorf("nothing is cut, yet it scrolls: %+v", e.wide)
	}
}

// → on the selected answer descends; then →/← scroll it sideways, rows
// changing in place (no row moves, the view does not move); ← at the left
// edge comes back out, and esc does too.
func TestWide_ScrollSideways(t *testing.T) {
	v := wideConvo(t, 100, 40)
	i := answerIndex(v)
	v.m = key(v.m, kTab) // focus the conversation
	for v.m.sel != i {
		before := v.m.sel
		v.m = key(v.m, kUp)
		if v.m.sel == before {
			t.Fatalf("cannot select the answer (sel %d, want %d)", v.m.sel, i)
		}
	}
	rows0 := strings.Split(v.view(), "\n")
	yoff := v.m.vp.YOffset
	if !strings.Contains(lastRow(v.m), "→ scroll sideways") {
		t.Errorf("status: %q", lastRow(v.m))
	}
	v.m = key(v.m, kRight)
	if v.m.insideWide() == nil {
		t.Fatal("→ did not descend")
	}
	for k := 0; k < 10; k++ {
		v.m = key(v.m, kRight)
	}
	rows := strings.Split(v.view(), "\n")
	if len(rows) != len(rows0) || v.m.vp.YOffset != yoff {
		t.Fatalf("the screen moved: %d rows → %d, offset %d → %d", len(rows0), len(rows), yoff, v.m.vp.YOffset)
	}
	view := v.view()
	if !strings.Contains(view, "// END") || !strings.Contains(view, "TAIL") {
		t.Errorf("scrolled to the end, the ends are not shown:\n%s", view)
	}
	for k := range rows {
		// Only rows of the wide blocks change; prose and the band stay.
		if strings.Contains(rows0[k], "Here is the call") && rows[k] != rows0[k] {
			t.Errorf("a prose row changed: %q → %q", rows0[k], rows[k])
		}
	}
	if !strings.Contains(lastRow(v.m), "sideways") {
		t.Errorf("status inside: %q", lastRow(v.m))
	}
	for k := 0; k < 12 && v.m.insideWide() != nil; k++ {
		v.m = key(v.m, kLeft)
	}
	if v.m.insideWide() != nil || v.m.convo[i].wide.off != 0 {
		t.Errorf("← did not come back to the edge and out: %+v", v.m.convo[i].wide)
	}
	if got := strings.Split(v.view(), "\n"); strings.Join(got, "\n") != strings.Join(rows0, "\n") {
		t.Error("back at the edge, the screen is not what it was")
	}
	v.m = key(v.m, kRight)
	v.m = key(v.m, kRight)
	v.m = key(v.m, kEsc)
	if v.m.insideWide() != nil || v.m.focus != focusConvo {
		t.Error("esc did not leave the block (and only the block)")
	}
}

// The streaming reply is drawn the way it will be when finished: the
// finish changes no row.
func TestWide_StreamedAsFinished(t *testing.T) {
	v := newVtui(100, 40)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(1)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.event(map[string]any{"type": "settings", "model": "m", "url": "http://llm/v1"})
	v.send("show me")
	v.event(map[string]any{"type": "token", "text": wideAnswer})
	v.m.refresh()
	during := v.view()
	v.event(map[string]any{"type": "done"})
	after := v.view()
	strip := func(s string) []string { // the conversation rows only
		rows := strings.Split(s, "\n")
		return rows[:len(rows)-3]
	}
	d, a := strip(during), strip(after)
	for k := range d {
		if k < len(a) && d[k] != a[k] && strings.TrimSpace(d[k]) != "" {
			t.Errorf("row %d changed at the finish:\n %q\n %q", k, d[k], a[k])
		}
	}
}
