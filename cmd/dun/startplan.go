package main

import (
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// startPlan is the plan an engine says when it starts, so a snapshot of
// the session carries it (a restart, a session/load): nil with no rows.
func startPlan(h *harness.Harness) *proto.Plan {
	if len(h.Plan().Rows) == 0 {
		return nil
	}
	return planEvent(h, "")
}
