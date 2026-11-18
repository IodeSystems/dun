package tui

import (
	"strings"
	"testing"
	"time"
)

// turn_state's net_wait is the idle status line: since when, and the next
// probe counting down; it goes with the next turn_state that lacks it.
func TestTurnState_NetWaitStatus(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = resized(m, 120, 30)
	since := time.Date(2026, 11, 18, 14, 2, 0, 0, time.Local)
	next := time.Now().Add(30 * time.Second)
	m = m.handleEvent(evMsg{"type": "turn_state", "phase": "idle", "queued": []any{},
		"net_wait": map[string]any{"since": float64(since.UnixMilli()), "next": float64(next.UnixMilli())}})
	got := m.readyWord()
	if !strings.HasPrefix(got, "waiting for network · since 14:02 · next try ") {
		t.Fatalf("status %q", got)
	}
	if w := netWaitWord(since, since.Add(30*time.Second), since); w != "waiting for network · since 14:02 · next try 30s" {
		t.Errorf("word %q", w)
	}
	if w := netWaitWord(since, since, since.Add(time.Second)); !strings.HasSuffix(w, "· trying now") {
		t.Errorf("past the probe: %q", w)
	}
	m = m.handleEvent(evMsg{"type": "turn_state", "phase": "idle", "queued": []any{}})
	if got := m.readyWord(); got != "ready" {
		t.Errorf("after the wait: %q", got)
	}
}
