package main

import (
	"testing"

	"github.com/iodesystems/dun/internal/harness"
)

// An engine with no plan says nothing at start.
func TestStartPlanEmpty(t *testing.T) {
	if ev := startPlan(&harness.Harness{}); ev != nil {
		t.Errorf("empty plan: %+v", ev)
	}
}
