package main

import (
	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// planEvent is the session's task plan for a client's /plan screen
// (dun/plan.go).
func planEvent(h *dun.Harness, msg string) *proto.Plan {
	s := h.Plan()
	ev := &proto.Plan{Mode: s.Mode, Approved: s.Approved, Total: s.Total, Done: s.Done, Rows: []proto.PlanNode{}, Message: msg}
	for _, r := range s.Rows {
		ev.Rows = append(ev.Rows, proto.PlanNode{ID: r.ID, Title: r.Title, Goal: r.Goal, Status: r.Status, DoneWhen: r.DoneWhen,
			Needs: r.Needs, Owner: r.Owner, Files: r.Files, Replaces: r.Replaces, Notes: r.Notes, Why: r.Why, Account: r.Account,
			Waiting: r.Waiting, Ready: r.Ready, Depth: r.Depth, Repeat: r.Repeat})
	}
	return ev
}
