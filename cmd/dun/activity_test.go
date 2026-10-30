package main

import (
	"strings"
	"testing"
	"time"
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
