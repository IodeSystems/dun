package tui

import (
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
