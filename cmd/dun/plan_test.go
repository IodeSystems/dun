package main

import (
	"testing"

	"github.com/iodesystems/dun"
)

// The plan event is the tree the harness reports, row for row.
func TestPlanEventCarriesTheTree(t *testing.T) {
	h := &dun.Harness{}
	if ev := planEvent(h, "hi"); ev.Message != "hi" || len(ev.Rows) != 0 || ev.Rows == nil {
		t.Errorf("empty plan: %+v", ev)
	}
}
