package dun

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Persist carries the sub-agents: a persisted root's resume restarts the
// children that were running or idle, idle, with their isolation; stopped
// ones stay stopped and dismissed ones stay dismissed. Without persist they
// come back stopped, as before.

// blockRound is a model round that never ends until its turn is cancelled.
func blockRound(_ context.Context, _ []llm.Message, out chan<- llm.StreamChunk) {
	<-make(chan struct{})
}

// cancellableBlock blocks until ctx ends.
func cancellableBlock(ctx context.Context, _ []llm.Message, _ chan<- llm.StreamChunk) { <-ctx.Done() }

// persistFixture is a root session on disk with three children: #1 idle in
// nodir isolation on model "a", #2 dismissed, #3 running when the root closed.
func persistFixture(t *testing.T) (ws, sessionFile string, runners map[string]*scriptRunner) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	ws = t.TempDir()
	sessionFile = filepath.Join(t.TempDir(), "s.jsonl")
	runners = map[string]*scriptRunner{
		"a": {rounds: []roundFn{textRound("found it")}},
		"b": {rounds: []roundFn{textRound("done")}},
		"c": {rounds: []roundFn{cancellableBlock}},
	}
	h := startWith(t, ws, sessionFile, runners, true)
	one, err := h.spawnAgent(context.Background(), "find the config", "a", IsoNoDir)
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, one)
	two := spawnKidOn(t, h, "count the lines", "b")
	waitIdle(t, two)
	two.quit()
	three := spawnKidOn(t, h, "read the whole log", "c")
	waitRunning(t, runners["c"])
	if st, _, _, _ := three.snapshot(); st != agentRunning {
		t.Fatalf("#3 should be running, is %s", st)
	}
	h.Close()
	return ws, sessionFile, runners
}

func startWith(t *testing.T, ws, sessionFile string, runners map[string]*scriptRunner, persist bool) *Harness {
	t.Helper()
	var mu sync.Mutex
	h, err := Start(context.Background(), Config{
		Workspace: ws, Servers: []Server{}, Client: &scriptRunner{}, Exec: &HostShell{Dir: ws},
		ClientFor: func(model string) (agent.LLMRunner, error) {
			mu.Lock()
			defer mu.Unlock()
			return runners[model], nil
		},
		SessionFile: sessionFile, Persist: persist,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func spawnKidOn(t *testing.T, h *Harness, prompt, model string) *subAgent {
	t.Helper()
	sa, err := h.spawnAgent(context.Background(), prompt, model, "")
	if err != nil {
		t.Fatal(err)
	}
	return sa
}

// waitRunning waits until r has been asked for a round.
func waitRunning(t *testing.T, r *scriptRunner) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for r.calls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the child never asked its model")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func agentStates(h *Harness) map[int]string {
	out := map[int]string{}
	for _, a := range h.Agents() {
		out[a.ID] = a.State
	}
	return out
}

func TestSubAgent_PersistedResumeRestartsLiveChildren(t *testing.T) {
	ws, sf, runners := persistFixture(t)
	h := startWith(t, ws, sf, runners, true)
	defer h.Close()

	want := map[int]string{1: "idle", 2: "dismissed", 3: "idle"}
	if got := agentStates(h); len(got) != 3 || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("states %v, want %v", got, want)
	}
	one, two, three := h.agentByID(1), h.agentByID(2), h.agentByID(3)
	if one.harness() == nil || three.harness() == nil || two.harness() != nil {
		t.Fatal("#1 and #3 must run again, #2 must not")
	}
	// Its isolation and its model came back with it.
	if c := one.harness().cfg; one.iso.Mode != IsoNoDir || !c.NoDir || c.Workspace != one.iso.Path || one.model != "a" {
		t.Errorf("#1: iso %+v, nodir %v, ws %q, model %q", one.iso, c.NoDir, c.Workspace, one.model)
	}
	// Said once, in the root's context: which came back, and that #3 was cut.
	note := h.restoredAgentsNote(nil, nil)
	if note != "" {
		t.Errorf("nothing is stopped, so only the resume note is due: %q", note)
	}
	var said []string
	for _, n := range h.notes() {
		if strings.Contains(n, "Sub-agents") {
			said = append(said, n)
		}
	}
	if len(said) != 1 || !strings.Contains(said[0], "#1 (find the config)") ||
		!strings.Contains(said[0], "#3 (read the whole log) — it was working") ||
		strings.Contains(said[0], "#2") || strings.Contains(said[0], "#1 (find the config) — it was working") {
		t.Errorf("resume note: %q", said)
	}
	// It has its transcript: a tell continues the same conversation.
	runners["a"].rounds = append(runners["a"].rounds, textRound("again"))
	if out := one.tell("once more"); !strings.Contains(out, "Told agent #1") {
		t.Fatalf("tell: %s", out)
	}
	waitIdle(t, one)
	sent := runners["a"].sent[len(runners["a"].sent)-1]
	if !strings.Contains(llmText(sent), "find the config") {
		t.Errorf("the resumed child lost its transcript: %v", llmText(sent))
	}

}

// Without persist the same children come back stopped; the dismissed one
// stays dismissed. Toggling persist is the switch between the two: dund
// passes it at every engine start (internal/dund, spawnArgs).
func TestSubAgent_UnpersistedResumeLeavesChildrenStopped(t *testing.T) {
	ws, sf, runners := persistFixture(t)
	h := startWith(t, ws, sf, runners, false)
	if got := agentStates(h); got[1] != "stopped" || got[2] != "dismissed" || got[3] != "stopped" {
		t.Fatalf("states %v", got)
	}
	if note := h.restoredAgentsNote(nil, nil); !strings.Contains(note, "STOPPED") || strings.Contains(note, "#2") {
		t.Errorf("note %q", note)
	}
	h.Close()

	// The setting toggled on: the next resume brings #1 and #3 back.
	h = startWith(t, ws, sf, runners, true)
	defer h.Close()
	if got := agentStates(h); got[1] != "idle" || got[2] != "dismissed" || got[3] != "idle" {
		t.Fatalf("after persist on: %v", got)
	}
}

// A watched child's new entries reach OnAgentEntry after the history it was
// opened with, and stop when the last viewer leaves.
func TestSubAgent_WatchStreamsEntries(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	kid := &scriptRunner{rounds: []roundFn{textRound("first answer"), textRound("second answer"), textRound("third")}}
	var mu sync.Mutex
	var live []HistoryItem
	h, err := Start(context.Background(), Config{
		Workspace: ws, Servers: []Server{}, Client: &scriptRunner{},
		ClientFor:   func(string) (agent.LLMRunner, error) { return kid, nil },
		SessionFile: filepath.Join(ws, "s.jsonl"),
		OnAgentEntry: func(id int, items []HistoryItem) {
			mu.Lock()
			defer mu.Unlock()
			if id == 1 {
				live = append(live, items...)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	sa := spawnKidOn(t, h, "say something", "kid")
	waitIdle(t, sa)

	var hist []HistoryItem
	if !h.AgentWatch(1, func(items []HistoryItem) { hist = items }) {
		t.Fatal("no agent #1")
	}
	if len(hist) < 2 || hist[0].Content != "say something" {
		t.Fatalf("history %+v", hist)
	}
	sa.tell("and again")
	waitIdle(t, sa)
	mu.Lock()
	got := append([]HistoryItem(nil), live...)
	mu.Unlock()
	if len(got) != 2 || got[0].Kind != "user" || got[0].Content != "and again" ||
		got[1].Kind != "assistant" || got[1].Content != "second answer" {
		t.Fatalf("live entries %+v", got)
	}

	h.AgentUnwatch(1)
	sa.tell("a third time")
	waitIdle(t, sa)
	mu.Lock()
	n := len(live)
	mu.Unlock()
	if n != 2 {
		t.Errorf("entries forwarded after the viewer left: %d", n)
	}
}

func llmText(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content + "\n")
	}
	return b.String()
}
