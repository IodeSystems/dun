package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"
)

// replyRunner answers every request with reply, and keeps the last request.
type replyRunner struct {
	reply string
	calls int
	last  []llm.Message
}

func (r *replyRunner) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.calls++
	r.last = append([]llm.Message(nil), msgs...)
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: r.reply}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

// asksDone reports whether the last request asked for the done part.
func (r *replyRunner) asksDone() bool {
	return len(r.last) > 0 && strings.Contains(r.last[len(r.last)-1].Content, `"done":[`)
}

// idleHarness is a session with a plan of agent nodes and one turn begun: a
// user message and, with tools, one tool call and its result.
func idleHarness(t *testing.T, reply string, nodes []string, tools bool) (*Harness, *replyRunner) {
	t.Helper()
	h := newNoteHarness(t)
	h.cfg.SessionFile = filepath.Join(t.TempDir(), "s.jsonl")
	r := &replyRunner{reply: reply}
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: r, System: "You are dun.",
		Build: func(ctx context.Context, sid, system string) ([]llm.Message, error) {
			return agent.DefaultContextBuilder(ctx, h.store, sid, system)
		}}
	ctx := context.Background()
	if len(nodes) > 0 {
		err := planStore{h}.Update(ctx, func(g *taskgraph.Graph) error {
			for _, title := range nodes {
				if _, err := g.Add(taskgraph.Node{Title: title, DoneWhen: "it is done"}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	h.planMarkTurn() // prepareTurn
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "u1", Kind: agent.KindUser, Content: "write the parser tests", CreatedAt: 1})
	if tools {
		_ = h.store.Append(ctx, "dun", agent.Entry{ID: "c1", Kind: agent.KindToolCall, ToolCallID: "x1", ToolName: "exec", Content: `{}`, CreatedAt: 2})
		_ = h.store.Append(ctx, "dun", agent.Entry{ID: "r1", Kind: agent.KindToolResult, ToolCallID: "x1", ToolName: "exec", Content: "ok", CreatedAt: 3})
	}
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "a1", Kind: agent.KindAssistant, Content: "Done.", CreatedAt: 4})
	return h, r
}

func planGraph(t *testing.T, h *Harness) taskgraph.Graph {
	t.Helper()
	g, err := planStore{h}.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

const sugsOnly = `"suggestions":[{"text":"run the tests","prob":0.5}]`

// The done part becomes done nodes marked as the suggester's, under the node
// the model named (or at the top), kept in the sidecar; the agent's node is
// untouched, and the suggestions still arrive.
func TestSuggestDone_AddsUnderParent(t *testing.T) {
	reply := `{` + sugsOnly + `,"done":[{"title":"Wrote the parser tests","under":"1"},{"title":"Fixed the lint","under":""},{"title":"Bumped the dep","under":"99"}]}`
	h, r := idleHarness(t, reply, []string{"Parse the file"}, true)
	g0 := planGraph(t, h)
	before := *g0.Get("1")
	sugs, err := h.Suggestions(context.Background())
	if err != nil || len(sugs) != 1 {
		t.Fatalf("suggestions: %v %v", sugs, err)
	}
	if !r.asksDone() || !strings.Contains(r.last[len(r.last)-1].Content, "1 [todo] Parse the file") {
		t.Fatalf("instruction lacks the done part or the plan: %q", r.last[len(r.last)-1].Content)
	}
	g := planGraph(t, h)
	if len(g.Nodes) != 4 {
		t.Fatalf("want 4 nodes, got %d: %s", len(g.Nodes), g.List())
	}
	if got := *g.Get("1"); !reflect.DeepEqual(got, before) {
		t.Errorf("agent's node changed: %+v → %+v", before, got)
	}
	for id, needs := range map[string][]string{"2": {"1"}, "3": nil, "4": nil} { // 99: no such node, top level
		n := g.Get(id)
		if n.Status != taskgraph.Done || !reflect.DeepEqual(n.Needs, needs) {
			t.Errorf("node %s: %s needs %v; want done needs %v", id, n.Status, n.Needs, needs)
		}
	}
	snap := h.Plan()
	for _, row := range snap.Rows {
		want := planBySuggest
		if row.ID == "1" {
			want = ""
		}
		if row.By != want {
			t.Errorf("row %s by %q, want %q", row.ID, row.By, want)
		}
	}
	out, _ := h.PlanCmd(context.Background(), "")
	if !strings.Contains(out, "2 [done] (auto) Wrote the parser tests") || strings.Contains(out, "1 [todo] (auto)") {
		t.Errorf("/plan text: %s", out)
	}
	b, err := os.ReadFile(PlanFile(h.cfg.SessionFile))
	if err != nil {
		t.Fatal(err)
	}
	var d planFileData
	if json.Unmarshal(b, &d) != nil || d.By["2"] != planBySuggest || d.By["1"] != "" {
		t.Errorf("sidecar by: %v", d.By)
	}
}

// Nothing is added when the turn changed the plan, when the plan is empty and
// the turn made no tool call, or when no turn ran in this process.
func TestSuggestDone_Skips(t *testing.T) {
	reply := `{` + sugsOnly + `,"done":[{"title":"Wrote the parser tests","under":""}]}`
	ctx := context.Background()

	h, r := idleHarness(t, reply, []string{"Parse the file"}, true)
	_ = planStore{h}.Update(ctx, func(g *taskgraph.Graph) error { return g.Update("1", taskgraph.Patch{Status: ptr(taskgraph.Doing)}) })
	if sugs, _ := h.Suggestions(ctx); len(sugs) != 1 || r.asksDone() || len(planGraph(t, h).Nodes) != 1 {
		t.Errorf("turn changed the plan: asked %v, %d nodes", r.asksDone(), len(planGraph(t, h).Nodes))
	}

	h, r = idleHarness(t, reply, nil, false)
	if sugs, _ := h.Suggestions(ctx); len(sugs) != 1 || r.asksDone() || len(planGraph(t, h).Nodes) != 0 {
		t.Errorf("empty plan, no tools: asked %v, %d nodes", r.asksDone(), len(planGraph(t, h).Nodes))
	}
	// The same with a tool call: added.
	h, r = idleHarness(t, reply, nil, true)
	if _, _ = h.Suggestions(ctx); !r.asksDone() || len(planGraph(t, h).Nodes) != 1 {
		t.Errorf("empty plan, a tool call: asked %v, %d nodes", r.asksDone(), len(planGraph(t, h).Nodes))
	}
	// A second call in the same idle: the first one's node moved the plan.
	if _, _ = h.Suggestions(ctx); r.asksDone() || len(planGraph(t, h).Nodes) != 1 {
		t.Errorf("second call: asked %v, %d nodes", r.asksDone(), len(planGraph(t, h).Nodes))
	}

	h, r = idleHarness(t, reply, []string{"Parse the file"}, true)
	h.planMu.Lock()
	h.planTurn = planTurnMark{} // a resumed session: no turn ran here
	h.planMu.Unlock()
	if _, _ = h.Suggestions(ctx); r.asksDone() || len(planGraph(t, h).Nodes) != 1 {
		t.Errorf("no turn: asked %v, %d nodes", r.asksDone(), len(planGraph(t, h).Nodes))
	}

	// A turn began while the call ran: what the model read is stale.
	h, _ = idleHarness(t, reply, []string{"Parse the file"}, true)
	m, _, ok := h.suggestDoneScope(ctx)
	h.planMarkTurn()
	if !ok || h.addSuggestDone(ctx, m, parseSuggestDone(reply)) != nil {
		t.Errorf("stale mark added nodes (scope ok %v)", ok)
	}
}

// A malformed done part adds nothing, and the suggestions still arrive.
func TestSuggestDone_MalformedDone(t *testing.T) {
	for _, reply := range []string{
		`{` + sugsOnly + `,"done":[{"title":"x",]}`,
		`{` + sugsOnly + `,"done":"wrote it"}`,
		`{` + sugsOnly + `,"done":[{"title":7}]}`,
	} {
		h, _ := idleHarness(t, reply, []string{"Parse the file"}, true)
		sugs, err := h.Suggestions(context.Background())
		if err != nil || len(sugs) != 1 || sugs[0].Text != "run the tests" {
			t.Errorf("%s: suggestions %v %v", reply, sugs, err)
		}
		if n := len(planGraph(t, h).Nodes); n != 1 {
			t.Errorf("%s: %d nodes", reply, n)
		}
	}
}

// At most three, titles cut short, none the plan (or the reply) already has.
func TestSuggestDone_CapAndDedupe(t *testing.T) {
	long := strings.Repeat("very long title ", 10)
	reply := `{` + sugsOnly + `,"done":[{"title":"parse THE file"},{"title":"Wrote tests"},{"title":"wrote  tests"},` +
		`{"title":"` + long + `"},{"title":"Fixed lint","under":1},{"title":"Fourth new one"}]}`
	h, _ := idleHarness(t, reply, []string{"Parse the file"}, true)
	_, _ = h.Suggestions(context.Background())
	g := planGraph(t, h)
	var titles []string
	for _, n := range g.Nodes[1:] {
		titles = append(titles, n.Title)
	}
	if len(titles) != 3 || titles[0] != "Wrote tests" || titles[2] != "Fixed lint" {
		t.Fatalf("added %q", titles)
	}
	if n := len([]rune(titles[1])); n > suggestDoneTitleMax || !strings.HasSuffix(titles[1], "…") {
		t.Errorf("long title not cut: %d runes %q", n, titles[1])
	}
	if !reflect.DeepEqual(g.Get("4").Needs, []string{"1"}) {
		t.Errorf("numeric under: needs %v", g.Get("4").Needs)
	}
}

// The agent may edit a suggester's node later, like any other.
func TestSuggestDone_AgentMayEdit(t *testing.T) {
	reply := `{` + sugsOnly + `,"done":[{"title":"Wrote tests"}]}`
	h, _ := idleHarness(t, reply, []string{"Parse the file"}, true)
	_, _ = h.Suggestions(context.Background())
	tg := h.planTools()
	out := tg.Dispatch(context.Background(), taskgraph.ToolUpdate, `{"id":"2","title":"Wrote the parser tests"}`)
	g := planGraph(t, h)
	if strings.HasPrefix(out, "ERROR") || g.Get("2").Title != "Wrote the parser tests" {
		t.Fatalf("agent edit: %q", out)
	}
}
