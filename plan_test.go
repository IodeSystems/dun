package dun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/taskgraph"
)

func planHarness(t *testing.T, file bool) *Harness {
	t.Helper()
	h := newNoteHarness(t)
	if file {
		h.cfg.SessionFile = filepath.Join(t.TempDir(), "s1.jsonl")
	}
	return h
}

func planCall(t *testing.T, h *Harness, args string) string {
	t.Helper()
	_, handle := h.planTool()
	return handle(context.Background(), args)
}

func queuedTexts(h *Harness) []string {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	var out []string
	for _, q := range h.queue {
		out = append(out, q.text)
	}
	return out
}

func TestPlan_PersistsBesideTheSession(t *testing.T) {
	h := planHarness(t, true)
	planCall(t, h, `{"action":"add","title":"a","done_when":"x"}`)
	if _, err := h.PlanCmd(context.Background(), "mode on"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(PlanFile(h.cfg.SessionFile)); err != nil {
		t.Fatalf("no sidecar: %v", err)
	}
	if !strings.HasSuffix(PlanFile("/x/s1.jsonl"), "/x/s1.plan.json") {
		t.Errorf("PlanFile: %s", PlanFile("/x/s1.jsonl"))
	}
	// A new process: the plan and the mode come back.
	h2 := newNoteHarness(t)
	h2.cfg.SessionFile = h.cfg.SessionFile
	s := h2.Plan()
	if len(s.Rows) != 1 || !s.Mode || s.Approved {
		t.Errorf("resumed: %+v", s)
	}
	// The session's file changed (a fork): its own plan, read again.
	h2.cfg.SessionFile = filepath.Join(t.TempDir(), "s2.jsonl")
	if s := h2.Plan(); len(s.Rows) != 0 || s.Mode {
		t.Errorf("another session: %+v", s)
	}
}

func TestPlan_InMemoryWithoutAFile(t *testing.T) {
	h := planHarness(t, false)
	planCall(t, h, `{"action":"add","title":"a","done_when":"x"}`)
	if len(h.Plan().Rows) != 1 {
		t.Error("plan lost")
	}
	if _, err := h.PlanCmd(context.Background(), "approve"); err != nil {
		t.Error(err)
	}
}

func TestPlan_ToolIsAGroupAndNamesNeitherModeNorFilesInTheGraph(t *testing.T) {
	if g := groupOfTool("plan"); g != "plan" {
		t.Errorf("group of plan: %s", g)
	}
	h := planHarness(t, false)
	d, _ := h.planTool()
	if !strings.Contains(d.Function.Description, "plan mode") || !strings.Contains(d.Function.Description, "list files") {
		t.Errorf("the host's note is missing: %s", d.Function.Description)
	}
}

func TestPlan_ChangesWorkspace(t *testing.T) {
	for name, want := range map[string]bool{
		"exec": true, "file_edit": true, "file_write": true, "file_delete": true, "file_move": true,
		"agent": true, "mount": true, "loop_ctl": true, "dun_msg": true,
		"file_read": false, "file_grep": false, "file_list": false, "plan": false, "ask_user": false, "session": false,
		"read_saved": false, "recap": false, "skill": false, "memory": false, "jobs": false, "tell_parent": false, "dun_list": false,
	} {
		if got := changesWorkspace(name, ""); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	for _, tc := range []struct {
		name, server string
		want         bool
	}{
		{"search", ServerDocs, false}, {"rename_symbol", ServerCode, true}, {"hover", ServerCode, false},
		{"run", ServerShell, true}, {"anything", ServerShell, true},
		{"git__commit", "git", true}, {"git__log", "git", false}, {"srv__apply_patch", "srv", true},
		{"srv__prefix", "srv", false}, // "fix" inside a word is not a verb
	} {
		if got := changesWorkspace(tc.name, tc.server); got != tc.want {
			t.Errorf("%s/%s: %v, want %v", tc.server, tc.name, got, tc.want)
		}
	}
}

func TestPlan_GateRefusesUntilApproved(t *testing.T) {
	h := planHarness(t, false)
	inner := &counting{}
	d := withPlanGate(inner.dispatch, h, map[string]string{})
	run := func(tool string) string {
		out, err := d(context.Background(), call(tool, `{}`))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Off by default: nothing is refused.
	if out := run("exec"); out != "ok" {
		t.Errorf("mode off: %s", out)
	}
	ctx := context.Background()
	h.PlanCmd(ctx, "mode on")
	for _, tool := range []string{"exec", "file_write", "file_edit"} {
		if out := run(tool); !strings.Contains(out, "plan mode is on and the plan is not approved") || !strings.Contains(out, "/plan approve") {
			t.Errorf("%s: %s", tool, out)
		}
	}
	for _, tool := range []string{"file_read", "plan", "ask_user"} {
		if out := run(tool); out != "ok" {
			t.Errorf("%s was refused: %s", tool, out)
		}
	}
	if len(inner.calls) != 4 { // exec while off, then the three allowed
		t.Errorf("reached the tools: %v", inner.calls)
	}
	// An empty plan cannot be approved; a plan can; an edit keeps it.
	if _, err := h.PlanCmd(ctx, "approve"); err == nil {
		t.Error("approved an empty plan")
	}
	planCall(t, h, `{"action":"add","title":"a","done_when":"x"}`)
	if _, err := h.PlanCmd(ctx, "approve"); err != nil {
		t.Fatal(err)
	}
	if out := run("exec"); out != "ok" {
		t.Errorf("approved: %s", out)
	}
	planCall(t, h, `{"action":"add","title":"b","done_when":"x"}`)
	if out := run("exec"); out != "ok" {
		t.Errorf("after a tool edit the approval was lost: %s", out)
	}
	// Mode off then on takes the approval back.
	h.PlanCmd(ctx, "mode off")
	h.PlanCmd(ctx, "mode on")
	if out := run("exec"); !strings.Contains(out, "refused") {
		t.Errorf("mode on again: %s", out)
	}
	// An MCP tool is looked up by server.
	d = withPlanGate(inner.dispatch, h, map[string]string{"srv__push": "srv"})
	if out, _ := d(ctx, call("srv__push", `{}`)); !strings.Contains(out, "refused") {
		t.Errorf("mcp: %s", out)
	}
}

func TestPlan_TurnLine(t *testing.T) {
	h := planHarness(t, false)
	h.planTurnLine()
	if q := queuedTexts(h); len(q) != 0 {
		t.Errorf("a line for an empty plan: %v", q)
	}
	planCall(t, h, `{"action":"add","title":"a","done_when":"x"}`)
	planCall(t, h, `{"action":"add","title":"b","done_when":"x","needs":["1"]}`)
	// Tool calls add nothing to the queue.
	if q := queuedTexts(h); len(q) != 0 {
		t.Errorf("a tool call queued a line: %v", q)
	}
	h.planTurnLine()
	q := queuedTexts(h)
	if len(q) != 1 || !strings.HasPrefix(q[0], "[plan] plan: 0 of 2 done") || !strings.Contains(q[0], "ready: 1 a") {
		t.Fatalf("line: %v", q)
	}
	// It is an aside: it never causes a turn.
	if h.Pending() != 0 {
		t.Error("the plan line woke the model")
	}
	// A turn begins: one line, read at the tail with the turn.
	h.queue = nil
	planCall(t, h, `{"action":"update","id":"1","status":"doing"}`)
	planCall(t, h, `{"action":"done","id":"1","account":"ok"}`)
	planCall(t, h, `{"action":"update","id":"2","status":"doing"}`)
	planCall(t, h, `{"action":"done","id":"2","account":"ok"}`)
	h.planTurnLine()
	if q := queuedTexts(h); len(q) != 0 {
		t.Errorf("a line with nothing open: %v", q)
	}
	// A child has none.
	h.parent = &Harness{}
	planCall(t, h, `{"action":"add","title":"c","done_when":"x"}`)
	h.planTurnLine()
	if q := queuedTexts(h); len(q) != 0 {
		t.Errorf("a child got a line: %v", q)
	}
}

func TestPlan_PersonsChangesReachTheModel(t *testing.T) {
	h := planHarness(t, false)
	ctx := context.Background()
	planCall(t, h, `{"action":"add","title":"a","done_when":"x"}`)
	planCall(t, h, `{"action":"add","title":"b","done_when":"x","needs":["1"]}`)
	changed := 0
	h.cfg.OnPlan = func() { changed++ }
	for _, tc := range []struct {
		cmd, want string
		wakes     bool
	}{
		{"edit 1 title Parse it", "changed node 1's title to: Parse it", false},
		{"edit 1 goal Read the file", "goal to: Read the file", false},
		{"drop 1 wrong way", "dropped node 1 (wrong way). 1 node(s) wait on it", false},
		{"mode on", "plan mode on", false},
		{"approve", "approved the plan", true},
	} {
		h.queue = nil
		if _, err := h.PlanCmd(ctx, tc.cmd); err != nil {
			t.Fatalf("%s: %v", tc.cmd, err)
		}
		q := queuedTexts(h)
		if len(q) != 1 || !strings.Contains(q[0], tc.want) || !strings.HasPrefix(q[0], "[control/plan] ") {
			t.Errorf("%s: %v", tc.cmd, q)
		}
		if (h.Pending() > 0) != tc.wakes {
			t.Errorf("%s: wakes %v", tc.cmd, !tc.wakes)
		}
	}
	if changed != 5 {
		t.Errorf("OnPlan fired %d times", changed)
	}
	for _, bad := range []string{"edit", "edit 1 title", "edit 1 colour red", "edit 9 title x", "drop", "drop 9", "mode maybe", "frob"} {
		h.queue = nil
		if _, err := h.PlanCmd(ctx, bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
		if q := queuedTexts(h); len(q) != 0 {
			t.Errorf("%q: a refused command told the model: %v", bad, q)
		}
	}
	// The tool's own changes tell the UI too, and the model nothing.
	h.queue, changed = nil, 0
	planCall(t, h, `{"action":"add","title":"c","done_when":"x"}`)
	if changed != 1 || len(queuedTexts(h)) != 0 {
		t.Errorf("tool change: %d, %v", changed, queuedTexts(h))
	}
}

func TestPlan_SnapshotIsTheTree(t *testing.T) {
	h := planHarness(t, false)
	planCall(t, h, `{"action":"add","title":"a","done_when":"x"}`)
	planCall(t, h, `{"action":"add","title":"b","done_when":"x"}`)
	planCall(t, h, `{"action":"add","title":"c","done_when":"x","needs":["1","2"]}`)
	s := h.Plan()
	var got []string
	for _, r := range s.Rows {
		g := r.ID
		if r.Repeat {
			g += "*"
		}
		if r.Ready {
			g += "!"
		}
		got = append(got, strings.Repeat(".", r.Depth)+g)
	}
	if strings.Join(got, " ") != "1! .3 2! .3*" || s.Total != 3 {
		t.Errorf("rows: %v", got)
	}
	if w := s.Rows[1].Waiting; len(w) != 2 {
		t.Errorf("waiting: %v", w)
	}
}

// The store is dun's (agentkit ships none): a failed save undoes the change
// and the tool says so.
func TestPlan_MemStoreFailedSaveUndoes(t *testing.T) {
	fail := false
	st := &planMemStore{save: func(taskgraph.Graph) error {
		if fail {
			return errors.New("disk full")
		}
		return nil
	}}
	_, handle := taskgraph.Tool(st, "")
	handle(context.Background(), `{"action":"add","title":"a","done_when":"x"}`)
	fail = true
	if out := handle(context.Background(), `{"action":"add","title":"b","done_when":"x"}`); !strings.Contains(out, "disk full") {
		t.Errorf("save failure: %s", out)
	}
	if g, _ := st.View(context.Background()); len(g.Nodes) != 1 {
		t.Errorf("nodes after a failed save: %d", len(g.Nodes))
	}
}

// The model's own change to its own node is told to no one: it already has
// the tool's answer.
func TestPlan_ModelsOwnChangeIsNotAnAside(t *testing.T) {
	h := planHarness(t, false)
	planCall(t, h, `{"action":"add","title":"a","done_when":"x"}`)
	if q := queuedTexts(h); len(q) != 0 {
		t.Errorf("asides: %q", q)
	}
}

// A child spawned for a node owns it and what needs it: they leave the main
// agent's ready list, show the owner, and come back when it is dismissed
// unfinished. A node that cannot be given starts no child.
func TestPlan_ChildOwnsItsNode(t *testing.T) {
	h := startParentOf(t, &scriptRunner{rounds: []roundFn{textRound("done"), textRound("done")}})
	planCall(t, h, `{"action":"add","title":"build","done_when":"x"}`)
	planCall(t, h, `{"action":"add","title":"test","done_when":"x","needs":["1"]}`)
	planCall(t, h, `{"action":"add","title":"other","done_when":"x"}`)
	ids := map[string]string{}
	for _, r := range h.Plan().Rows {
		ids[r.Title] = r.ID
	}

	if out := spawnTool(context.Background(), h, "x", "", "", "", "", "nope", false); !strings.Contains(out, "no plan node nope") ||
		!strings.Contains(out, "No agent was started") || len(h.agentList()) != 0 {
		t.Fatalf("a bad node: %q, %d agents", out, len(h.agentList()))
	}
	out := spawnTool(context.Background(), h, "build it", "", "", "", "", ids["build"], false)
	if !strings.Contains(out, "owns plan node "+ids["build"]) {
		t.Fatalf("spawn: %q", out)
	}
	sa := h.agentList()[0]
	owner := planOwner(sa.num)
	for _, r := range h.Plan().Rows {
		want := ""
		if r.Title != "other" {
			want = owner
		}
		if r.Owner != want {
			t.Errorf("%s owned by %q, want %q", r.Title, r.Owner, want)
		}
	}
	g, _ := h.planNow().store.View(context.Background())
	if ready := g.Ready(""); len(ready) != 1 || ready[0].Title != "other" {
		t.Errorf("the main agent's ready list: %v", ready)
	}
	if out := spawnTool(context.Background(), h, "x", "", "", "", "", ids["test"], false); !strings.Contains(out, "already belongs to "+owner) {
		t.Errorf("a node already owned: %q", out)
	}

	planCall(t, h, `{"action":"update","id":"`+ids["build"]+`","status":"doing"}`)
	if out := planCall(t, h, `{"action":"done","id":"`+ids["build"]+`","account":"built"}`); !strings.Contains(out, "done") || strings.Contains(out, "cannot") {
		t.Fatalf("done: %s", out)
	}
	waitIdle(t, sa)
	sa.quit()
	for _, r := range h.Plan().Rows {
		switch r.Title {
		case "build":
			if r.Owner != owner {
				t.Errorf("a finished node keeps who did it: %q", r.Owner)
			}
		case "test":
			if r.Owner != "" {
				t.Errorf("an unfinished node goes back to the main agent: %q", r.Owner)
			}
		}
	}
}

// A child that owns nodes has a plan tool over its parent's graph that changes
// only its own nodes, and what it changes is told to the main agent when it
// touches theirs. The person's words about a node go to its owner.
func TestPlan_ChildToolAndTell(t *testing.T) {
	h := startParentOf(t, &scriptRunner{rounds: []roundFn{textRound("done"), textRound("done"), textRound("done")}})
	planCall(t, h, `{"action":"add","title":"decide db","done_when":"x"}`)
	planCall(t, h, `{"action":"add","title":"main's","done_when":"x"}`)
	ids := map[string]string{}
	for _, r := range h.Plan().Rows {
		ids[r.Title] = r.ID
	}
	out := spawnTool(context.Background(), h, "decide", "", "", "", "", ids["decide db"], false)
	if !strings.Contains(out, "has a plan tool") {
		t.Fatalf("spawn: %q", out)
	}
	sa := h.agentList()[0]
	waitIdle(t, sa)
	sa.mu.Lock()
	child := sa.h
	sa.mu.Unlock()
	if child.cfg.PlanOwner != planOwner(sa.num) {
		t.Fatalf("the child is not an owner: %q", child.cfg.PlanOwner)
	}
	_, handle := child.childPlanTool()
	ctx := context.Background()
	if out := handle(ctx, `{"action":"update","id":"`+ids["decide db"]+`","goal":"postgres"}`); strings.Contains(out, "ERROR") || strings.Contains(out, "may not") {
		t.Errorf("its own node: %q", out)
	}
	if out := handle(ctx, `{"action":"update","id":"`+ids["main's"]+`","goal":"hijacked"}`); !strings.Contains(strings.ToLower(out), "not") {
		t.Errorf("another's node must be refused: %q", out)
	}
	for _, r := range h.Plan().Rows {
		if r.Title == "decide db" && r.Goal != "postgres" {
			t.Errorf("the child's change did not reach the parent's plan: %+v", r)
		}
		if r.Title == "main's" && r.Goal == "hijacked" {
			t.Errorf("the child changed a node it does not own")
		}
	}

	// The person's words: to the main agent for its node, to the child for its.
	h.notes()
	msg, err := h.PlanCmd(ctx, "tell "+ids["main's"]+" it's this way")
	if err != nil || !strings.Contains(msg, "main agent") {
		t.Fatalf("tell main: %q %v", msg, err)
	}
	if n := h.notes(); len(n) != 1 || !strings.Contains(n[0], "it's this way") || !strings.Contains(n[0], "Change the plan") {
		t.Errorf("main agent heard %q", n)
	}
	msg, err = h.PlanCmd(ctx, "tell "+ids["decide db"]+" use sqlite")
	if err != nil || !strings.Contains(msg, fmt.Sprintf("agent #%d", sa.num)) {
		t.Fatalf("tell child: %q %v", msg, err)
	}
	waitIdle(t, sa)
	if _, err := h.PlanCmd(ctx, "tell nope x"); err == nil {
		t.Error("an unknown node must be an error")
	}
}
