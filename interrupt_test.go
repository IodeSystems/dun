package dun

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// roundFn is one round of a scripted model: it writes chunks to out and
// returns when done. It sees the messages the round was sent.
type roundFn func(ctx context.Context, msgs []llm.Message, out chan<- llm.StreamChunk)

// scriptRunner plays roundFns in order, one per ChatStream call, and records
// what every call was sent.
type scriptRunner struct {
	mu     sync.Mutex
	rounds []roundFn
	sent   [][]llm.Message
}

func (r *scriptRunner) ChatStream(ctx context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.mu.Lock()
	n := len(r.sent)
	r.sent = append(r.sent, msgs)
	var fn roundFn
	if n < len(r.rounds) {
		fn = r.rounds[n]
	}
	r.mu.Unlock()
	out := make(chan llm.StreamChunk)
	go func() {
		defer close(out)
		if fn == nil {
			out <- llm.StreamChunk{Content: "ok"}
			out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
			return
		}
		fn(ctx, msgs, out)
	}()
	return out, nil
}

func (r *scriptRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

func (r *scriptRunner) lastSent() []llm.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent[len(r.sent)-1]
}

// thinkThenHang streams reasoning, then some reply text, then signals
// started and waits for the turn to be cancelled — a model mid-thought.
func thinkThenHang(reasoning, content string, started chan<- struct{}) roundFn {
	return func(ctx context.Context, _ []llm.Message, out chan<- llm.StreamChunk) {
		out <- llm.StreamChunk{Reasoning: reasoning}
		if content != "" {
			out <- llm.StreamChunk{Content: content}
		}
		close(started)
		<-ctx.Done()
		select {
		case out <- llm.StreamChunk{Error: ctx.Err().Error()}:
		case <-time.After(time.Second):
		}
	}
}

func startScripted(t *testing.T, r *scriptRunner) *Harness {
	t.Helper()
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []Server{}, Client: r,
		SessionFile: filepath.Join(dir, "s.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func entries(t *testing.T, h *Harness) []agent.Entry {
	t.Helper()
	es, err := h.store.Context(context.Background(), "dun")
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func lastOfKind(es []agent.Entry, kind agent.EntryKind) *agent.Entry {
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Kind == kind {
			return &es[i]
		}
	}
	return nil
}

// askAsync runs h.Ask on a goroutine; the channel gets its error.
func askAsync(h *Harness, task string) <-chan error {
	done := make(chan error, 1)
	go func() { _, err := h.Ask(context.Background(), task); done <- err }()
	return done
}

func waitErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not end after the interrupt")
		return nil
	}
}

// The core contract: Interrupt ends the turn with ErrInterrupted, nothing is
// retried, the partial reply and the marker are persisted, the reasoning goes
// in only as a short note, and the next turn's request carries both.
func TestInterrupt_SavesPartialAndMarker(t *testing.T) {
	started := make(chan struct{})
	long := strings.Repeat("I keep going around in circles on this. ", 200) + "Maybe the cache is stale"
	r := &scriptRunner{rounds: []roundFn{thinkThenHang(long, "Hello, I will start by", started)}}
	h := startScripted(t, r)

	if h.Interrupt("") {
		t.Fatal("Interrupt with no turn running must report false")
	}
	done := askAsync(h, "fix the bug")
	<-started
	if !h.Interrupt("") {
		t.Fatal("Interrupt during a turn must report true")
	}
	if err := waitErr(t, done); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	if n := r.calls(); n != 1 {
		t.Fatalf("the model was called %d times; an interrupt must not be retried", n)
	}

	es := entries(t, h)
	a := lastOfKind(es, agent.KindAssistant)
	if a == nil || a.Content != "Hello, I will start by" {
		t.Fatalf("partial reply not saved: %+v", a)
	}
	n := lastOfKind(es, agent.KindNotification)
	if n == nil || n.Tag != interruptTag || !strings.HasPrefix(n.Content, "[the user interrupted this turn]") {
		t.Fatalf("marker not saved: %+v", n)
	}
	if len(n.Content) > 800 {
		t.Errorf("the reasoning note is %d chars; it must be short:\n%s", len(n.Content), n.Content)
	}
	if !strings.Contains(n.Content, "Maybe the cache is stale") || strings.Contains(n.Content, "circles") {
		t.Errorf("the note should quote the LAST sentence only:\n%s", n.Content)
	}
	if h.Pending() != 0 {
		t.Errorf("an interrupt alone must not schedule a turn: pending %d", h.Pending())
	}

	// The next turn sees the partial and the marker, then the new message.
	if _, err := h.Ask(context.Background(), "try the other file"); err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, m := range r.lastSent() {
		seen = append(seen, m.Role+": "+m.Content)
	}
	all := strings.Join(seen, "\n")
	ip := strings.Index(all, "assistant: Hello, I will start by")
	im := strings.Index(all, "[the user interrupted this turn]")
	iu := strings.Index(all, "try the other file")
	if ip < 0 || im < ip || iu < im {
		t.Fatalf("next request lacks partial → marker → message in order:\n%s", all)
	}
	if strings.Contains(all, "circles") {
		t.Errorf("the reasoning text itself was sent back:\n%s", all)
	}
}

// An interrupt with a reason (dund restarting) records that reason in the
// marker, not "the user interrupted", and does not tell the model to wait
// for the user.
func TestInterrupt_ReasonIsRecorded(t *testing.T) {
	started := make(chan struct{})
	r := &scriptRunner{rounds: []roundFn{thinkThenHang("thinking", "Partial", started)}}
	h := startScripted(t, r)
	done := askAsync(h, "go")
	<-started
	h.Interrupt("dund restarted into build v2 while this turn was running")
	if err := waitErr(t, done); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v", err)
	}
	n := lastOfKind(entries(t, h), agent.KindNotification)
	if n == nil || !strings.HasPrefix(n.Content, "[dund restarted into build v2 while this turn was running]") ||
		strings.Contains(n.Content, "the user") {
		t.Fatalf("marker: %+v", n)
	}
}

// A thought cut before any reply text leaves no empty assistant entry, only
// the marker with the note.
func TestInterrupt_ReasoningOnly(t *testing.T) {
	started := make(chan struct{})
	r := &scriptRunner{rounds: []roundFn{thinkThenHang("Let me think. This is wrong", "", started)}}
	h := startScripted(t, r)
	done := askAsync(h, "go")
	<-started
	h.Interrupt("")
	waitErr(t, done)
	es := entries(t, h)
	if a := lastOfKind(es, agent.KindAssistant); a != nil {
		t.Fatalf("no reply was streamed, yet an assistant entry was saved: %+v", a)
	}
	n := lastOfKind(es, agent.KindNotification)
	if n == nil || !strings.Contains(n.Content, `"This is wrong"`) {
		t.Fatalf("marker: %+v", n)
	}
}

// A tool call in flight is closed as interrupted by the user; a later call
// of the same round is not run; a message typed during the turn is not lifted
// into either result — it stays pending and starts the next turn.
func TestInterrupt_ToolInFlightAndQueued(t *testing.T) {
	r := &scriptRunner{rounds: []roundFn{
		func(_ context.Context, _ []llm.Message, out chan<- llm.StreamChunk) {
			for _, id := range []string{"c1", "c2"} {
				tc := &llm.ToolCall{ID: id, Type: "function"}
				tc.Function.Name, tc.Function.Arguments = "slow", `{}`
				out <- llm.StreamChunk{ToolCall: tc}
			}
			out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
		},
	}}
	h := startScripted(t, r)
	inTool := make(chan struct{})
	var ran int
	h.Session.Dispatch = withInterrupt(withLiftedQueue(func(ctx context.Context, tc llm.ToolCall) (string, error) {
		ran++
		close(inTool)
		<-ctx.Done()
		return "", ctx.Err()
	}, h))

	done := askAsync(h, "run it")
	<-inTool
	h.Say("actually, stop and look at main.go")
	h.Interrupt("")
	if err := waitErr(t, done); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v", err)
	}
	if ran != 1 {
		t.Fatalf("dispatched %d calls; the one after the interrupt must not run", ran)
	}
	results := map[string]string{}
	for _, e := range entries(t, h) {
		if e.Kind == agent.KindToolResult {
			results[e.ToolCallID] = e.Content
		}
	}
	if !strings.Contains(results["c1"], "INTERRUPTED by the user") {
		t.Errorf("c1: %q", results["c1"])
	}
	if !strings.Contains(results["c2"], "not run") {
		t.Errorf("c2: %q", results["c2"])
	}
	for id, c := range results {
		if strings.Contains(c, "main.go") {
			t.Errorf("the queued message was lifted into %s's result: %q", id, c)
		}
	}
	if h.Pending() == 0 {
		t.Fatal("the queued message must be pending, to start the next turn")
	}
	if _, err := h.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	var last string
	for _, m := range r.lastSent() {
		if m.Role == "user" {
			last = m.Content
		}
	}
	if !strings.Contains(last, "actually, stop and look at main.go") {
		t.Fatalf("the next turn did not get the queued message; last user message %q", last)
	}
}

// A call left without a result (the turn was cut between persisting it and
// dispatching it) is healed with the user's wording, silently.
func TestInterrupt_HealsOrphanSilently(t *testing.T) {
	h := startScripted(t, &scriptRunner{})
	h.store.appendSilent(agent.Entry{ID: "x", Kind: agent.KindToolCall, ToolCallID: "c9", ToolName: "exec", Content: "{}"})
	h.round.reset()
	h.recordInterrupt(context.Background())
	r := lastOfKind(entries(t, h), agent.KindToolResult)
	if r == nil || r.ToolCallID != "c9" || !strings.Contains(r.Content, "INTERRUPTED by the user") {
		t.Fatalf("orphan not healed: %+v", r)
	}
	if h.Pending() != 0 {
		t.Fatalf("healing an interrupt must not schedule a turn: pending %d", h.Pending())
	}
}

func TestLastSentence(t *testing.T) {
	for in, want := range map[string]string{
		"One. Two three.":     "Two three",
		"no stop at all":      "no stop at all",
		"pi is 3.14 exactly.": "pi is 3.14 exactly",
		"First!\nSecond?\n":   "Second",
	} {
		if got := lastSentence(in, 200); got != want {
			t.Errorf("lastSentence(%q) = %q, want %q", in, got, want)
		}
	}
	if got := lastSentence(strings.Repeat("a", 500), 10); len([]rune(got)) != 10 || !strings.HasPrefix(got, "…") {
		t.Errorf("long: %q", got)
	}
}
