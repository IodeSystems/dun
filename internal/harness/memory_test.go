package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/memory"
)

// forkRunner answers a fork with one memory call and one other call.
type forkRunner struct {
	prefixRunner
	opts []*llm.ChatOpts
}

func (r *forkRunner) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.opts = append(r.opts, opts)
	if len(r.msgs) == 0 {
		return r.prefixRunner.ChatStream(ctx, msgs, tools, opts)
	}
	r.msgs = append(r.msgs, msgs)
	r.tools = append(r.tools, tools)
	ch := make(chan llm.StreamChunk, 4)
	call := func(name, args string) *llm.ToolCall {
		tc := &llm.ToolCall{ID: name}
		tc.Function.Name, tc.Function.Arguments = name, args
		return tc
	}
	ch <- llm.StreamChunk{ToolCall: call("exec", `{"command":"rm -rf /"}`)}
	ch <- llm.StreamChunk{ToolCall: call("memory", `{"action":"add","content":"The user wants fragments, not prose."}`)}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

// The fork starts with the turn's prefix, forces the memory tool, and runs
// only the memory call — every tool rides along for the cache, none of the
// others may run.
func TestRemember_ForksWithTheTurnsPrefixAndRunsOnlyMemory(t *testing.T) {
	defs := []llm.ToolDef{{Type: "function"}, memory.ToolDef()}
	defs[0].Function.Name = "exec"
	h := newNoteHarness(t)
	r := &forkRunner{}
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: r,
		System: "You are dun.", Tools: defs,
		Build: func(ctx context.Context, sid, system string) ([]llm.Message, error) {
			return agent.DefaultContextBuilder(ctx, h.store, sid, system)
		}}
	m := testNotes(t)
	h.memory.Store(m)
	ctx := context.Background()
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "u1", Kind: agent.KindUser, Content: "short answers please"})
	if _, err := h.Session.Turn(ctx); err != nil {
		t.Fatal(err)
	}

	out, err := h.Remember(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "saved ") || strings.Contains(out, ";") {
		t.Errorf("want exactly the memory call's result, got %q", out)
	}
	turn, fork := r.msgs[0], r.msgs[1]
	if len(fork) != len(turn)+2 { // + the turn's reply + the instruction
		t.Fatalf("fork has %d messages, turn %d", len(fork), len(turn))
	}
	for i := range turn {
		if fork[i].Role != turn[i].Role || fork[i].Content != turn[i].Content {
			t.Errorf("message %d differs from the turn's", i)
		}
	}
	if len(r.tools[1]) != len(defs) {
		t.Errorf("fork sent %d tools; the cache needs all %d", len(r.tools[1]), len(defs))
	}
	if got := r.opts[1].ToolChoice; !strings.Contains(got, `"name":"memory"`) {
		t.Errorf("tool_choice %q does not force memory", got)
	}
}

// No RagLink is DUN_MEMORY=0: /rag on would not help, so it is not named.
func TestRemember_NeedsTheProjectIndex(t *testing.T) {
	h := newNoteHarness(t)
	if _, err := h.Remember(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "DUN_MEMORY=0") {
		t.Errorf("want a pointer to DUN_MEMORY=0, got %v", err)
	}
}

func TestRaglitStoreFlags_KeepsTheStoreNotTheIndex(t *testing.T) {
	got := strings.Join(raglitStoreFlags([]string{"serve", "--project", "dun", "--index", "dun-main"}), " ")
	if got != "--project dun" {
		t.Errorf("daemon config: got %q", got)
	}
	got = strings.Join(raglitStoreFlags([]string{"serve", "--embedded", "--home=/h", "--index", "m0", "--embed"}), " ")
	if got != "--embedded --home=/h" {
		t.Errorf("embedded config: got %q", got)
	}
}

// testNotes is a notes store with no index, in a temp DUN_HOME.
func testNotes(t *testing.T) *memory.Store {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	return memory.NotesFor(nil, t.TempDir())
}
