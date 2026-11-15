package main

import (
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// A turn cut before its first round returns a zero tally; the usage event
// keeps the session's totals instead of saying 0 (clients set them).
func TestUsageTally_ZeroTurnKeepsTheTotals(t *testing.T) {
	var tl usageTally
	two := agent.TokenUsage{Total: 900, Cached: 500, Processed: 300, Generated: 100, Turns: 2, Active: 700}
	if got := tl.at(two); got != two {
		t.Fatalf("got %+v", got)
	}
	got := tl.at(agent.TokenUsage{Active: 750})
	if got.Total != 900 || got.Turns != 2 || got.Active != 750 {
		t.Fatalf("a zero tally: %+v", got)
	}
	three := agent.TokenUsage{Total: 1400, Turns: 3}
	if got := tl.at(three); got != three {
		t.Fatalf("a later tally: %+v", got)
	}
}
