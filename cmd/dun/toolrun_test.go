package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// call feeds one round: a line of thinking, then an exec call and its result.
func call(v *vtui, cmd, result string) {
	v.event(map[string]any{"type": "reasoning", "text": "next: " + cmd})
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

// Five calls in a row: the four earlier ones (and their thinking) fold into
// one row that counts them, and the fifth shows as a call does.
func TestToolRun_FoldsEarlierCalls(t *testing.T) {
	v := newVtui(100, 30)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("run them")
	for i := 0; i < 4; i++ {
		call(v, fmt.Sprintf("echo %d", i), fmt.Sprintf("%d\n", i))
	}
	v.event(map[string]any{"type": "reasoning", "text": "one more"})
	v.event(map[string]any{"type": "tool_call", "tool": "eval", "args": map[string]any{"code": "1/0"}})
	v.event(map[string]any{"type": "tool_result", "tool": "eval", "result": "ERROR: division by zero"})
	call(v, "echo last", "last\n")

	r := runIdx(v.m)
	if r < 0 {
		t.Fatalf("no aggregate:\n%s", v.convo())
	}
	agg := v.m.convo[r].run
	if agg.calls() != 5 {
		t.Errorf("aggregate holds %d calls, want 5", agg.calls())
	}
	sum := stripANSI(v.m.convo[r].view())
	if !strings.Contains(sum, "5 tool calls — exec×4, eval×1 · 1 failed") || strings.Contains(sum, "\n") {
		t.Errorf("summary: %q", sum)
	}
	// After the aggregate: the latest round's thinking, then the latest call.
	tail := v.m.convo[r+1:]
	if len(tail) != 2 || tail[0].think == nil || tail[1].tool == nil || !strings.Contains(stripANSI(tail[1].view()), "echo last") {
		t.Fatalf("tail after the aggregate: %d entries\n%s", len(tail), v.convo())
	}
	// Exactly one aggregate: the run folded into one row, not one per call.
	n := 0
	for _, e := range v.m.convo {
		if e.run != nil {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d aggregates", n)
	}
	// A reply ends the run: the next call starts a new one.
	v.event(map[string]any{"type": "token", "text": "half way"})
	call(v, "echo new", "new\n")
	if runIdx(v.m) != r {
		t.Errorf("a call after a reply joined the old run")
	}
}

// The latest call is held at the tallest height the run has shown, and gets
// its own height back when the run ends.
func TestToolRun_HoldsTheTallestHeight(t *testing.T) {
	v := newVtui(40, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("go")
	call(v, "echo "+strings.Repeat("long ", 14), "x\n") // wraps at 40 columns
	tall := v.m.frame.blockH[len(v.m.convo)-1]
	call(v, "echo short", "y\n")
	last := len(v.m.convo) - 1
	if got := v.m.frame.blockH[last]; got != tall {
		t.Fatalf("short call after a tall one is %d rows, want held at %d", got, tall)
	}
	natural := strings.Count(v.m.convo[last].view(), "\n") + 1
	if natural >= tall {
		t.Fatalf("test needs a shorter call: natural %d, tall %d", natural, tall)
	}
	v.event(map[string]any{"type": "done"})
	if got := v.m.frame.blockH[last]; got != natural {
		t.Errorf("after the run ended the call is %d rows, want its own %d", got, natural)
	}
}

// A reader who scrolled up is not moved by calls arriving, by results, or by
// folding — and a fold that would change rows on their screen waits.
func TestToolRun_DoesNotScrollAReaderWhoScrolledUp(t *testing.T) {
	v := tallConvo(t)
	v.send("again")
	call(v, "echo 0", "0\n")
	call(v, "echo 1", "1\n")
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyPgUp})
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

// An aggregate the person has selected does not change under them; the fold
// catches up once they move away.
func TestToolRun_SelectedAggregateDoesNotRefold(t *testing.T) {
	v := newVtui(100, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("go")
	for i := 0; i < 3; i++ {
		call(v, fmt.Sprintf("echo %d", i), "ok\n")
	}
	r := runIdx(v.m)
	v.m = key(v.m, kTab)
	for v.m.sel > r {
		v.m = key(v.m, kUp)
	}
	if v.m.sel != r {
		t.Fatalf("could not select the aggregate: sel=%d run=%d", v.m.sel, r)
	}
	before := v.m.convo[r].run.calls()
	call(v, "echo 3", "ok\n")
	if v.m.sel != r || v.m.convo[r].run == nil || v.m.convo[r].run.calls() != before {
		t.Fatalf("the selected aggregate changed: sel=%d calls %d → %d", v.m.sel, before, v.m.convo[r].run.calls())
	}
	v.m = key(v.m, kTab) // away from the conversation
	call(v, "echo 4", "ok\n")
	if got := v.m.convo[runIdx(v.m)].run.calls(); got != 4 {
		t.Errorf("deferred fold did not catch up: %d calls, want 4", got)
	}
}

// Enter on the aggregate opens it as a tree: a node per call (and thinking),
// enter opens a node to its result, failures are marked, esc comes back.
func TestToolRun_EnterOpensTheTree(t *testing.T) {
	v := newVtui(100, 30)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("go")
	call(v, "echo 0", "zero\n")
	call(v, "false", "\n[exit: 1]")
	call(v, "echo 2", "two\n")
	r := runIdx(v.m)
	v.m = key(v.m, kTab)
	for v.m.sel > r {
		v.m = key(v.m, kUp)
	}
	v.m = key(v.m, kEnter)
	if !v.m.paging || !v.m.pg.tree() {
		t.Fatalf("enter did not open the tree: paging=%v", v.m.paging)
	}
	// thinking, call, thinking, call
	if len(v.m.pg.doc.nodes) != 4 || !v.m.pg.doc.nodes[3].err || v.m.pg.doc.nodes[1].err {
		t.Fatalf("nodes: %+v", v.m.pg.doc.nodes)
	}
	scr := v.view()
	if !strings.Contains(scr, "2 tool calls") || !strings.Contains(scr, "✓ $ echo 0") || !strings.Contains(scr, "✗ $ false") {
		t.Fatalf("tree not on screen:\n%s", scr)
	}
	if strings.Contains(scr, "zero") {
		t.Fatalf("a node opened before it was asked:\n%s", scr)
	}
	v.m = key(v.m, kDown)
	v.m = key(v.m, kEnter)
	if scr := v.view(); !strings.Contains(scr, "zero") {
		t.Fatalf("enter did not open the node:\n%s", scr)
	}
	v.m = key(v.m, kEnter)
	if scr := v.view(); strings.Contains(scr, "zero") {
		t.Fatalf("enter did not close the node:\n%s", scr)
	}
	v.m = key(v.m, kEsc)
	if v.m.paging || v.m.quitting || v.m.sel != r {
		t.Fatalf("esc: paging=%v quitting=%v sel=%d", v.m.paging, v.m.quitting, v.m.sel)
	}
}

// A long tree scrolls: the selection stays on screen as it moves down.
func TestToolRun_TreeScrolls(t *testing.T) {
	v := newVtui(100, 16)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("go")
	for i := 0; i < 30; i++ {
		call(v, fmt.Sprintf("echo %d", i), "ok\n")
	}
	r := runIdx(v.m)
	v.m = key(v.m, kTab)
	for v.m.sel > r {
		v.m = key(v.m, kUp)
	}
	v.m = key(v.m, kEnter)
	for i := 0; i < 50; i++ {
		v.m = key(v.m, kDown)
	}
	if v.m.pg.off == 0 {
		t.Fatal("the tree did not scroll with the selection")
	}
	if scr := v.view(); !strings.Contains(scr, "echo 25") {
		t.Errorf("selected node not on screen:\n%s", scr)
	}
}

// Resumed history folds a past run the same way.
func TestToolRun_ReplayFolds(t *testing.T) {
	v := newVtui(100, 30)
	items := []any{map[string]any{"kind": "user", "content": "go"}}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("c%d", i)
		items = append(items,
			map[string]any{"kind": "reasoning", "content": "think " + id},
			map[string]any{"kind": "tool_call", "tool": "exec", "call_id": id, "args": map[string]any{"command": "echo " + id}},
			map[string]any{"kind": "tool_result", "tool": "exec", "call_id": id, "content": id})
	}
	items = append(items, map[string]any{"kind": "assistant", "content": "done"})
	v.event(map[string]any{"type": "history", "items": items})
	r := runIdx(v.m)
	if r < 0 || v.m.convo[r].run.calls() != 4 {
		t.Fatalf("history did not fold:\n%s", v.convo())
	}
	if e := v.m.convo[len(v.m.convo)-2]; e.mdSource != "done" {
		// the reply, then the resumed marker
		t.Errorf("reply not after the run: %q", e.mdSource)
	}
	for _, e := range v.m.convo {
		if e.holdH != 0 {
			t.Error("history holds a height")
		}
	}
}
