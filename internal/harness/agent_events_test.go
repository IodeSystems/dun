package harness

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// callRound is a model round that makes one call to name with args.
func callRound(id, name, args string) roundFn {
	return func(_ context.Context, _ []llm.Message, out chan<- llm.StreamChunk) {
		tc := &llm.ToolCall{ID: id, Type: "function"}
		tc.Function.Name, tc.Function.Arguments = name, args
		out <- llm.StreamChunk{ToolCall: tc}
		out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
	}
}

// The agent call names its child: the spawn's tool_result (its number is
// known only once it started), a follow-up's tool_call and tool_result (its
// id argument). The replayed history carries the same numbers, from the
// live harness and from the session file alone (the child's sidecar keeps
// the spawning call's id).
func TestAgentCall_EventsAndHistoryNameTheChild(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	file := filepath.Join(dir, "s.jsonl")
	parent := &scriptRunner{}
	var mu sync.Mutex
	starts := map[string]ToolStart{}
	ends := map[string]ToolEnd{}
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []Server{}, Client: parent,
		ClientFor: func(string, string) (ChildClient, error) {
			return ChildClient{Runner: &scriptRunner{rounds: []roundFn{textRound("child done")}}}, nil
		},
		SessionFile: file,
		OnToolStart: func(s ToolStart) { mu.Lock(); starts[s.CallID] = s; mu.Unlock() },
		OnToolEnd:   func(e ToolEnd) { mu.Lock(); ends[e.CallID] = e; mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	planCall(t, h, "plan_add", `{"title":"build","done_when":"x"}`)
	node := planIDs(h)["build"]
	parent.rounds = []roundFn{
		callRound("spawn1", "agent", `{"model":"m","node":"`+node+`"}`),
		textRound("started"),
	}
	if _, err := h.Ask(context.Background(), "build it"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h.agentByID(1))
	parent.mu.Lock()
	n := len(parent.sent)

	parent.rounds = append(parent.rounds[:n:n],
		callRound("view1", "agent", `{"id":1}`),
		callRound("gone1", "agent", `{"id":9}`),
		textRound("done"))
	parent.mu.Unlock()
	if _, err := h.Ask(context.Background(), "how is it"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	if s := starts["spawn1"]; s.Agent != 0 {
		t.Errorf("a spawn's start names no child yet: %+v", s)
	}
	if e := ends["spawn1"]; e.Agent != 1 {
		t.Errorf("spawn result agent = %d, want 1 (%q)", e.Agent, e.Result)
	}
	if s, e := starts["view1"], ends["view1"]; s.Agent != 1 || e.Agent != 1 {
		t.Errorf("follow-up: start %d end %d, want 1", s.Agent, e.Agent)
	}
	if s, e := starts["gone1"], ends["gone1"]; s.Agent != 9 || e.Agent != 0 {
		t.Errorf("no such child: start %d (the asked id) end %d (none reached)", s.Agent, e.Agent)
	}
	mu.Unlock()

	want := map[string]int{"spawn1": 1, "view1": 1}
	check := func(from string, items []HistoryItem) {
		t.Helper()
		seen := 0
		for _, it := range items {
			if it.Tool != "agent" || (it.Kind != "tool_call" && it.Kind != "tool_result") {
				continue
			}
			w, ok := want[it.CallID]
			if !ok {
				continue
			}
			seen++
			if it.Agent != w {
				t.Errorf("%s: %s %s agent = %d, want %d", from, it.Kind, it.CallID, it.Agent, w)
			}
		}
		if seen != 4 {
			t.Errorf("%s: %d agent items, want 4", from, seen)
		}
	}
	check("History", h.History())
	check("SessionHistory", SessionHistory(file))
	if rec := loadAgentState(filepath.Join(dir, "s.sub1.jsonl")); rec.CallID != "spawn1" {
		t.Errorf("sidecar call id = %q", rec.CallID)
	}
}

func TestNoticeAgent(t *testing.T) {
	for text, want := range map[string]int{
		"agent #3: found it":                                    3,
		"agent #12 is idle after 2m (1k tokens)\ntask: x":       12,
		"agent #2 is still running, quiet for 5m — task: x":     2,
		"[control/plan] agent #4 published plan node(s) 2 done": 4,
		"background job #3 ended":                               0,
		"the agent #3 said":                                     0,
		"agent #x: no":                                          0,
	} {
		if got := NoticeAgent(text); got != want {
			t.Errorf("NoticeAgent(%q) = %d, want %d", text, got, want)
		}
	}
	items := historyItems([]agent.Entry{{Kind: agent.KindNotification, Content: "agent #5: done"}})
	if len(items) != 1 || items[0].Agent != 5 {
		t.Errorf("a replayed notice: %+v", items)
	}
}
