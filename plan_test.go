package dun

import (
	"context"
	"errors"
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
