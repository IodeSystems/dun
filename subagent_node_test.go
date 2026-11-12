package dun

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// USER 2026-11-10: "Subagents should only be launchable via plan nodes!" The
// agent tool starts a child only on a ready plan node; every other call is
// refused, says how to fix it, and changes nothing.

// planRowsSig is the plan as a string, to show a refused call left it alone.
func planRowsSig(h *Harness) string {
	var b strings.Builder
	for _, r := range h.Plan().Rows {
		fmt.Fprintf(&b, "%s|%s|%s|%s;", r.ID, r.Title, r.Status, r.Owner)
	}
	return b.String()
}

func TestAgentNode_ToolSchemaHasNoPrompt(t *testing.T) {
	props := agentToolDef("", "", "", nil).Function.Parameters.(map[string]any)["properties"].(map[string]any)
	if _, ok := props["prompt"]; ok {
		t.Error("the agent tool still offers prompt")
	}
	if _, ok := props["node"]; !ok {
		t.Error("the agent tool has no node")
	}
}

func TestAgentNode_RefusedWithoutAUsableNode(t *testing.T) {
	h := startParentOf(t, &scriptRunner{})
	planCall(t, h, "plan_add", `{"title":"finished","done_when":"x"}`)
	ids := planIDs(h)
	planCall(t, h, "plan_update", `{"id":"`+ids["finished"]+`","status":"doing"}`)
	if out := planCall(t, h, "plan_close", `{"outcome":"done","id":"`+ids["finished"]+`","account":"did it"}`); strings.Contains(out, "cannot") {
		t.Fatalf("done: %s", out)
	}
	before := planRowsSig(h)

	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"no node":       {map[string]any{}, agentNoNode},
		"prompt only":   {map[string]any{"prompt": "read x"}, agentNoPrompt},
		"prompt + node": {map[string]any{"prompt": "read x", "node": ids["finished"]}, agentNoPrompt},
		"unknown node": {map[string]any{"node": "nope"},
			"ERROR: no plan node nope. " + planFix + " No agent was started."},
		"done node": {map[string]any{"node": ids["finished"]},
			"ERROR: node " + ids["finished"] + " is done. " + planFix + " No agent was started."},
	} {
		// A prefix: an aside (the plan nudge) may ride the result.
		if out := callTool(t, h, "agent", tc.args); !strings.HasPrefix(out, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", name, out, tc.want)
		}
	}
	if n := len(h.agentList()); n != 0 {
		t.Errorf("%d agents started by refused calls", n)
	}
	if after := planRowsSig(h); after != before {
		t.Errorf("a refused call changed the plan:\n%s\n%s", before, after)
	}
	if !strings.Contains(agentNoNode, `Add a node with plan_add(`) || !strings.Contains(agentNoNode, `then call agent({node:"<id>"})`) {
		t.Errorf("the refusal does not say the fix: %q", agentNoNode)
	}
}

// One node, one child: a second child on a node that has a live one is refused.
func TestAgentNode_OneChildPerNode(t *testing.T) {
	h := startParentOf(t, &scriptRunner{})
	planCall(t, h, "plan_add", `{"title":"scan","done_when":"x"}`)
	id := planIDs(h)["scan"]
	if out := callTool(t, h, "agent", map[string]any{"node": id}); !strings.HasPrefix(out, "Started agent #1") {
		t.Fatalf("first child: %q", out)
	}
	sa := h.agentList()[0]
	waitIdle(t, sa)
	before := planRowsSig(h)
	// Another model each time: the loop guard refuses byte-identical calls.
	out := callTool(t, h, "agent", map[string]any{"node": id, "model": "m2"})
	if want := "ERROR: node " + id + " already has a child, agent #1; one child per node. Act on it with agent({id:1}), " +
		"or add another node with plan. No agent was started."; !strings.HasPrefix(out, want) {
		t.Errorf("second child: %q", out)
	}
	if n := len(h.agentList()); n != 1 {
		t.Errorf("%d agents, want 1", n)
	}
	if after := planRowsSig(h); after != before {
		t.Errorf("a refused call changed the plan:\n%s\n%s", before, after)
	}
	// Dismissed, the node goes back, and a new child may have it.
	sa.quit()
	if out := callTool(t, h, "agent", map[string]any{"node": id, "model": "m3"}); !strings.HasPrefix(out, "Started agent #2") {
		t.Errorf("after dismiss: %q", out)
	}
}

// The child's brief is the node's title, details and done_when, and it
// publishes onto the node.
func TestAgentNode_BriefsFromTheNodeAndReportsOntoIt(t *testing.T) {
	r := &scriptRunner{}
	h := startParentOf(t, r)
	planCall(t, h, "plan_add", `{"title":"read the log","details":"find the first panic in build.log","done_when":"the panic line is quoted"}`)
	id := planIDs(h)["read the log"]
	out := callTool(t, h, "agent", map[string]any{"node": id, "model": "m"})
	if !strings.HasPrefix(out, "Started agent #1") || !strings.Contains(out, "owns plan node "+id) {
		t.Fatalf("spawn: %q", out)
	}
	sa := h.agentList()[0]
	waitIdle(t, sa)
	brief := "You own plan node " + id + ": read the log. Details: find the first panic in build.log. Done when: the panic line is quoted."
	if !sentHas(r, brief) {
		t.Error("the child was not briefed from the node")
	}
	if !strings.HasPrefix(sa.prompt, "Work this plan node.") || !strings.Contains(sa.prompt, brief) {
		t.Errorf("kept prompt: %q", sa.prompt)
	}

	handle := sa.harness().childPlanTool().Dispatch
	h.notes()
	handle(context.Background(), "plan_update", `{"id":"`+id+`","status":"doing"}`)
	handle(context.Background(), "plan_close", `{"outcome":"done","id":"`+id+`","account":"panic: nil map at main.go:12"}`)
	for _, row := range h.Plan().Rows {
		if row.ID == id && (row.Status != "done" || row.Account != "panic: nil map at main.go:12" || row.Owner != planOwner(sa.num)) {
			t.Errorf("the node after the publish: %+v", row)
		}
	}
	if n := h.notes(); len(n) == 0 || !strings.Contains(n[len(n)-1], "agent #1 published plan node(s) "+id+" done") {
		t.Errorf("the main agent heard %q", n)
	}
	// Done, the node takes no new child.
	if out := callTool(t, h, "agent", map[string]any{"node": id}); !strings.Contains(out, "is done. "+planFix) {
		t.Errorf("a published node: %q", out)
	}
}

// With the plan tool switched off there is no node to name: the refusal says so.
func TestAgentNode_PlanToolOff(t *testing.T) {
	h := startParentOf(t, &scriptRunner{})
	names := []string{"agent"}
	h.toolNames.Store(&names)
	if out := spawnTool(context.Background(), h, "", "", "", "", "", false); out != agentNoPlan {
		t.Errorf("%q", out)
	}
}
