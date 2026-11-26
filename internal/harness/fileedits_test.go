package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	filetools "github.com/iodesystems/agentkit/files"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/dun/proto"
)

// editHarness is a harness rooted at a temp workspace with a session file,
// hearing tool ends, and the file toolset with every writing tool.
func editHarness(t *testing.T) (*Harness, *filetools.Toolset, *[]ToolEnd) {
	t.Helper()
	ws := t.TempDir()
	st, err := openSessionStore(filepath.Join(t.TempDir(), "s1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ends := &[]ToolEnd{}
	h := &Harness{store: st, wake: make(chan struct{}, 1)}
	h.cfg.Workspace = ws
	h.cfg.OnToolEnd = func(e ToolEnd) { *ends = append(*ends, e) }
	ts, err := filetools.New(filetools.Config{Root: ws, Tools: []string{filetools.Read, filetools.Edit,
		filetools.Write, filetools.Delete, filetools.Move}})
	if err != nil {
		t.Fatal(err)
	}
	return h, ts, ends
}

// editCall runs one tool call through withEdits and withToolEvents, with inner
// as the tool; it returns the edits the tool_result event carried.
func editCall(t *testing.T, h *Harness, ts *filetools.Toolset, ends *[]ToolEnd, id, name, args string,
	inner agent.ToolDispatcher) []FileEdit {
	t.Helper()
	if inner == nil {
		inner = func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
			return ts.Dispatch(ctx, tc.Function.Name, tc.Function.Arguments), nil
		}
	}
	tc := llm.ToolCall{ID: id}
	tc.Function.Name, tc.Function.Arguments = name, args
	n := len(*ends)
	if _, err := withToolEvents(withEdits(inner, h, ts), h)(context.Background(), tc); err != nil {
		t.Fatal(err)
	}
	if len(*ends) != n+1 {
		t.Fatalf("%d tool ends, want %d", len(*ends), n+1)
	}
	return (*ends)[n].Edits
}

func writeWS(t *testing.T, h *Harness, rel, text string) string {
	t.Helper()
	p := filepath.Join(h.cfg.Workspace, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func wantEdits(t *testing.T, got []FileEdit, want ...FileEdit) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("edits\n got %s\nwant %s", g, w)
	}
}

// dun's own file tools: each writing tool's result carries the file's text
// before and after; a read, and a refused write, carry none.
func TestEdits_FileTools(t *testing.T) {
	h, ts, ends := editHarness(t)
	ws := h.cfg.Workspace
	a := writeWS(t, h, "a.txt", "one\ntwo\n")

	editCall(t, h, ts, ends, "r1", ts.Name(filetools.Read), `{"path":"a.txt"}`, nil)
	if e := (*ends)[0].Edits; e != nil {
		t.Errorf("a read carried edits: %+v", e)
	}

	got := editCall(t, h, ts, ends, "e1", ts.Name(filetools.Edit), `{"path":"a.txt","edits":[{"old":"two","new":"TWO"}]}`, nil)
	wantEdits(t, got, FileEdit{Path: a, OldText: "one\ntwo\n", NewText: "one\nTWO\n"})

	b := filepath.Join(ws, "sub", "b.txt")
	got = editCall(t, h, ts, ends, "w1", ts.Name(filetools.Write), `{"path":"sub/b.txt","content":"new\n"}`, nil)
	wantEdits(t, got, FileEdit{Path: b, NewText: "new\n", Created: true})

	c := filepath.Join(ws, "c.txt")
	got = editCall(t, h, ts, ends, "m1", ts.Name(filetools.Move), `{"path":"a.txt","to":"c.txt"}`, nil)
	wantEdits(t, got, FileEdit{Path: a, OldText: "one\nTWO\n", Deleted: true},
		FileEdit{Path: c, NewText: "one\nTWO\n", Created: true})

	got = editCall(t, h, ts, ends, "d1", ts.Name(filetools.Delete), `{"path":"c.txt"}`, nil)
	wantEdits(t, got, FileEdit{Path: c, OldText: "one\nTWO\n", Deleted: true})

	// An edit whose old text is not there changes nothing.
	got = editCall(t, h, ts, ends, "e2", ts.Name(filetools.Edit), `{"path":"sub/b.txt","edits":[{"old":"absent","new":"x"}]}`, nil)
	if got != nil {
		t.Errorf("a failed edit carried edits: %+v", got)
	}
}

// node_edit writes the file itself: the harness reads the file it names
// (file, or node's file) before and after. node_refactor is not read.
func TestEdits_NodeEdit(t *testing.T) {
	h, ts, ends := editHarness(t)
	p := writeWS(t, h, "pkg/x.go", "package x\n")
	writer := func(text string) agent.ToolDispatcher {
		return func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
			return llm.TextResult(`{"node":"pkg/x.go","bytesAdded":1}`), os.WriteFile(p, []byte(text), 0o644)
		}
	}
	got := editCall(t, h, ts, ends, "n1", "node_edit", `{"file":"pkg/x.go","newText":"package x\n\nfunc F() {}\n"}`,
		writer("package x\n\nfunc F() {}\n"))
	wantEdits(t, got, FileEdit{Path: p, OldText: "package x\n", NewText: "package x\n\nfunc F() {}\n"})

	got = editCall(t, h, ts, ends, "n2", "node_edit", `{"node":"pkg/x.go#F","newText":"func F() { G() }"}`,
		writer("package x\n\nfunc F() { G() }\n"))
	wantEdits(t, got, FileEdit{Path: p, OldText: "package x\n\nfunc F() {}\n", NewText: "package x\n\nfunc F() { G() }\n"})

	del := func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
		return llm.TextResult(`{"deleted":true}`), os.Remove(p)
	}
	got = editCall(t, h, ts, ends, "n3", "node_delete", `{"file":"`+p+`"}`, del)
	wantEdits(t, got, FileEdit{Path: p, OldText: "package x\n\nfunc F() { G() }\n", Deleted: true})

	q := writeWS(t, h, "q.go", "package q\n")
	got = editCall(t, h, ts, ends, "n4", "node_refactor", `{"file":"q.go"}`, func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
		return llm.TextResult("{}"), os.WriteFile(q, []byte("package r\n"), 0o644)
	})
	if got != nil {
		t.Errorf("node_refactor carried edits: %+v", got)
	}
}

// A file over the size cap, a binary one and a call past its cap are named
// with skipped and no texts.
func TestEdits_Caps(t *testing.T) {
	h, ts, ends := editHarness(t)
	big := writeWS(t, h, "big.txt", strings.Repeat("x", editFileMax+1))
	got := editCall(t, h, ts, ends, "b1", "node_edit", `{"file":"big.txt"}`, func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
		return llm.TextResult("{}"), os.WriteFile(big, []byte("small\n"), 0o644)
	})
	wantEdits(t, got, FileEdit{Path: big, Skipped: "too_large"})

	bin := writeWS(t, h, "bin.dat", "a\x00b")
	got = editCall(t, h, ts, ends, "b2", "node_edit", `{"file":"bin.dat"}`, func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
		return llm.TextResult("{}"), os.WriteFile(bin, []byte("a\x00c"), 0o644)
	})
	wantEdits(t, got, FileEdit{Path: bin, Skipped: "binary"})

	// Two files whose texts together pass the call cap: the second is named only.
	half := strings.Repeat("y", editCallMax/2-10)
	capped := capCall([]FileEdit{{Path: "/a", NewText: half}, {Path: "/b", OldText: half, NewText: half}})
	if capped[0].NewText != half || capped[1].Skipped != "call_cap" || capped[1].OldText != "" {
		t.Errorf("call cap: %q %+v", capped[0].Skipped, capped[1].Skipped)
	}
}

// Without a client hearing tool ends (a sub-agent), no file is read.
func TestEdits_OffWithoutToolEnd(t *testing.T) {
	h, ts, _ := editHarness(t)
	h.cfg.OnToolEnd = nil
	p := writeWS(t, h, "a", "a")
	inner := func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
		return llm.TextResult(""), os.WriteFile(p, []byte("b"), 0o644)
	}
	tc := llm.ToolCall{ID: "x"}
	tc.Function.Name, tc.Function.Arguments = "node_edit", `{"file":"a"}`
	_, _ = withEdits(inner, h, ts)(context.Background(), tc)
	if e := h.toolCur.takeEdits(); e != nil {
		t.Errorf("edits noted with no OnToolEnd: %+v", e)
	}
}

// The edits are kept in the sidecar (a big text as a blob) and come back on
// the history's tool_result items; /clear removes them.
func TestEdits_PersistedForHistory(t *testing.T) {
	h, ts, ends := editHarness(t)
	big := strings.Repeat("line\n", 4000) // over blobThreshold
	p := writeWS(t, h, "f.txt", big)
	editCall(t, h, ts, ends, "c1", "node_edit", `{"file":"f.txt"}`, func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
		return llm.TextResult("{}"), os.WriteFile(p, []byte(big+"end\n"), 0o644)
	})
	raw, err := os.ReadFile(h.store.editsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "line\nline") || !strings.Contains(string(raw), `"call_id":"c1"`) {
		t.Errorf("sidecar kept the text inline or lost the call: %.200q", raw)
	}
	ctx := context.Background()
	_ = h.store.Append(ctx, "", agent.Entry{Kind: agent.KindToolCall, ToolName: "node_edit", ToolCallID: "c1", Content: `{"file":"f.txt"}`, CreatedAt: 1})
	_ = h.store.Append(ctx, "", agent.Entry{Kind: agent.KindToolResult, ToolName: "node_edit", ToolCallID: "c1", Content: "{}", CreatedAt: 2})

	for name, items := range map[string][]HistoryItem{"live": h.History(), "file": SessionHistory(h.store.path)} {
		var res *HistoryItem
		for i := range items {
			if items[i].Kind == "tool_result" {
				res = &items[i]
			}
		}
		if res == nil || len(res.Edits) != 1 || res.Edits[0].OldText != big || res.Edits[0].NewText != big+"end\n" {
			t.Errorf("%s: history result %+v", name, res)
		}
	}
	// The transcript the model reads has no edit text.
	if tr, _ := os.ReadFile(h.store.path); strings.Contains(string(tr), "end\\n") {
		t.Error("the transcript carries the edit's text")
	}
	h.store.Reset()
	if _, err := os.Stat(h.store.editsPath()); !os.IsNotExist(err) {
		t.Errorf("sidecar kept after /clear: %v", err)
	}
}

// A replay keeps the newest edits' texts within the history cap.
func TestEdits_HistoryCap(t *testing.T) {
	half := strings.Repeat("z", editHistoryMax/2+1)
	items := []HistoryItem{{Kind: "tool_result", CallID: "old"}, {Kind: "tool_result", CallID: "new"}}
	out := attachEdits(items, map[string][]FileEdit{
		"old": {{Path: "/o", NewText: half}},
		"new": {{Path: "/n", NewText: half}},
	})
	if out[1].Edits[0].NewText != half || out[0].Edits[0].Skipped != "history_cap" || out[0].Edits[0].NewText != "" {
		t.Errorf("cap kept the wrong one: old %q new %q", out[0].Edits[0].Skipped, out[1].Edits[0].Skipped)
	}
}

// The event's JSON: edits omitted when none, snake_case fields, empty ones
// omitted; the harness's FileEdit converts to the protocol's.
func TestEdits_EventJSON(t *testing.T) {
	b, _ := json.Marshal(proto.ToolResult{CallID: "c", Tool: "file_edit", Result: "ok"})
	if strings.Contains(string(b), "edits") {
		t.Errorf("empty edits sent: %s", b)
	}
	e := proto.FileEdit(FileEdit{Path: "/w/a.go", OldText: "a", NewText: "b"})
	b, _ = json.Marshal(proto.ToolResult{CallID: "c", Tool: "file_edit", Result: "ok", Edits: []proto.FileEdit{e,
		{Path: "/w/n.go", NewText: "x", Created: true}, {Path: "/w/big", Deleted: true, Skipped: "too_large"}}})
	want := `{"call_id":"c","tool":"file_edit","result":"ok","edits":[` +
		`{"path":"/w/a.go","old_text":"a","new_text":"b"},` +
		`{"path":"/w/n.go","new_text":"x","created":true},` +
		`{"path":"/w/big","deleted":true,"skipped":"too_large"}]}`
	if string(b) != want {
		t.Errorf("json\n got %s\nwant %s", b, want)
	}
}
