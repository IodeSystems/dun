package main

import (
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// planEvent is the session's task plan for a client's /plan screen
// (dun/plan.go), with the last /plan summary (dun/plansummary.go) and the
// asks: each ask node's state, and the intake (harness asktool.go).
func planEvent(h *harness.Harness, msg string) *proto.Plan {
	s := h.Plan()
	ev := &proto.Plan{Mode: s.Mode, Approved: s.Approved, Total: s.Total, Done: s.Done, Rows: []proto.PlanNode{}, Message: msg}
	for _, r := range s.Rows {
		n := proto.PlanNode{ID: r.ID, Title: r.Title, Details: r.Details, Status: r.Status, DoneWhen: r.DoneWhen,
			Needs: r.Needs, Owner: r.Owner, Replaces: r.Replaces, Why: r.Why, Account: r.Account,
			Waiting: r.Waiting, Ready: r.Ready, Depth: r.Depth, Repeat: r.Repeat, By: r.By}
		if a := r.Ask; a != nil {
			n.Ask = &proto.PlanAsk{N: a.N, State: a.State, At: a.At.Unix(), Working: a.Working, Filed: a.Filed}
		}
		ev.Rows = append(ev.Rows, n)
	}
	if in := s.Intake; in != nil {
		ev.Intake = &proto.PlanIntake{Messages: []proto.PlanMessage{}, Working: in.Working}
		for _, m := range in.Msgs {
			ev.Intake.Messages = append(ev.Intake.Messages, proto.PlanMessage{At: m.At.Unix(), Text: m.Text})
		}
	}
	if sum := h.PlanSummaryNow(); sum.Running || sum.Text != "" || sum.Err != "" {
		ev.Summary = &proto.PlanSummary{Text: sum.Text, Made: sum.Made(), Running: sum.Running, Error: sum.Err,
			Tokens: sum.Tokens, Cost: sum.Cost, Currency: sum.Currency}
		if !sum.At.IsZero() {
			ev.Summary.At = sum.At.Unix()
		}
	}
	return ev
}

// journalEvent is the session's /journal for a client's screen
// (dun/journal.go).
func journalEvent(h *harness.Harness) *proto.Journal {
	ev := &proto.Journal{Entries: []proto.JournalItem{}}
	for i, e := range h.Journal() {
		ev.Entries = append(ev.Entries, proto.JournalItem{N: i + 1, At: e.At.Unix(), Kind: e.Kind, Title: e.Title,
			Tokens: e.Tokens, Line: e.Line(i + 1), Text: e.Body(), Path: e.Path, Outcome: e.AskOutcome()})
	}
	return ev
}
