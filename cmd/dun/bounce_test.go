package main

import (
	"fmt"
	"strings"
	"testing"
)

// bounce_test.go: nothing on screen moves when something updates. The rule
// (owner, 2026-10-23): on a change that adds rows the view keeps its top row
// (and a pinned view follows the bottom, as always); on a change that takes
// rows away the view does not snap back either — the top row stays and blank
// rows fill in below, but only to the bottom of the view as it was, so the
// furthest scroll is where the view already is. New rows take the blank rows
// up again.

// shown is the pane's visible rows, without styling.
func shown(v *vtui) []string {
	vis := v.m.vp.visible()
	out := make([]string, len(vis))
	for i, l := range vis {
		out[i] = strings.TrimRight(stripANSI(l), " ")
	}
	return out
}

// splashConvo is a fresh, logged-in session with its splash and enough
// conversation under it to scroll.
func splashConvo(t *testing.T) *vtui {
	t.Helper()
	v := newVtui(100, 20)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}, "servers": []any{
		map[string]any{"id": "shell", "running": true, "tools": float64(1)},
		map[string]any{"id": "raglit", "slash": "raglit"},
		map[string]any{"id": "poly-lsp", "slash": "poly-lsp"},
	}})
	v.event(map[string]any{"type": "settings", "model": "chat-standard", "url": "http://llm/v1", "provider": "work"})
	if len(v.m.convo) == 0 || !v.m.convo[0].liveSplash {
		t.Fatalf("no live splash:\n%s", v.convo())
	}
	for i := 0; i < 6; i++ {
		v.send(fmt.Sprintf("question %d", i))
		v.event(map[string]any{"type": "token", "text": fmt.Sprintf("answer %d\n\nmore\n\nand more", i)})
		v.event(map[string]any{"type": "done"})
	}
	if v.m.vp.maxYOffset() < 10 {
		t.Fatalf("the conversation is too short to scroll: max offset %d", v.m.vp.maxYOffset())
	}
	return v
}

// serversEvent says the servers are now these: shell, notes (running),
// and the two extras, raglit on or off.
func serversEvent(raglitOn bool) map[string]any {
	return map[string]any{"type": "server", "servers": []any{
		map[string]any{"id": "shell", "running": true, "tools": float64(1)},
		map[string]any{"id": "notes", "running": true, "tools": float64(1)},
		map[string]any{"id": "raglit", "slash": "raglit", "running": raglitOn},
		map[string]any{"id": "poly-lsp", "slash": "poly-lsp"},
	}}
}

// The splash says what is true now — a server added, an extra turned on —
// by rewriting its rows in place: the same number of rows, the view where
// it was, whether the splash is on screen or scrolled away.
func TestNoBounce_LiveSplashRewritesItsRowsInPlace(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   func(v *vtui) int // the offset to read from; -1 pinned at the bottom
	}{
		{"splash on screen", func(*vtui) int { return 0 }},
		{"scrolled part way", func(v *vtui) int { return v.m.vp.maxYOffset() / 2 }},
		{"at the bottom", func(*vtui) int { return -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := splashConvo(t)
			if y := tc.at(v); y >= 0 {
				v.setScrollPin(false)
				v.setYOffset(y)
			}
			yoff, rows, h := v.m.vp.YOffset, shown(v), v.m.frame.blockH[0]

			v.event(serversEvent(false))
			s := stripANSI(v.m.convo[0].collapsed)
			if !strings.Contains(s, "notes running") {
				t.Fatalf("the splash does not say notes is running:\n%s", s)
			}
			v.event(serversEvent(true))
			s = stripANSI(v.m.convo[0].collapsed)
			if !strings.Contains(s, "project search (raglit) is on") || !strings.Contains(s, "code navigation (poly-lsp) is off") {
				t.Fatalf("the extras row does not follow the switch:\n%s", s)
			}
			if got := v.m.frame.blockH[0]; got != h {
				t.Errorf("the splash went from %d rows to %d", h, got)
			}
			if v.m.vp.YOffset != yoff {
				t.Errorf("the view moved: offset %d -> %d", yoff, v.m.vp.YOffset)
			}
			now := shown(v)
			for i := range rows {
				// Only the splash's own rows may say something new.
				if i+yoff < h {
					continue
				}
				if i >= len(now) || now[i] != rows[i] {
					t.Fatalf("row %d moved:\n was %q\n now %q", i, rows[i], now[min(i, len(now)-1)])
				}
			}
		})
	}
}

// expandedTail is a conversation that ends in a tall block, open.
func expandedTail(t *testing.T) (*vtui, int) {
	t.Helper()
	v := tallConvo(t)
	full := strings.TrimSpace(strings.Repeat("detail row\n", 6))
	v.m.convo = append(v.m.convo, convoEntry{collapsed: "▸ a block, closed", full: full, state: viewExpanded})
	v.m.refresh()
	return v, len(v.m.convo) - 1
}

// A tall block closing at the bottom of the view: the view keeps its top
// row; the rows the block gave back are blank; the view cannot be scrolled
// past where it is; scrolling up and back down finds no more blank; new rows
// take the blank rows.
func TestNoBounce_ClosingABlockKeepsTheTopRow(t *testing.T) {
	v, i := expandedTail(t)
	if !v.m.vp.AtBottom() {
		t.Fatal("precondition: pinned at the bottom")
	}
	yoff, top := v.m.vp.YOffset, shown(v)[0]

	v.m.convo[i].state = viewMinimized
	v.m.refresh()
	if v.m.vp.YOffset != yoff || shown(v)[0] != top {
		t.Fatalf("closing moved the view: offset %d -> %d, top %q -> %q", yoff, v.m.vp.YOffset, top, shown(v)[0])
	}
	if got := v.m.vp.maxYOffset(); got > yoff {
		t.Errorf("blank rows past the view: the furthest scroll is %d, the view was at %d", got, yoff)
	}
	if n := len(v.m.vp.lines); n > yoff+v.m.vp.Height {
		t.Errorf("padded to %d rows, past the bottom of the view (%d)", n, yoff+v.m.vp.Height)
	}

	// Up and back down: no more blank than before.
	v.setScrollPin(false)
	v.setYOffset(yoff - 4)
	v.m.vp.GotoBottom()
	if v.m.vp.YOffset != yoff {
		t.Errorf("scrolling back down went to %d, not %d", v.m.vp.YOffset, yoff)
	}

	// New rows fill the blank rows; once past them the hold is gone.
	v.setScrollPin(true)
	for k := 0; k < 15; k++ {
		v.m.append(fmt.Sprintf("new row %d", k))
	}
	if v.m.convoHold != 0 {
		t.Errorf("the hold outlived the content that passed it: %d", v.m.convoHold)
	}
	if last := shown(v)[len(shown(v))-1]; last != "new row 14" {
		t.Errorf("the pinned view does not follow the new rows: last row %q", last)
	}
}

// The same block closing while the person reads further up: the view does
// not move at all.
func TestNoBounce_ClosingABlockScrolledUp(t *testing.T) {
	v, i := expandedTail(t)
	v.setScrollPin(false)
	v.setYOffset(v.m.vp.maxYOffset() - 3) // the block's top rows still on screen
	yoff, rows := v.m.vp.YOffset, shown(v)

	v.m.convo[i].state = viewMinimized
	v.m.refresh()
	if v.m.vp.YOffset != yoff {
		t.Fatalf("closing moved the view: offset %d -> %d", yoff, v.m.vp.YOffset)
	}
	now := shown(v)
	for k, r := range rows {
		if strings.Contains(r, "detail row") || strings.Contains(r, "a block") {
			break // from the block on, its rows change
		}
		if now[k] != r {
			t.Fatalf("row %d moved: %q -> %q", k, r, now[k])
		}
	}
	if got := v.m.vp.maxYOffset(); got > yoff {
		t.Errorf("blank rows past the view: max %d, view at %d", got, yoff)
	}
}

// One tool call is one line, in words, that stays one line when its result
// comes: no summary over it, no "×1", no "()". Neither the call nor its
// result moves the view, scrolled up or at the bottom.
func TestNoBounce_OneToolCallIsOneLine(t *testing.T) {
	for _, pinned := range []bool{true, false} {
		t.Run(fmt.Sprintf("pinned=%v", pinned), func(t *testing.T) {
			v := tallConvo(t)
			v.send("what do the notes say?")
			v.event(map[string]any{"type": "turn"})
			v.event(map[string]any{"type": "tool_call", "tool": "notes__project_notes", "args": map[string]any{}})
			if !pinned {
				v.setScrollPin(false)
				v.setYOffset(v.m.vp.maxYOffset() - 3)
			}
			i := runIdx(v.m)
			yoff, h := v.m.vp.YOffset, v.m.frame.blockH[i]
			v.event(map[string]any{"type": "tool_result", "tool": "notes__project_notes", "result": "keep the greeting short"})
			if got := v.m.frame.blockH[i]; got != 1 || h != 1 {
				t.Errorf("the call is %d rows, then %d; want 1 and 1", h, got)
			}
			if v.m.vp.YOffset != yoff {
				t.Errorf("the result moved the view: %d -> %d", yoff, v.m.vp.YOffset)
			}
			line := v.m.vp.lines[v.m.frame.rowOffset[i]]
			line = strings.TrimSpace(stripANSI(line))
			if !strings.HasPrefix(line, "✓ notes · project notes") || !strings.Contains(line, "keep the greeting short") {
				t.Errorf("the call reads %q", line)
			}
			for _, bad := range []string{"()", "×1", "1 step", "⚙", "__"} {
				if strings.Contains(v.view(), bad) {
					t.Errorf("the screen shows %q:\n%s", bad, v.view())
				}
			}
		})
	}
}

// A second step makes the run a group: a summary in words over the steps.
func TestRun_TwoStepsAreAGroupInWords(t *testing.T) {
	v := tallConvo(t)
	v.send("go")
	v.event(map[string]any{"type": "turn"})
	v.event(map[string]any{"type": "tool_call", "tool": "notes__project_notes", "args": map[string]any{}})
	v.event(map[string]any{"type": "tool_result", "tool": "notes__project_notes", "result": "a"})
	v.event(map[string]any{"type": "tool_call", "tool": "notes__project_notes", "args": map[string]any{}})
	v.event(map[string]any{"type": "tool_result", "tool": "notes__project_notes", "result": "b"})
	head := stripANSI(v.m.convo[runIdx(v.m)].run.summary())
	if head != "⚙ 2 steps — notes · project notes ×2" {
		t.Errorf("summary %q", head)
	}
}

func TestToolLabel(t *testing.T) {
	for in, want := range map[string]string{
		"notes__project_notes": "notes · project notes",
		"node_query":           "node query",
		"eval":                 "eval",
		"a__b":                 "a · b",
		"__x":                  "x",
	} {
		if got := toolLabel(in); got != want {
			t.Errorf("toolLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// A notice arriving while the person reads further up does not move the
// view.
func TestNoBounce_NoticeWhileScrolledUp(t *testing.T) {
	v := tallConvo(t)
	v.setScrollPin(false)
	v.setYOffset(2)
	yoff, rows := v.m.vp.YOffset, shown(v)
	v.m.appendNotice(convoEntry{collapsed: okNotice("notes added", "running")})
	if v.m.vp.YOffset != yoff || strings.Join(shown(v), "\n") != strings.Join(rows, "\n") {
		t.Errorf("the notice moved the view: offset %d -> %d", yoff, v.m.vp.YOffset)
	}
}

// A form closing grows the pane: the view keeps its top row, and the new
// rows at the bottom are blank until content takes them.
func TestNoBounce_FormClosingKeepsTheTopRow(t *testing.T) {
	v := tallConvo(t)
	v.m = typeStr(v.m, "/login")
	v.m = key(v.m, kEnter)
	if !v.m.providerForm {
		t.Fatal("the login form did not open")
	}
	yoff, top := v.m.vp.YOffset, shown(v)[0]
	v.m = key(v.m, kEsc)
	if v.m.providerForm {
		t.Fatal("esc did not close the form")
	}
	if v.m.vp.YOffset != yoff || shown(v)[0] != top {
		t.Fatalf("closing the form moved the view: offset %d -> %d, top %q -> %q", yoff, v.m.vp.YOffset, top, shown(v)[0])
	}
	if got := v.m.vp.maxYOffset(); got > yoff {
		t.Errorf("blank rows past the view: max %d, view at %d", got, yoff)
	}
}

// On a phone (48×24) typing "/mcp add" opens the palette (the pane shrinks,
// the view follows the bottom), narrows it (the pane grows and holds blank
// rows at the bottom), then enter opens the form (the pane shrinks again).
// That last shrink gives up the blank rows: the top row stays. Anchoring
// the bottom there pushed the text out of the view and left the pane blank.
func TestNoBounce_PhoneFormAfterPaletteKeepsTheText(t *testing.T) {
	v := newVtui(48, 24)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}, "servers": []any{
		map[string]any{"id": "shell", "running": true, "tools": float64(1)},
	}})
	v.event(map[string]any{"type": "settings", "model": "chat-standard", "url": "http://llm/v1", "provider": "work"})
	v.send("Can you help me with this project?")
	v.event(map[string]any{"type": "token", "text": "This project is a small command-line tool. The README says it prints a greeting, and main.go holds the whole program. I can read files, run commands in an isolated copy of the project, and use the tools you add. What would you like to change first?"})
	v.event(map[string]any{"type": "done"})
	for _, r := range "/mcp add" {
		v.m = typeStr(v.m, string(r))
	}
	yoff, top := v.m.vp.YOffset, shown(v)[0]
	v.m = key(v.m, kEnter)
	if !v.m.mcpForm {
		t.Fatal("the /mcp add form did not open")
	}
	if v.m.vp.YOffset != yoff || shown(v)[0] != top {
		t.Errorf("opening the form moved the view: offset %d -> %d, top %q -> %q", yoff, v.m.vp.YOffset, top, shown(v)[0])
	}
	if !strings.Contains(strings.Join(shown(v), "\n"), "change first?") {
		t.Errorf("the answer left the view:\n%s", strings.Join(shown(v), "\n"))
	}
}
