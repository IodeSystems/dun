package acp

import (
	"encoding/json"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// planSent waits for the plan the editor was sent: plan or plan_update.
func planSent(t *testing.T, r *rig) (*sdk.SessionUpdatePlan, *sdk.SessionPlanUpdate) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, u := range r.ed.take() {
			if u.Plan != nil || u.PlanUpdate != nil {
				return u.Plan, u.PlanUpdate
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no plan sent")
	return nil, nil
}

// An editor that advertises plan (or Air's planFile) gets plan_update
// (items, dun's one plan id) instead of plan; one that does not gets plan.
func TestPlanUpdate(t *testing.T) {
	plan := &proto.Plan{Rows: []proto.PlanNode{{ID: "1", Title: "edit", Status: "doing"}, {ID: "2", Title: "test", Status: "todo"}}}
	for _, tc := range []struct {
		name   string
		caps   sdk.ClientCapabilities
		update bool
	}{
		{"baseline", sdk.ClientCapabilities{}, false},
		{"plan capability", sdk.ClientCapabilities{Plan: &sdk.PlanCapabilities{}}, true},
		{"air planFile", sdk.ClientCapabilities{Meta: sdk.Meta{"jetbrains": json.RawMessage(`{"air":{"version":1,"capabilities":["planFile"]}}`)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRigCaps(t, tc.caps)
			sid := r.newSession()
			r.ed.take()
			r.d.emit(plan)
			base, upd := planSent(t, r)
			if !tc.update {
				if base == nil || len(base.Entries) != 2 {
					t.Fatalf("want plan, got plan %+v plan_update %+v", base, upd)
				}
				return
			}
			if upd == nil || upd.Plan.Items == nil {
				t.Fatalf("want plan_update items, got plan %+v plan_update %+v", base, upd)
			}
			it := upd.Plan.Items
			if string(it.PlanId) != planID(string(sid)) || len(it.Entries) != 2 || it.Entries[0].Status != sdk.PlanEntryStatusInProgress {
				t.Fatalf("plan_update %+v", it)
			}
			b, _ := json.Marshal(sdk.SessionUpdate{PlanUpdate: upd})
			var w struct {
				SessionUpdate string `json:"sessionUpdate"`
				Plan          struct {
					Type   string `json:"type"`
					PlanID string `json:"planId"`
				} `json:"plan"`
			}
			if json.Unmarshal(b, &w) != nil || w.SessionUpdate != "plan_update" || w.Plan.Type != "items" || w.Plan.PlanID != planID(string(sid)) {
				t.Errorf("wire %s", b)
			}
		})
	}
}
