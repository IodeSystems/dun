package acp

import (
	"context"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// plans collects the plan updates among the editor's updates, as entry counts.
func plans(ups []sdk.SessionUpdate) (n []int, ids []string) {
	for _, u := range ups {
		switch {
		case u.Plan != nil:
			n = append(n, len(u.Plan.Entries))
		case u.PlanUpdate != nil && u.PlanUpdate.Plan.Items != nil:
			n = append(n, len(u.PlanUpdate.Plan.Items.Entries))
			ids = append(ids, string(u.PlanUpdate.Plan.Items.PlanId))
		}
	}
	return n, ids
}

// A plan persists: the last one is sent again at the start of the next
// prompt and at its end, under the session's own plan id.
func TestPlanResentPerPrompt(t *testing.T) {
	r := newRigCaps(t, sdk.ClientCapabilities{Plan: &sdk.PlanCapabilities{}})
	sid := r.newSession()
	r.ed.take()
	r.d.emit(&proto.Plan{Rows: []proto.PlanNode{{ID: "1", Title: "edit", Status: "doing"}, {ID: "2", Title: "test", Status: "todo"}}})
	planSent(t, r)
	ch := r.prompt(sid, "go on")
	r.d.next()
	_, upd := planSent(t, r)
	if upd == nil || len(upd.Plan.Items.Entries) != 2 || string(upd.Plan.Items.PlanId) != "dun-"+string(sid) {
		t.Fatalf("at the prompt's start: %+v", upd)
	}
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "go on"}, &proto.Turn{Kind: "user"}, &proto.Done{})
	waitPrompt(t, ch)
	if n, _ := plans(r.ed.take()); len(n) != 1 || n[0] != 2 {
		t.Fatalf("at the turn's end: %v", n)
	}
}

// session/load sends the plan the snapshot carries.
func TestPlanLoad(t *testing.T) {
	r := newRigCaps(t, sdk.ClientCapabilities{Plan: &sdk.PlanCapabilities{}})
	r.d.state = []any{&proto.Plan{Rows: []proto.PlanNode{{ID: "1", Title: "edit", Status: "todo"}}}}
	if _, err := r.cc.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: "s1", Cwd: r.cwd, McpServers: []sdk.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	n, ids := plans(r.ed.take())
	if len(n) != 1 || n[0] != 1 || ids[0] != "dun-s1" {
		t.Fatalf("plans %v %v", n, ids)
	}
}
