package main

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// call feeds one round: a line of thinking, then an exec call and its result.
func call(v *vtui, cmd, result string) {
	v.event(map[string]any{"type": "reasoning", "text": "next: " + cmd + ". Then check it."})
	v.event(map[string]any{"type": "tool_call", "tool": "exec", "args": map[string]any{"command": cmd}})
	v.event(map[string]any{"type": "tool_result", "tool": "exec", "result": result})
}

func runIdx(m tuiModel) int {
	for i := len(m.convo) - 1; i >= 0; i-- {
		if m.convo[i].run != nil {
			return i
		}
	}
	return -1
}

func runsIn(m tuiModel) int {
	n := 0
	for _, e := range m.convo {
		if e.run != nil {
			n++
		}
	}
	return n
}

// A run of calls with thinking between them is ONE box: every thought and
// call is appended inside it, the first thought included, and the header
// counts them.
func TestRun_OneBoxThatAppends(t *testing.T) {
	v := newVtui(100, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("run them")
	blocks := len(v.m.convo)
	for i := 0; i < 4; i++ {
		call(v, fmt.Sprintf("echo %d", i), fmt.Sprintf("%d\n", i))
	}
	v.event(map[string]any{"type": "reasoning", "text": "divide."})
	v.event(map[string]any{"type": "tool_call", "tool": "eval", "args": map[string]any{"code": "1/0"}})
	v.event(map[string]any{"type": "tool_result", "tool": "eval", "result": "ERROR: division by zero"})

	if runsIn(v.m) != 1 || len(v.m.convo) != blocks+1 {
		t.Fatalf("want one run block after the message, got %d runs, %d blocks (was %d)", runsIn(v.m), len(v.m.convo), blocks)
	}
	r := v.m.convo[runIdx(v.m)].run
	if len(r.items) != 10 || r.items[0].think == nil {
		t.Fatalf("items: %d, first is a thought: %v", len(r.items), r.items[0].think != nil)
	}
	head := stripANSI(r.summary())
	if !strings.Contains(head, "10 steps — exec ×4, eval ·") || !strings.Contains(head, "1 failed") {
		t.Errorf("header: %q", head)
	}
	// A reply ends the run; the next call starts another, below.
	v.event(map[string]any{"type": "token", "text": "half way"})
	call(v, "echo new", "new\n")
	if runsIn(v.m) != 2 {
		t.Errorf("a call after a reply should start a new run: %d runs", runsIn(v.m))
	}
}

// A finished thought stays open while its run is on screen: the box shows
// it as it streamed, under its "thought for" header. Once the run has
// scrolled off the top of the view, the thought folds to its last sentence.
func TestRun_FinishedThoughtOpenUntilScrolledOff(t *testing.T) {
	v := newVtui(100, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("go")
	call(v, "echo 0", "0\n")
	v.event(map[string]any{"type": "reasoning", "text": "first part.\nsecond line of it\n"})
	nm, _ := v.m.Update(renderTickMsg{})
	v.m = nm.(tuiModel)
	scr := v.view()
	for _, want := range []string{"▾ ✻ thought for", "next: echo 0. Then check it.", "✓ $ echo 0", "▾ ✻ thinking…", "second line of it"} {
		if !strings.Contains(scr, want) {
			t.Errorf("missing %q:\n%s", want, scr)
		}
	}
	// Push the run off the top, then read back up to it.
	v.event(map[string]any{"type": "done"})
	for i := 0; i < 6; i++ {
		v.send(fmt.Sprintf("question %d", i))
		v.event(map[string]any{"type": "token", "text": "answer\n\nmore\n\nand more\n\nand more"})
		v.event(map[string]any{"type": "done"})
	}
	r := runIdx(v.m)
	if top := v.m.frame.rowOffset[r]; top+v.m.frame.blockH[r] > v.m.vp.YOffset {
		t.Fatalf("precondition: the run is above the view (%d+%d, view at %d)", top, v.m.frame.blockH[r], v.m.vp.YOffset)
	}
	v.setScrollPin(false)
	v.setYOffset(v.m.frame.rowOffset[r])
	scr = v.view()
	if !strings.Contains(scr, "✻ Then check it.  (") || strings.Contains(scr, "next: echo 0.") {
		t.Errorf("scrolled off and back, the finished thought should be its last sentence:\n%s", scr)
	}
}

// When the active step finishes, the box keeps its height and its rows: the
// thought's text stays where it was, and the next call comes in under it.
func TestRun_FinishedThoughtKeepsItsRows(t *testing.T) {
	v := newVtui(100, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	fillScreen(v) // no free rows: the box is at its push cap, short of all its steps
	v.send("go")
	for i := 0; i < 6; i++ {
		call(v, fmt.Sprintf("echo %d", i), "ok\n")
	}
	var long strings.Builder
	for k := 0; k < 20; k++ {
		fmt.Fprintf(&long, "a long line %d of thought\n", k)
	}
	v.event(map[string]any{"type": "reasoning", "text": long.String()})
	nm, _ := v.m.Update(renderTickMsg{})
	v.m = nm.(tuiModel)
	r := runIdx(v.m)
	before := v.m.frame.blockH[r]
	if before != capOf(v.m, r) {
		t.Fatalf("precondition: box at its cap, %d", before)
	}
	v.event(map[string]any{"type": "tool_call", "tool": "exec", "args": map[string]any{"command": "echo 6"}})
	if h := v.m.frame.blockH[r]; h != before {
		t.Fatalf("the box changed height when the thought finished: %d → %d", before, h)
	}
	scr := v.view()
	if !strings.Contains(scr, "a long line 19 of thought") || !strings.Contains(scr, "$ echo 6") {
		t.Errorf("the finished thought's rows should stay, the call under them:\n%s", scr)
	}
	if strings.Contains(scr, "$ echo 2") {
		t.Errorf("the thought folded on screen: earlier steps took its rows:\n%s", scr)
	}
}

// A failed call keeps up to two lines of its error, and is not grouped.
func TestRun_FailureLines(t *testing.T) {
	v := newVtui(100, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("go")
	call(v, "go test ./...", "ok  pkg/a\nconfig_test.go:41: want 3 layers, got 2\nFAIL pkg/b\n[exit: 1]")
	scr := v.view()
	if !strings.Contains(scr, "✗ $ go test ./...") || !strings.Contains(scr, "config_test.go:41: want 3 layers, got 2") || !strings.Contains(scr, "FAIL pkg/b") {
		t.Errorf("failure and its error lines:\n%s", scr)
	}
	if strings.Contains(scr, "ok  pkg/a") {
		t.Errorf("only the last two lines of the error belong under it:\n%s", scr)
	}
}

// Consecutive successful calls of one tool with short targets group into one
// line; a failed one is not grouped.
func TestRun_Grouping(t *testing.T) {
	items := []*runItem{}
	for _, f := range []string{"a.go", "b.go", "c.go", "d.go"} {
		items = append(items, &runItem{tool: "read", args: map[string]any{"path": f}, done: true})
	}
	items = append(items, &runItem{tool: "read", args: map[string]any{"path": "e.go"}, done: true, failed: true})
	if n := groupLen(items); n != 4 {
		t.Fatalf("groupLen = %d, want 4 (the failed one breaks the group)", n)
	}
	if got := stripANSI(groupLine(items[:4], 80)); got != "✓ read ×4: a.go, b.go, c.go, d.go" {
		t.Errorf("group line: %q", got)
	}
	long := &runItem{tool: "read", args: map[string]any{"path": strings.Repeat("x", 40)}, done: true}
	if groupLen([]*runItem{items[0], long}) != 1 {
		t.Error("a long target should not group")
	}
}

func TestLastSentence(t *testing.T) {
	for in, want := range map[string]string{
		"First. Then the second one.":           "Then the second one.",
		"one\nsentence across lines":            "one sentence across lines",
		"Ends with a question? And an answer!":  "And an answer!",
		"version 1.2 is fine. so load it last.": "so load it last.",
		"  trailing space.  ":                   "trailing space.",
		"":                                      "",
	} {
		if got := lastSentence(in); got != want {
			t.Errorf("lastSentence(%q) = %q, want %q", in, got, want)
		}
	}
}

// A reader who scrolled up is not moved by calls arriving in a run.
func TestRun_DoesNotScrollAReaderWhoScrolledUp(t *testing.T) {
	v := tallConvo(t)
	v.send("again")
	call(v, "echo 0", "0\n")
	call(v, "echo 1", "1\n")
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if v.m.scrollPinned {
		t.Fatal("precondition: pgup should unpin")
	}
	y := v.m.vp.YOffset
	for i := 2; i < 8; i++ {
		call(v, fmt.Sprintf("echo %d", i), fmt.Sprintf("%d\n", i))
		if v.m.vp.YOffset != y || v.m.scrollPinned {
			t.Fatalf("call %d moved the viewport: %d → %d (pinned=%v)", i, y, v.m.vp.YOffset, v.m.scrollPinned)
		}
	}
}

// Enter on the run opens it as a tree: a node per thought and call, a call
// opens to its result, failures are marked.
func TestRun_EnterOpensTheTree(t *testing.T) {
	v := newVtui(100, 30)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("go")
	call(v, "echo 0", "zero\n")
	call(v, "false", "\n[exit: 1]")
	r := runIdx(v.m)
	v.m = key(v.m, kTab)
	for v.m.sel > r {
		v.m = key(v.m, kUp)
	}
	v.m = key(v.m, kEnter)
	if !v.m.paging || !v.m.pg.tree() {
		t.Fatalf("enter did not open the tree: paging=%v", v.m.paging)
	}
	if len(v.m.pg.doc.nodes) != 4 || !v.m.pg.doc.nodes[3].err || v.m.pg.doc.nodes[1].err {
		t.Fatalf("nodes: %+v", v.m.pg.doc.nodes)
	}
	v.m = key(v.m, kDown)
	v.m = key(v.m, kEnter)
	if !strings.Contains(v.view(), "zero") {
		t.Fatalf("enter did not open the node:\n%s", v.view())
	}
	v.m = key(v.m, kEsc)
	if v.m.paging || v.m.sel != r {
		t.Fatalf("esc: paging=%v sel=%d", v.m.paging, v.m.sel)
	}
}

// Resumed history builds the same run box.
func TestRun_Replay(t *testing.T) {
	v := newVtui(100, 30)
	items := []any{map[string]any{"kind": "user", "content": "go"}}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("c%d", i)
		items = append(items,
			map[string]any{"kind": "reasoning", "content": "think " + id + "."},
			map[string]any{"kind": "tool_call", "tool": "exec", "call_id": id, "args": map[string]any{"command": "echo " + id}},
			map[string]any{"kind": "tool_result", "tool": "exec", "call_id": id, "content": id})
	}
	items = append(items, map[string]any{"kind": "assistant", "content": "done"})
	v.event(map[string]any{"type": "history", "items": items})
	if runsIn(v.m) != 1 {
		t.Fatalf("history should build one run, got %d:\n%s", runsIn(v.m), v.convo())
	}
	r := v.m.convo[runIdx(v.m)]
	if len(r.run.items) != 10 || r.run.open || !r.boxed() {
		t.Errorf("replayed run: %d items, open=%v, boxed=%v", len(r.run.items), r.run.open, r.boxed())
	}
}
