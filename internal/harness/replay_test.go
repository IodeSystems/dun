package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/dun/internal/mcpserver"
)

// replayRunner makes call on the person's "go now", and answers a tool
// result with text that quotes it. It keeps every request.
type replayRunner struct {
	mu     sync.Mutex
	call   *llm.ToolCall
	called bool
	sent   [][]llm.Message
}

func (r *replayRunner) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.mu.Lock()
	r.sent = append(r.sent, msgs)
	r.mu.Unlock()
	out := make(chan llm.StreamChunk, 2)
	defer close(out)
	last := msgs[len(msgs)-1]
	var tool *llm.Message
	for i := range msgs {
		if msgs[i].Role == "tool" {
			tool = &msgs[i]
		}
	}
	switch {
	case tool != nil:
		out <- llm.StreamChunk{Content: "got: " + tool.Content}
	case !r.called && strings.Contains(last.Content, "go now"):
		r.called = true
		out <- llm.StreamChunk{ToolCall: r.call}
	default:
		out <- llm.StreamChunk{Content: "ok"}
	}
	out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
	return out, nil
}

func (r *replayRunner) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (r *replayRunner) requests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

// seenAsk is what an Ask saw: the question, its ids from ctx.
type seenAsk struct {
	q, askID, callID string
}

func startReplay(t *testing.T, r *replayRunner, ws, sf string, ask AskFunc) *Harness {
	t.Helper()
	cfg := Config{Workspace: ws, Servers: []mcpserver.Server{}, Client: r, Exec: &HostShell{Dir: ws},
		SessionFile: sf, PlanNudgeRounds: -1, PlanNudgeFails: -1, PlanStallRounds: -1, SettleDelay: -1, Ask: ask}
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// suspendAsk is the engine's Ask when its stop comes while the question is
// open: it reports what it was asked and returns ErrSuspended.
func suspendAsk(seen *[]seenAsk) AskFunc {
	return func(ctx context.Context, q string, _ []string, _, _ bool, _ []string) (string, error) {
		id := ""
		if tc := CallFrom(ctx); tc != nil {
			id = tc.ID
		}
		*seen = append(*seen, seenAsk{q, AskIDFrom(ctx), id})
		return "", ErrSuspended
	}
}

func answerAsk(seen *[]seenAsk, ans string) AskFunc {
	return func(ctx context.Context, q string, _ []string, _, _ bool, _ []string) (string, error) {
		id := ""
		if tc := CallFrom(ctx); tc != nil {
			id = tc.ID
		}
		*seen = append(*seen, seenAsk{q, AskIDFrom(ctx), id})
		return ans, nil
	}
}

func resultFor(t *testing.T, h *Harness, id string) []agent.Entry {
	t.Helper()
	entries, err := h.store.Context(context.Background(), "dun")
	if err != nil {
		t.Fatal(err)
	}
	var out []agent.Entry
	for _, e := range entries {
		if e.Kind == agent.KindToolResult && e.ToolCallID == id {
			out = append(out, e)
		}
	}
	return out
}

const askArgs = `{"question":"which one?","options":["a","b"],"recommended":"a"}`

// askStopped runs "go now" to an ask_user call the engine's stop suspends,
// and closes that engine. It returns the session file.
func askStopped(t *testing.T, r *replayRunner, ws string) (sf string, first seenAsk) {
	t.Helper()
	sf = filepath.Join(t.TempDir(), "s.jsonl")
	var seen []seenAsk
	h := startReplay(t, r, ws, sf, suspendAsk(&seen))
	_, err := h.Ask(context.Background(), "go now")
	if !errors.Is(err, ErrSuspended) {
		t.Fatalf("turn ended %v, want ErrSuspended", err)
	}
	if got := resultFor(t, h, r.call.ID); len(got) != 0 {
		t.Fatalf("the suspended call has a result: %+v", got)
	}
	if h.PendingReplay() != nil {
		t.Error("the engine that suspended it would replay it itself")
	}
	h.Close()
	if len(seen) != 1 || seen[0].callID != r.call.ID || seen[0].askID == "" {
		t.Fatalf("asked %+v", seen)
	}
	if !QuestionPending(sf) {
		t.Fatal("QuestionPending false after the stop")
	}
	return sf, seen[0]
}

// A question open when the engine stopped is asked again by the next one,
// with the same ask id and call id; the answer is that call's result and
// the turn goes on from it with no new user message.
func TestReplay_AskAgainAndContinue(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	r := &replayRunner{call: nudgeCall("q1", "ask_user", askArgs)}
	sf, first := askStopped(t, r, ws)
	before := r.requests()

	var seen []seenAsk
	h := startReplay(t, r, ws, sf, answerAsk(&seen, "b"))
	defer h.Close()
	p := h.PendingReplay()
	if p == nil || p.CallID != "q1" || p.Question != "which one?" || p.AskID != first.askID {
		t.Fatalf("pending %+v", p)
	}
	res, ran, err := h.Replay(context.Background())
	if !ran || err != nil {
		t.Fatalf("replay ran %v err %v", ran, err)
	}
	if len(seen) != 1 || seen[0] != first {
		t.Fatalf("asked again %+v, want %+v", seen, first)
	}
	got := resultFor(t, h, "q1")
	if len(got) != 1 || got[0].Content != "b" {
		t.Fatalf("result %+v", got)
	}
	// The next request carries the answer as q1's result, with no user
	// message after it.
	if r.requests() <= before {
		t.Fatalf("no model round after the answer")
	}
	r.mu.Lock()
	next := r.sent[before]
	r.mu.Unlock()
	if m := next[len(next)-1]; m.Role != "tool" || m.Content != "b" || m.ToolCallID != "q1" {
		t.Errorf("the request after the answer ends with %+v", m)
	}
	if res.Reply != "got: b" {
		t.Errorf("reply %q", res.Reply)
	}
	if _, err := os.Stat(PendingFile(sf)); !os.IsNotExist(err) {
		t.Errorf("sidecar left: %v", err)
	}
	if _, ran, _ := h.Replay(context.Background()); ran {
		t.Error("replayed twice")
	}
}

// An interrupt instead of an answer cancels the question: its result says
// so and the turn ends, with no model round after it.
func TestReplay_InterruptCancels(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	r := &replayRunner{call: nudgeCall("q1", "ask_user", askArgs)}
	sf, _ := askStopped(t, r, ws)
	before := r.requests()

	var h *Harness
	asked := make(chan struct{})
	h = startReplay(t, r, ws, sf, func(ctx context.Context, _ string, _ []string, _, _ bool, _ []string) (string, error) {
		close(asked)
		<-ctx.Done()
		return "", ctx.Err()
	})
	defer h.Close()
	go func() {
		<-asked
		h.Interrupt("")
	}()
	_, ran, err := h.Replay(context.Background())
	if !ran || !errors.Is(err, ErrInterrupted) {
		t.Fatalf("replay ran %v err %v", ran, err)
	}
	got := resultFor(t, h, "q1")
	if len(got) != 1 || !strings.Contains(got[0].Content, "cancelled by the user") {
		t.Fatalf("result %+v", got)
	}
	if r.requests() != before {
		t.Errorf("a model round ran after the cancel: %d requests, want %d", r.requests(), before)
	}
	if QuestionPending(sf) || h.PendingReplay() != nil {
		t.Error("still pending after the cancel")
	}
}

// The file-access question is replayed too: the file call runs again, asks
// again, and with a yes reads the file.
func TestReplay_FileAccess(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws, other := t.TempDir(), t.TempDir()
	outside := filepath.Join(other, "notes.txt")
	if err := os.WriteFile(outside, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &replayRunner{call: nudgeCall("f1", "file_read", `{"path":"`+outside+`"}`)}
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	var seen []seenAsk
	h := startReplay(t, r, ws, sf, suspendAsk(&seen))
	if _, err := h.Ask(context.Background(), "go now"); !errors.Is(err, ErrSuspended) {
		t.Fatalf("turn ended %v", err)
	}
	h.Close()
	if len(seen) != 1 || !strings.Contains(seen[0].q, outside) || seen[0].callID != "f1" {
		t.Fatalf("asked %+v", seen)
	}

	var again []seenAsk
	h2 := startReplay(t, r, ws, sf, answerAsk(&again, accessSession))
	defer h2.Close()
	if _, ran, err := h2.Replay(context.Background()); !ran || err != nil {
		t.Fatalf("replay ran %v err %v", ran, err)
	}
	if len(again) != 1 || again[0] != seen[0] {
		t.Fatalf("asked again %+v, want %+v", again, seen)
	}
	got := resultFor(t, h2, "f1")
	if len(got) != 1 || !strings.Contains(got[0].Content, "alpha") {
		t.Fatalf("result %+v", got)
	}
}

// A call that is not replayable, cut by the stop, gets the failed result
// that tells the model to try again.
func TestReplay_OtherCallsFail(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	r := &replayRunner{}
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	h := startReplay(t, r, ws, sf, answerAsk(new([]seenAsk), "a"))
	defer h.Close()
	ctx := context.Background()
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "1", Kind: agent.KindUser, Content: "build"})
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "2", Kind: agent.KindToolCall, Content: `{"command":"go build"}`,
		ToolCallID: "x1", ToolName: "exec", CreatedAt: time.Now().UnixNano()})
	if h.PendingReplay() != nil {
		t.Fatal("exec replayed")
	}
	h.prepareTurn(ctx)
	got := resultFor(t, h, "x1")
	if len(got) != 1 || !strings.HasPrefix(got[0].Content, "ERROR: this exec call was cut off by a restart") ||
		!strings.Contains(got[0].Content, "Try it again") || !ToolFailed("exec", got[0].Content) {
		t.Fatalf("result %+v", got)
	}
}

// /until: a run paused on a question stays paused across the restart, and
// the answer to the question asked again goes on with it.
func TestReplay_UntilPausedResumes(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	r := &replayRunner{call: nudgeCall("q1", "ask_user", askArgs)}
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	var seen []seenAsk
	h := startReplay(t, r, ws, sf, suspendAsk(&seen))
	untilSet(t, h, "turns 5")
	if _, err := h.Ask(context.Background(), "go now"); !errors.Is(err, ErrSuspended) {
		t.Fatalf("turn ended %v", err)
	}
	if s := h.Until(); !s.Paused || !s.Running {
		t.Fatalf("after the stop: %+v", s)
	}
	h.Close()

	var h2 *Harness
	var during UntilSnapshot
	h2 = startReplay(t, r, ws, sf, func(context.Context, string, []string, bool, bool, []string) (string, error) {
		during = h2.Until()
		return "b", nil
	})
	defer h2.Close()
	if s := h2.Until(); !s.Paused || !s.Running || s.Turn != 1 {
		t.Fatalf("resumed %+v", s)
	}
	if st := h2.UntilNext(context.Background()); st.Continue || st.Stopped != "" {
		t.Errorf("owed a turn while paused: %+v", st)
	}
	if _, ran, err := h2.Replay(context.Background()); !ran || err != nil {
		t.Fatalf("replay ran %v err %v", ran, err)
	}
	if !during.Paused || !during.Running {
		t.Errorf("while asked again: %+v", during)
	}
	if s := h2.Until(); s.Paused || !s.Running {
		t.Errorf("after the answer: %+v", s)
	}
	if st := h2.UntilNext(context.Background()); !st.Continue {
		t.Errorf("the run does not go on: %+v", st)
	}
}

// A stale sidecar (its call answered, or gone with a /clear) replays nothing
// and is removed.
func TestReplay_StaleRecordDropped(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(PendingFile(sf), []byte(`{"ask_id":"a","call_id":"gone","tool":"ask_user","args":"{}"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if QuestionPending(sf) {
		t.Error("QuestionPending for a call not in the transcript")
	}
	h := startReplay(t, &replayRunner{}, ws, sf, answerAsk(new([]seenAsk), "a"))
	defer h.Close()
	if h.PendingReplay() != nil {
		t.Fatal("stale record replayed")
	}
	if _, err := os.Stat(PendingFile(sf)); !os.IsNotExist(err) {
		t.Errorf("stale sidecar kept: %v", err)
	}
}
