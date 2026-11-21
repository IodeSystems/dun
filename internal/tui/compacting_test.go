package tui

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// busyModel is a TUI mid-turn, ready to stream.
func busyModel(t *testing.T) tuiModel {
	t.Helper()
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = resized(m, 100, 40)
	m = m.feed(evMsg{"type": "ready", "tools": []any{"exec"}})
	m.busy = true
	return m
}

// inOrder fails unless each of want appears in the drawn view, in order.
func inOrder(t *testing.T, m tuiModel, want ...string) {
	t.Helper()
	m.refresh()
	view := stripANSI(m.View().Content)
	at := 0
	for _, w := range want {
		i := strings.Index(view[at:], w)
		if i < 0 {
			t.Fatalf("%q not after %d in the view:\n%s", w, at, view)
		}
		at += i + len(w)
	}
}

// A message queued while the reply streams is drawn below the reply, and
// stays below what the turn shows until it is delivered, where it is.
func TestQueued_BelowTheStreamingReply(t *testing.T) {
	m := busyModel(t)
	m = m.feed(evMsg{"type": "token", "text": "Working on the parser now."})
	m = m.feed(evMsg{"type": "user", "id": "m1", "content": "also the lexer", "queued": true})
	inOrder(t, m, "Working on the parser now.", "also the lexer (queued)")
	if m.cur == "" {
		t.Fatal("queueing a message finished the streaming reply")
	}

	// The reply ends in a tool call: the queued row stays below it.
	m = m.feed(evMsg{"type": "tool_call", "call_id": "c1", "tool": "exec", "args": map[string]any{"cmd": "ls lexer"}})
	m = m.feed(evMsg{"type": "tool_result", "call_id": "c1", "tool": "exec", "result": "lexer.go"})
	inOrder(t, m, "Working on the parser now.", "ls lexer", "also the lexer (queued)")

	// Delivered: an ordinary user row, at the end, where it was drawn.
	m = m.feed(evMsg{"type": "delivered", "ids": []any{"m1"}})
	inOrder(t, m, "Working on the parser now.", "ls lexer", "also the lexer")
	if strings.Contains(stripANSI(m.View().Content), "(queued)") {
		t.Error("still drawn as queued after delivered")
	}
}

// Compacting mid-turn: the reply so far, the "Compacting…" row, the message
// queued meanwhile; then the compaction's line takes the row's place.
func TestCompacting_RowThenDetailsThenQueued(t *testing.T) {
	m := busyModel(t)
	m = m.feed(evMsg{"type": "token", "text": "Reading the files."})
	m = m.feed(evMsg{"type": "compacting", "phase": "start"})
	if m.cur != "" {
		t.Fatal("the reply was not finished before the row")
	}
	m = m.feed(evMsg{"type": "user", "id": "m1", "content": "my msg", "queued": true})
	inOrder(t, m, "Reading the files.", "Compacting…", "my msg (queued)")

	m = m.feed(evMsg{"type": "compaction", "text": "folded 12 entries", "tokens_before": 9000.0, "tokens_after": 2000.0})
	inOrder(t, m, "Reading the files.", "🗜 folded 12 entries", "my msg (queued)")
	if strings.Contains(stripANSI(m.View().Content), "Compacting…") {
		t.Error("the row still turns after the compaction")
	}

	m = m.feed(evMsg{"type": "delivered", "ids": []any{"m1"}})
	inOrder(t, m, "Reading the files.", "🗜 folded 12 entries", "my msg")
	if n := strings.Count(stripANSI(convoText(m)), "folded 12 entries"); n != 1 {
		t.Errorf("compaction line drawn %d times", n)
	}
}

// A fold that ends without a compaction: a failed one says so in the row's
// place; one that folded nothing takes the row out.
func TestCompacting_EndWithoutCompaction(t *testing.T) {
	m := busyModel(t)
	m = m.feed(evMsg{"type": "compacting", "phase": "start"})
	m = m.feed(evMsg{"type": "compacting", "phase": "end", "error": "context canceled"})
	view := stripANSI(convoText(m))
	if strings.Contains(view, "Compacting…") || !strings.Contains(view, "compaction did not finish: context canceled") {
		t.Errorf("failed fold:\n%s", view)
	}

	n := len(m.convo)
	m = m.feed(evMsg{"type": "compacting", "phase": "start"})
	m = m.feed(evMsg{"type": "compacting", "phase": "end"})
	if len(m.convo) != n || m.compactingRow() >= 0 {
		t.Errorf("a fold of nothing left a row:\n%s", convoText(m))
	}
}

// An engine before the compacting event: the compaction still lands after
// the reply streamed so far, not above it.
func TestCompaction_WithoutStartFollowsTheReply(t *testing.T) {
	m := busyModel(t)
	m = m.feed(evMsg{"type": "token", "text": "Reading the files."})
	m = m.feed(evMsg{"type": "compaction", "text": "folded 3 entries"})
	inOrder(t, m, "Reading the files.", "🗜 folded 3 entries")
}

// The engine goes away mid-fold: no end will come, so the row goes.
func TestCompacting_EngineRestartEndsTheRow(t *testing.T) {
	m := busyModel(t)
	m = m.feed(evMsg{"type": "compacting", "phase": "start"})
	m = m.feed(evMsg{"type": "engine", "state": "restarting", "reason": "the engine crashed"})
	if m.compactingRow() >= 0 {
		t.Error("the row outlived its engine")
	}
}

// Delivered while a reply streams: the reply is finished first, above the
// message the engine put after it.
func TestDelivered_FinishesTheStreamingReply(t *testing.T) {
	m := busyModel(t)
	m = m.feed(evMsg{"type": "user", "id": "m1", "content": "also the lexer", "queued": true})
	m = m.feed(evMsg{"type": "token", "text": "Working on the parser now."})
	m = m.feed(evMsg{"type": "delivered", "ids": []any{"m1"}})
	if m.cur != "" {
		t.Fatal("the reply is still streaming below a delivered message")
	}
	inOrder(t, m, "Working on the parser now.", "also the lexer")
}

// A selected row keeps its selection when queued rows settle around it.
func TestQueued_SettlingKeepsTheSelection(t *testing.T) {
	m := busyModel(t)
	m.append("row a")
	m = m.feed(evMsg{"type": "user", "id": "m1", "content": "queued one", "queued": true})
	m.append("row b")
	m.sel = len(m.convo) - 1 // row b, after the queued row
	m.busy = false
	m.reconcileQueued([]any{}) // lost: moved last
	if !strings.Contains(m.convo[m.sel].collapsed, "row b") {
		t.Errorf("selection moved to %q", m.convo[m.sel].collapsed)
	}
	m.sel = len(m.convo) - 1 // the lost row itself
	m.sinkRows(func(e *convoEntry) bool { return e.lost })
	if m.sel < 0 || !m.convo[m.sel].lost {
		t.Errorf("selection left the sunk row: %d", m.sel)
	}
}

// ↓ with only queued rows below stays on the last real row; with no real
// row at all there is no selection.
func TestSelSkipDown_OverQueuedRows(t *testing.T) {
	m := busyModel(t)
	m.append("real")
	base := len(m.convo) - 1
	m = m.feed(evMsg{"type": "user", "id": "m1", "content": "queued", "queued": true})
	if got := m.selSkipDown(base + 1); got != base {
		t.Errorf("selSkipDown = %d, want %d", got, base)
	}
	m.convo = []convoEntry{queuedEntry("q", "only queued")}
	if got := m.selSkipDown(0); got != -1 {
		t.Errorf("all skippable: selSkipDown = %d, want -1", got)
	}
}

// ctrl+t keeps the row at the top of the view where it was, even with a
// queued row earlier in the conversation than the rows drawn above it.
func TestToggleThinking_AnchorIsTheTopRowNotAQueuedOne(t *testing.T) {
	m := busyModel(t)
	for i := 0; i < 30; i++ {
		m.append("before " + strconv.Itoa(i))
	}
	m = m.feed(evMsg{"type": "user", "id": "m1", "content": "queued one", "queued": true})
	m.append("anchor row")
	m.addThought(&thinkBlock{text: "**Thinking**\n" + strings.Repeat("a line of thought\n", 12)})
	for i := 0; i < 80; i++ {
		m.append("after " + strconv.Itoa(i))
	}
	m.refresh()
	at := slices.IndexFunc(m.convo, func(e convoEntry) bool { return strings.Contains(e.collapsed, "anchor row") })
	m.scrollPinned = false
	m.vp.SetYOffset(m.frame.rowOffset[at])
	if m.vp.YOffset != m.frame.rowOffset[at] {
		t.Fatalf("cannot scroll to the anchor row: %d", m.vp.YOffset)
	}
	m.toggleThinking()
	if got := m.vp.YOffset; got != m.frame.rowOffset[at] {
		t.Errorf("view top moved: YOffset %d, anchor row at %d", got, m.frame.rowOffset[at])
	}
}

// The jump to the latest message skips a queued one drawn below.
func TestScrollToLastUser_SkipsQueued(t *testing.T) {
	m := busyModel(t)
	m.appendUser("the real ask")
	for i := 0; i < 40; i++ {
		m.append("line " + strconv.Itoa(i))
	}
	m = m.feed(evMsg{"type": "user", "id": "m1", "content": "queued one", "queued": true})
	m.refresh()
	m.scrollToLastUser()
	if m.userJump < 0 || m.convo[m.userJump].provisional {
		t.Errorf("jumped to %d", m.userJump)
	}
}
