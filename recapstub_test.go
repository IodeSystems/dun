package dun

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// stubConvo is a turn: the user message, a tool call and result for each of
// calls, then the live recap call.
func stubConvo(calls [][3]string) []agent.Entry {
	n := 0
	id := func() string { n++; return fmt.Sprintf("e%03d", n) }
	out := []agent.Entry{{ID: id(), Kind: agent.KindUser, Content: "fix the build"}}
	for k, c := range calls {
		cid := fmt.Sprintf("c%d", k+1)
		out = append(out,
			agent.Entry{ID: id(), Kind: agent.KindAssistant, Content: "trying " + cid},
			agent.Entry{ID: id(), Kind: agent.KindToolCall, Content: c[1], ToolCallID: cid, ToolName: c[0]},
			agent.Entry{ID: id(), Kind: agent.KindToolResult, Content: c[2], ToolCallID: cid, ToolName: c[0]})
	}
	return append(out,
		agent.Entry{ID: id(), Kind: agent.KindAssistant, Content: "recapping"},
		agent.Entry{ID: id(), Kind: agent.KindToolCall, Content: `{"summary":"x"}`, ToolCallID: "live", ToolName: "recap"})
}

func stubHarness(t *testing.T, entries []agent.Entry) (*Harness, *sessionStore) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "s.jsonl")
	st, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		st.appendSilent(e)
	}
	return &Harness{store: st, cfg: Config{SessionFile: path, PlanNudgeFails: -1}}, st
}

// recapText runs a recap and returns the replacement as stored.
func recapText(t *testing.T, h *Harness, st *sessionStore, summary string, keep, drop []string) string {
	t.Helper()
	if out := h.runRecap(context.Background(), "", summary, keep, drop); strings.HasPrefix(out, "ERROR") {
		t.Fatal(out)
	}
	after, _ := st.Context(context.Background(), "dun")
	for _, e := range after {
		if strings.HasPrefix(e.Content, summary) {
			return e.Content
		}
	}
	t.Fatal("no replacement in context")
	return ""
}

var stubRe = regexp.MustCompile(`(?m)^- (\S+): (.*) → (.+), ([0-9]+) chars, (\S+)$`)

// Each removed call stays as one line: tool, arguments, outcome, size and a
// file that holds the result.
func TestRecapStub_ListsEachRemovedCallWithAReadablePath(t *testing.T) {
	big := strings.Repeat("FAIL line\n", 300) + "[exit: status 2]"
	h, st := stubHarness(t, stubConvo([][3]string{
		{"exec", `{"command":"go test ./...\necho more"}`, big},
		{"file_read", `{"path":"/src/main.go"}`, "package main"},
		{"node_query", `{"q":"x"}`, "ERROR: no such node"},
	}))
	text := recapText(t, h, st, "Build fixed.", nil, nil)
	if !strings.Contains(text, recapStubHead) {
		t.Fatalf("no stub list:\n%s", text)
	}
	if strings.Contains(text, "FAIL line") {
		t.Error("a stub must not carry the result itself")
	}
	m := stubRe.FindAllStringSubmatch(text, -1)
	if len(m) != 3 {
		t.Fatalf("want 3 stubs, got %d:\n%s", len(m), text)
	}
	want := []struct{ tool, args, outcome, result string }{
		{"exec", "go test ./... …", "exit 2", big},
		{"file_read", `{"path":"/src/main.go"}`, "ok", "package main"},
		{"node_query", `{"q":"x"}`, "ERROR", "ERROR: no such node"},
	}
	for i, w := range want {
		g := m[i]
		if g[1] != w.tool || g[2] != w.args || g[3] != w.outcome || g[4] != fmt.Sprint(len(w.result)) {
			t.Errorf("stub %d: %q, want %s %q %s %d", i, g[0], w.tool, w.args, w.outcome, len(w.result))
		}
		if !filepath.IsAbs(g[5]) || !strings.HasPrefix(g[5], IsoTmpDir(h.cfg.SessionFile)) {
			t.Errorf("stub %d: path %q is not in the isolation tmp", i, g[5])
		}
		data, err := os.ReadFile(g[5])
		if err != nil {
			t.Fatalf("stub %d: %v", i, err)
		}
		if !strings.HasSuffix(string(data), "\n\n"+w.result) {
			t.Errorf("stub %d: file does not hold the full result:\n%.200s", i, data)
		}
	}
	if data, _ := os.ReadFile(m[0][5]); !strings.HasPrefix(string(data), "$ go test ./...\necho more\n\n") {
		t.Errorf("an exec's file starts with its command:\n%.80s", data)
	}
	// The audit sidecar is still written.
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.cfg.SessionFile), "s.recap1.jsonl")); err != nil {
		t.Error(err)
	}
}

// keep is unchanged: the kept pair stays whole and gets no stub.
func TestRecapStub_KeepHoldsTheResultAndHasNoStub(t *testing.T) {
	h, st := stubHarness(t, stubConvo([][3]string{
		{"exec", `{"command":"make"}`, "built"},
		{"exec", `{"command":"grep -c TODO"}`, "17"},
	}))
	text := recapText(t, h, st, "Made it.", []string{"grep -c"}, nil)
	if strings.Contains(text, "grep -c") || !strings.Contains(text, "- exec: make → ok") {
		t.Fatalf("stubs:\n%s", text)
	}
	after, _ := st.Context(context.Background(), "dun")
	n := 0
	for _, e := range after {
		if e.ToolCallID == "c2" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("the kept pair: %d of 2 entries", n)
	}
}

// drop removes a call with no stub and no file; a call both keep and drop
// name is kept.
func TestRecapStub_DropLeavesNoStub(t *testing.T) {
	h, st := stubHarness(t, stubConvo([][3]string{
		{"exec", `{"command":"jobs status 1"}`, "running"},
		{"exec", `{"command":"jobs status 2"}`, "running"},
		{"exec", `{"command":"make test"}`, "PASS"},
	}))
	var note RecapNote
	h.cfg.OnRecap = func(n RecapNote) { note = n }
	out := h.runRecap(context.Background(), "", "Tests pass.", []string{"make test"}, []string{"jobs status", "make test", "nothing like this"})
	after, _ := st.Context(context.Background(), "dun")
	var text string
	for _, e := range after {
		if strings.HasPrefix(e.Content, "Tests pass.") {
			text = e.Content
		}
	}
	if strings.Contains(text, "jobs status") || strings.Contains(text, recapStubHead) {
		t.Errorf("dropped calls must leave no stub:\n%s", text)
	}
	kept := 0
	for _, e := range after {
		if e.ToolCallID == "c3" {
			kept++
		}
	}
	if kept != 2 {
		t.Errorf("keep wins over drop: %d of 2 entries", kept)
	}
	files, _ := filepath.Glob(filepath.Join(IsoTmpDir(h.cfg.SessionFile), "recap*.out"))
	if len(files) != 0 {
		t.Errorf("a dropped call gets no file: %v", files)
	}
	if !strings.Contains(out, "drop nothing like this matched no tool call") {
		t.Errorf("an unmatched drop term must be said: %q", out)
	}
	if !strings.Contains(note.Detail, "Dropped the exec, exec calls") {
		t.Errorf("the report names what was dropped: %q", note.Detail)
	}
}

// Past recapStubMax, the earliest calls become one count line.
func TestRecapStub_CollapsesPastTheLimit(t *testing.T) {
	var calls [][3]string
	for i := range recapStubMax + 5 {
		res := "ok"
		if i%2 == 0 && i < 5 {
			res = "bad\n[exit: status 1]"
		}
		calls = append(calls, [3]string{"exec", fmt.Sprintf(`{"command":"try %d"}`, i), res})
	}
	calls = append(calls, [3]string{"file_read", `{"path":"/a"}`, "a"})
	calls[1] = [3]string{"file_read", `{"path":"/b"}`, "b"}
	h, st := stubHarness(t, stubConvo(calls))
	text := recapText(t, h, st, "Tried many.", nil, nil)
	if got := len(stubRe.FindAllString(text, -1)); got != recapStubMax {
		t.Errorf("%d stub lines, want %d", got, recapStubMax)
	}
	dir := IsoTmpDir(h.cfg.SessionFile)
	want := fmt.Sprintf("6 earlier calls: 5 exec (3 failed), 1 file_read — results in %s", dir)
	if !strings.Contains(text, want) {
		t.Errorf("no count line %q:\n%s", want, text)
	}
	if strings.Contains(text, "try 5 →") || !strings.Contains(text, "try 6 →") {
		t.Error("the most recent calls get the lines of their own")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "recap1-*.out"))
	if len(files) != len(calls) {
		t.Errorf("%d result files, want one per call (%d)", len(files), len(calls))
	}
	// A second recap in the turn carries the first one's lines on.
	st.appendSilent(agent.Entry{ID: "z1", Kind: agent.KindToolResult, Content: "Done", ToolCallID: "live", ToolName: "recap"})
	st.appendSilent(agent.Entry{ID: "z2", Kind: agent.KindToolCall, Content: `{"command":"last"}`, ToolCallID: "n1", ToolName: "exec"})
	st.appendSilent(agent.Entry{ID: "z3", Kind: agent.KindToolResult, Content: "fine", ToolCallID: "n1", ToolName: "exec"})
	st.appendSilent(agent.Entry{ID: "z4", Kind: agent.KindToolCall, Content: `{}`, ToolCallID: "live2", ToolName: "recap"})
	text = recapText(t, h, st, "All of it.", nil, nil)
	// 40 carried lines and one new: the oldest carried line joins a count.
	// The earlier recap call itself gets no stub.
	if !strings.Contains(text, want) || !strings.Contains(text, "- exec: last → ok") ||
		!strings.Contains(text, "\n1 earlier call: 1 exec — results in "+dir) ||
		strings.Contains(text, "try 6 →") || strings.Contains(text, "- recap:") ||
		len(stubRe.FindAllString(text, -1)) != recapStubMax {
		t.Errorf("second recap:\n%s", text)
	}
}

type codeBackend struct{ res ExecResult }

func (b codeBackend) Run(context.Context, string, io.Writer) ExecResult { return b.res }

// A failed exec's stub shows its exit code, from the recorded ExecResult.
func TestRecapStub_FailedExecShowsItsExitCode(t *testing.T) {
	h, st := stubHarness(t, nil)
	d := withPlanNudge(withExec(nil, codeBackend{ExecResult{Output: "no rule", Code: 3}}, nil, nil, nil, nil, nil), h)
	out, _ := d(context.Background(), *nudgeCall("c1", "exec", `{"command":"make all"}`))
	if got := h.execVerdictOf("c1"); got != "exit 3" {
		t.Fatalf("recorded verdict %q", got)
	}
	for _, e := range stubConvo([][3]string{{"exec", `{"command":"make all"}`, out}})[1:] {
		st.appendSilent(e)
	}
	// The recorded verdict wins over the text: here they agree, so make the
	// text say otherwise.
	h.noteExecVerdict("c1", "exit 4")
	text := recapText(t, h, st, "make fails.", nil, nil)
	if !strings.Contains(text, "- exec: make all → exit 4, ") {
		t.Errorf("stub:\n%s", text)
	}
	// With no record (a resumed session), the exit line is read.
	if got := h.callOutcome(removedCall{
		Call:   agent.Entry{ToolName: "exec", ToolCallID: "gone"},
		Result: agent.Entry{Content: out + "\n\n" + queuedPolicies[queuedUser].prefix + "stop"},
	}); got != "exit 3" {
		t.Errorf("from the text: %q", got)
	}
}

// The replacement is one assistant entry: every call left in context still has
// its result, and every result its call.
func TestRecapStub_KeepsToolPairs(t *testing.T) {
	h, st := stubHarness(t, stubConvo([][3]string{
		{"exec", `{"command":"alpha"}`, "1"},
		{"exec", `{"command":"beta"}`, "2"},
		{"exec", `{"command":"gamma"}`, "3"},
	}))
	_ = recapText(t, h, st, "Done.", []string{"beta"}, []string{"gamma"})
	after, _ := st.Context(context.Background(), "dun")
	calls, results := map[string]int{}, map[string]int{}
	for _, e := range after {
		switch e.Kind {
		case agent.KindToolCall:
			calls[e.ToolCallID]++
		case agent.KindToolResult:
			results[e.ToolCallID]++
		}
	}
	delete(calls, "live") // its result is the recap's own
	for id, n := range calls {
		if n != 1 || results[id] != 1 {
			t.Errorf("call %s: %d calls, %d results", id, n, results[id])
		}
	}
	for id := range results {
		if calls[id] != 1 {
			t.Errorf("result %s has no call", id)
		}
	}
	if calls["c2"] != 1 || calls["c1"] != 0 || calls["c3"] != 0 {
		t.Errorf("calls left: %v", calls)
	}
}
