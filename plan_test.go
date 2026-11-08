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
		// The person's change is an event the model is told, beside the
		// board (docs/attention.md): what they changed, and for a drop what
		// waits on it, which the board cannot show.
		{"edit 1 title Parse it", "changed node 1's title to: Parse it", false},
		{"edit 1 goal Read the file", "goal to: Read the file", false},
		{"drop 1 wrong way", "dropped node 1 (wrong way). 1 node(s) wait on it", false},
		// A setting, not a request: no echo (USER 2026-11-07). The plan
		// gate's refusal says what plan mode means when a call hits it.
		{"mode on", "", false},
		{"approve", "approved the plan", true},
	} {
		h.queue = nil
		if _, err := h.PlanCmd(ctx, tc.cmd); err != nil {
			t.Fatalf("%s: %v", tc.cmd, err)
		}
		q := queuedTexts(h)
		if tc.want == "" {
			if len(q) != 0 {
				t.Errorf("%s: echoed %v", tc.cmd, q)
			}
			continue
		}
		said := false
		for _, x := range q {
			if strings.HasPrefix(x, "[control/plan] ") && strings.Contains(x, tc.want) {
				said = true
			}
		}
		if !said {
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

// planIDs maps each node's title to its id.
func planIDs(h *Harness) map[string]string {
	ids := map[string]string{}
	for _, r := range h.Plan().Rows {
		ids[r.Title] = r.ID
	}
	return ids
}

// offersTool says whether h's session offers the tool name.
func offersTool(h *Harness, name string) bool {
	for _, d := range h.Session.Tools {
		if d.Function.Name == name {
			return true
		}
	}
	return false
}

// sentHas says whether any request r got carries text.
func sentHas(r *scriptRunner, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, msgs := range r.sent {
		for _, m := range msgs {
			if strings.Contains(m.Content, text) {
				return true
			}
		}
	}
	return false
}

// Breadth first (USER 2026-11-06): only a ready node goes to a child, and
// only that node. It leaves the main agent's ready list; a node that cannot
// be given starts no child. A run that ends owing a publish is reminded once,
// and its report says what it still owes.
func TestPlan_ChildIsGivenOnlyAReadyNode(t *testing.T) {
	r := &scriptRunner{}
	h := startParentOf(t, r)
	planCall(t, h, `{"action":"add","title":"build","done_when":"x"}`)
	planCall(t, h, `{"action":"add","title":"test","done_when":"x","needs":["1"]}`)
	planCall(t, h, `{"action":"add","title":"other","done_when":"x"}`)
	ids := planIDs(h)

	for node, want := range map[string]string{"nope": "no plan node nope", ids["test"]: "is not ready"} {
		if out := spawnTool(context.Background(), h, "x", "", "", "", "", node, false); !strings.Contains(out, want) ||
			!strings.Contains(out, "No agent was started") || len(h.agentList()) != 0 {
			t.Fatalf("node %s: %q, %d agents", node, out, len(h.agentList()))
		}
	}
	out := spawnTool(context.Background(), h, "build it", "", "m", "", "", ids["build"], false)
	if !strings.Contains(out, "owns plan node "+ids["build"]) {
		t.Fatalf("spawn: %q", out)
	}
	sa := h.agentList()[0]
	owner := planOwner(sa.num)
	for _, row := range h.Plan().Rows {
		want := ""
		if row.Title == "build" {
			want = owner
		}
		if row.Owner != want {
			t.Errorf("%s owned by %q, want %q", row.Title, row.Owner, want)
		}
	}
	g, _ := h.planNow().store.View(context.Background())
	if ready := g.Ready(""); len(ready) != 1 || ready[0].Title != "other" {
		t.Errorf("the main agent's ready list: %v", ready)
	}
	if out := spawnTool(context.Background(), h, "x", "", "", "", "", ids["build"], false); !strings.Contains(out, "already belongs to "+owner) {
		t.Errorf("a node already owned: %q", out)
	}

	waitIdle(t, sa)
	if !sentHas(r, "You own plan node "+ids["build"]) {
		t.Error("the child was not briefed on its node")
	}
	if !sentHas(r, "you have not published plan node(s) "+ids["build"]) {
		t.Error("a run that ended owing was not reminded")
	}
	if rep := sa.report(); !strings.Contains(rep, "Still owes a publish for plan node(s) "+ids["build"]) {
		t.Errorf("the report does not say what is owed: %q", rep)
	}
	if n := r.calls(); n != 2 {
		t.Errorf("model rounds: %d, want 2 (the task, one reminder)", n)
	}

	sa.quit()
	for _, row := range h.Plan().Rows {
		if row.Owner != "" {
			t.Errorf("%s: an unfinished node goes back to the main agent, owned by %q", row.Title, row.Owner)
		}
	}
}

// A child publishes with its plan tool, over its parent's graph, changing only
// its own nodes; the main agent is woken with what became ready, and gives it
// to the same idle child (jobs node). A finished node keeps who did it.
func TestPlan_ChildPublishesAndIsGivenTheNext(t *testing.T) {
	r := &scriptRunner{}
	h := startParentOf(t, r)
	planCall(t, h, `{"action":"add","title":"build","done_when":"x"}`)
	planCall(t, h, `{"action":"add","title":"test","done_when":"x","needs":["1"]}`)
	planCall(t, h, `{"action":"add","title":"main's","done_when":"x"}`)
	ids := planIDs(h)
	spawnTool(context.Background(), h, "build it", "", "m", "", "", ids["build"], false)
	sa := h.agentList()[0]
	waitIdle(t, sa)
	child := sa.harness()
	if !offersTool(child, taskgraph.ToolName) {
		t.Fatal("a child that owns a node has no plan tool")
	}
	_, handle := child.childPlanTool()
	ctx := context.Background()
	if out := handle(ctx, `{"action":"update","id":"`+ids["main's"]+`","goal":"hijacked"}`); !strings.Contains(strings.ToLower(out), "not") {
		t.Errorf("another's node must be refused: %q", out)
	}
	h.notes()
	handle(ctx, `{"action":"update","id":"`+ids["build"]+`","status":"doing"}`)
	if out := handle(ctx, `{"action":"done","id":"`+ids["build"]+`","account":"built"}`); strings.Contains(out, "cannot") {
		t.Fatalf("publish: %s", out)
	}
	n := h.notes()
	if len(n) == 0 || !strings.Contains(n[len(n)-1], "published plan node(s) "+ids["build"]+" done") ||
		!strings.Contains(n[len(n)-1], "Now ready: "+ids["test"]) {
		t.Fatalf("the main agent heard %q", n)
	}

	if out := agentOp(h, sa.num, "run go test", ids["main's"]+"x", false, false); !strings.Contains(out, "ERROR") {
		t.Errorf("an unknown node: %q", out)
	}
	if out := agentOp(h, sa.num, "run go test", ids["test"], false, false); !strings.Contains(out, "Told agent") {
		t.Fatalf("give the next node: %q", out)
	}
	waitIdle(t, sa)
	if !sentHas(r, "Work this plan node next.") || !sentHas(r, "run go test") {
		t.Error("the child was not briefed on the node it was given")
	}
	sa.quit()
	for _, row := range h.Plan().Rows {
		want := ""
		if row.Title == "build" {
			want = planOwner(sa.num)
		}
		if row.Owner != want {
			t.Errorf("%s owned by %q, want %q", row.Title, row.Owner, want)
		}
	}
}

// A change someone else makes to a child's node is told to the child. The
// person's words about a node go to its owner; a node whose owner is gone
// goes to the main agent.
func TestPlan_OwnersHearOfTheirNodes(t *testing.T) {
	h := startParentOf(t, &scriptRunner{})
	planCall(t, h, `{"action":"add","title":"decide db","done_when":"x"}`)
	planCall(t, h, `{"action":"add","title":"main's","done_when":"x"}`)
	ids := planIDs(h)
	spawnTool(context.Background(), h, "decide", "", "", "", "", ids["decide db"], false)
	sa := h.agentList()[0]
	waitIdle(t, sa)
	child := sa.harness()
	ctx := context.Background()

	child.notes()
	if _, err := h.PlanCmd(ctx, "edit "+ids["decide db"]+" goal postgres"); err != nil {
		t.Fatal(err)
	}
	if n := child.notes(); len(n) != 1 || !strings.Contains(n[0], "the user changed plan node(s) "+ids["decide db"]) {
		t.Errorf("the child heard %q", n)
	}

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

	// Its owner gone (dismissed after it finished the node): to the main agent.
	_, handle := child.childPlanTool()
	handle(ctx, `{"action":"done","id":"`+ids["decide db"]+`","account":"sqlite"}`)
	sa.quit()
	h.notes()
	if msg, err := h.PlanCmd(ctx, "tell "+ids["decide db"]+" reopen it"); err != nil || !strings.Contains(msg, "main agent") {
		t.Errorf("a gone owner's node: %q %v", msg, err)
	}
}

// A node owned by a child the session has no record of (a crash between the
// give and the child's first save) goes back to the main agent on resume.
func TestPlan_OrphanedNodesAreReleased(t *testing.T) {
	h := startParentOf(t, &scriptRunner{})
	planCall(t, h, `{"action":"add","title":"a","done_when":"x"}`)
	id := planIDs(h)["a"]
	_ = h.planNow().store.Update(context.Background(), func(g *taskgraph.Graph) error {
		g.Get(id).Owner = planOwner(7)
		return nil
	})
	h.restoreAgents()
	if g, _ := h.planNow().store.View(context.Background()); g.Get(id).Owner != "" {
		t.Errorf("an orphan's node: owner %q", g.Get(id).Owner)
	}
}

// --non-interactive: no one approves or edits the plan, so the description
// says neither; interactive keeps both. Both say what the plan is for, and a
// session gets the same description every time (the prompt cache).
func TestPlan_DescriptionByMode(t *testing.T) {
	h := planHarness(t, false)
	d, _ := h.planTool()
	inter := d.Function.Description
	for _, want := range []string{"The plan is what survives", "done_when", "/plan approve", "plan mode", "edit the plan"} {
		if !strings.Contains(inter, want) {
			t.Errorf("interactive description lacks %q: %s", want, inter)
		}
	}
	h.cfg.NonInteractive = true
	d, _ = h.planTool()
	non := d.Function.Description
	if !strings.Contains(non, "The plan is what survives") || !strings.Contains(non, "done_when") {
		t.Errorf("non-interactive description lacks the purpose or the mechanics: %s", non)
	}
	for _, not := range []string{"approve", "plan mode", "user", "User"} {
		if strings.Contains(non, not) {
			t.Errorf("non-interactive description has %q: %s", not, non)
		}
	}
	d2, _ := h.planTool()
	if d2.Function.Description != non {
		t.Error("the description changed between two builds of one session")
	}
}
