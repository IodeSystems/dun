package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The engine pushes agents on a change only, so a running child's time is
// counted on from the push; a child that is not running keeps its figure.
func TestAgentRow_RunningTimeCountsOn(t *testing.T) {
	at := time.Now()
	a := agentRow{id: 1, state: "running", seconds: 5, at: at}
	if got := agentActRow(a, at.Add(70*time.Second)).label; !strings.Contains(got, compactDur(75)) {
		t.Errorf("running: %q, want %s", got, compactDur(75))
	}
	a.state = "idle"
	if got := agentActRow(a, at.Add(70*time.Second)).label; !strings.Contains(got, compactDur(5)) {
		t.Errorf("idle: %q, want %s", got, compactDur(5))
	}
}

// A line wrapped while the pane was narrow is wrapped again when it widens,
// also to a width it still does not fit.
func TestNotice_RewrapsWhenThePaneWidens(t *testing.T) {
	v := newVtui(48, 20)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	words := strings.TrimSpace(strings.Repeat("notice ", 30)) // 209 columns
	v.m.append(words)
	widest := func() int {
		n := 0
		for _, l := range strings.Split(v.view(), "\n") {
			if strings.Contains(l, "notice") {
				n = max(n, len(strings.TrimSpace(l)))
			}
		}
		return n
	}
	if w := widest(); w > 48 {
		t.Fatalf("setup: %d columns at 48", w)
	}
	nm, _ := v.m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	v.m = nm.(tuiModel)
	if w := widest(); w <= 48 {
		t.Errorf("widened to 120, the notice is still %d columns wide", w)
	}
}
