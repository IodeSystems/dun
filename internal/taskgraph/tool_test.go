package taskgraph

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// mine lets an actor change a node it owns, and the main one ("") any.
func mine(actor string, n *Node) bool { return actor == "" || n.Owner == actor }

func call(st Store, actor string, a map[string]any) string {
	_, h := Tool(st, actor, WithMayChange(mine))
	b, _ := json.Marshal(a)
	return h(context.Background(), string(b))
}

func view(t *testing.T, st Store) Graph {
	t.Helper()
	g, err := st.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestToolWorkflow(t *testing.T) {
	st := NewMemStore(Graph{}, nil)
	out := call(st, "", map[string]any{"action": "add", "title": "parse", "done_when": "tests pass"})
	if !strings.Contains(out, "1 [todo] parse") || !strings.Contains(out, "ready: 1 parse") {
		t.Errorf("add: %s", out)
	}
	out = call(st, "", map[string]any{"action": "add", "title": "emit", "done_when": "x", "needs": []string{"1"}, "files": []string{"emit.go"}})
	if !strings.Contains(out, "2 [todo] emit") || !strings.Contains(out, "ready: 1 parse") {
		t.Errorf("add 2: %s", out)
	}
	if out = call(st, "", map[string]any{"action": "update", "id": "2", "status": "doing"}); !strings.Contains(out, "plan: node 2 cannot start") {
		t.Errorf("start waiting: %s", out)
	}
	call(st, "", map[string]any{"action": "update", "id": "1", "status": "doing"})
	if out = call(st, "", map[string]any{"action": "done", "id": "1"}); !strings.Contains(out, "how done_when was met") {
		t.Errorf("done with no account: %s", out)
	}
	out = call(st, "", map[string]any{"action": "done", "id": "1", "account": "go test ok"})
	if !strings.Contains(out, "1 [done] parse") || !strings.Contains(out, "ready: 2 emit") {
		t.Errorf("done: %s", out)
	}
	if out = call(st, "", map[string]any{"action": "show", "id": "1"}); !strings.Contains(out, "account: go test ok") {
		t.Errorf("show: %s", out)
	}
	out = call(st, "", map[string]any{"action": "show"})
	if !strings.HasPrefix(out, "plan: 1 of 2 done") || !strings.Contains(out, "  2 [todo] emit") {
		t.Errorf("show all: %s", out)
	}
	if out = call(st, "", map[string]any{"action": "ready"}); out != "ready: 2 emit" {
		t.Errorf("ready: %s", out)
	}
}

func TestToolDropAndReplace(t *testing.T) {
	st := NewMemStore(Graph{}, nil)
	call(st, "", map[string]any{"action": "add", "title": "a", "done_when": "x"})
	call(st, "", map[string]any{"action": "add", "title": "b", "done_when": "x", "needs": []string{"1"}})
	out := call(st, "", map[string]any{"action": "drop", "id": "1"})
	if !strings.Contains(out, "say why") {
		t.Errorf("drop with no reason: %s", out)
	}
	out = call(st, "", map[string]any{"action": "drop", "id": "1", "reason": "bad idea"})
	if !strings.Contains(out, "1 [dropped]") || !strings.Contains(out, "wait on it still") || !strings.Contains(out, "replaces 1") || !strings.Contains(out, "ready: none") {
		t.Errorf("drop: %s", out)
	}
	out = call(st, "", map[string]any{"action": "add", "title": "a2", "done_when": "x", "replaces": "1"})
	if !strings.Contains(out, "(replaces 1)") {
		t.Errorf("replace: %s", out)
	}
	out = call(st, "", map[string]any{"action": "update", "id": "2", "needs": []string{"3"}})
	if !strings.Contains(out, "ready: 3 a2") {
		t.Errorf("re-point: %s", out)
	}
	if g := view(t, st); g.Get("1") == nil {
		t.Error("the dropped node was deleted")
	}
}

func TestToolErrors(t *testing.T) {
	st := NewMemStore(Graph{}, nil)
	call(st, "", map[string]any{"action": "add", "title": "a", "done_when": "x"})
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"unknown action": {map[string]any{"action": "nuke"}, "action"},
		"no id":          {map[string]any{"action": "done", "account": "x"}, "needs an id"},
		"missing node":   {map[string]any{"action": "block", "id": "9", "reason": "x"}, "no node 9"},
		"show missing":   {map[string]any{"action": "show", "id": "9"}, "no node 9"},
		"cycle":          {map[string]any{"action": "update", "id": "1", "needs": []string{"1"}}, "itself"},
		"no title":       {map[string]any{"action": "add", "done_when": "x"}, "title"},
		"bad status":     {map[string]any{"action": "update", "id": "1", "status": "done"}, "status"},
	} {
		if out := call(st, "", tc.args); !strings.HasPrefix(out, "plan: ") || !strings.Contains(out, tc.want) {
			t.Errorf("%s: %s", name, out)
		}
	}
	_, h := Tool(st, "")
	if out := h(context.Background(), "{nope"); !strings.Contains(out, "not valid JSON") {
		t.Errorf("bad json: %s", out)
	}
}

func TestToolOwnership(t *testing.T) {
	g := Graph{}
	g.Add(Node{Title: "a", DoneWhen: "x"})
	g.Add(Node{Title: "b", DoneWhen: "x", Needs: []string{"1"}})
	g.Add(Node{Title: "c", DoneWhen: "x"})
	if err := g.Assign("1", "7"); err != nil {
		t.Fatal(err)
	}
	st := NewMemStore(g, nil)
	if out := call(st, "7", map[string]any{"action": "update", "id": "3", "title": "mine now"}); !strings.Contains(out, "not yours") {
		t.Errorf("actor changed main's node: %s", out)
	}
	if out := call(st, "7", map[string]any{"action": "update", "id": "2", "title": "b2"}); !strings.Contains(out, "2 [todo] b2") {
		t.Errorf("actor changed its own: %s", out)
	}
	// It adds under its part, and the node is its own.
	if out := call(st, "7", map[string]any{"action": "add", "title": "d", "done_when": "x", "needs": []string{"2"}}); !strings.Contains(out, "(agent 7)") {
		t.Errorf("add: %s", out)
	}
	if out := call(st, "7", map[string]any{"action": "add", "title": "e", "done_when": "x", "needs": []string{"3"}}); !strings.Contains(out, "not yours") {
		t.Errorf("actor hung a node off main's: %s", out)
	}
	// The main agent may change any node.
	if out := call(st, "", map[string]any{"action": "update", "id": "2", "title": "b3"}); !strings.Contains(out, "b3") {
		t.Errorf("main: %s", out)
	}
	// A sub-agent's ready line is its own.
	if out := call(st, "7", map[string]any{"action": "ready"}); out != "ready: 1 a" {
		t.Errorf("ready: %s", out)
	}
}

func TestMemStoreSave(t *testing.T) {
	var saved []Graph
	fail := false
	st := NewMemStore(Graph{}, func(g Graph) error {
		if fail {
			return errors.New("disk full")
		}
		saved = append(saved, g)
		return nil
	})
	if out := call(st, "", map[string]any{"action": "add", "title": "a", "done_when": "x"}); strings.Contains(out, "plan:") && strings.Contains(out, "disk") {
		t.Fatal(out)
	}
	if len(saved) != 1 || len(saved[0].Nodes) != 1 {
		t.Fatalf("saved: %v", saved)
	}
	// A failed save undoes the change and says so.
	fail = true
	if out := call(st, "", map[string]any{"action": "add", "title": "b", "done_when": "x"}); !strings.Contains(out, "disk full") {
		t.Errorf("save failure: %s", out)
	}
	if g := view(t, st); len(g.Nodes) != 1 {
		t.Errorf("a change that was not saved was kept: %d nodes", len(g.Nodes))
	}
	// A refused change does not save.
	fail = false
	call(st, "", map[string]any{"action": "done", "id": "1", "account": "x"})
	if len(saved) != 1 {
		t.Errorf("saved after a refusal: %d", len(saved))
	}
	// View is a copy.
	g := view(t, st)
	g.Nodes[0].Title = "changed"
	if view(t, st).Nodes[0].Title != "a" {
		t.Error("View shares memory")
	}
}

func TestDef(t *testing.T) {
	d, _ := Tool(NewMemStore(Graph{}, nil), "", WithNote("Host words."))
	if !strings.HasSuffix(d.Function.Description, " Host words.") {
		t.Errorf("the host's note is not in the description: %s", d.Function.Description)
	}
	bare, _ := Tool(NewMemStore(Graph{}, nil), "")
	for _, w := range []string{"plan mode", "approve", "files"} {
		if strings.Contains(bare.Function.Description, w) {
			t.Errorf("the description names %q: the host says that", w)
		}
	}
	if d.Function.Name != "plan" || d.Function.Parameters == nil {
		t.Errorf("%+v", d)
	}
	props := d.Function.Parameters.(map[string]any)["properties"].(map[string]any)
	for _, k := range []string{"action", "id", "title", "goal", "done_when", "needs", "notes", "files", "replaces", "status", "account", "reason"} {
		if _, ok := props[k]; !ok {
			t.Errorf("no %s parameter", k)
		}
	}
}

func TestToolWithoutAHookAnyoneMayChange(t *testing.T) {
	g := Graph{}
	g.Add(Node{Title: "a", DoneWhen: "x", Owner: "z"})
	st := NewMemStore(g, nil)
	_, h := Tool(st, "q")
	if out := h(context.Background(), `{"action":"update","id":"1","title":"b"}`); !strings.Contains(out, "1 [todo] b") {
		t.Errorf("%s", out)
	}
	_, h = Tool(st, "q", WithMayChange(func(string, *Node) bool { return false }))
	if out := h(context.Background(), `{"action":"update","id":"1","title":"c"}`); !strings.Contains(out, "not yours") {
		t.Errorf("%s", out)
	}
}
