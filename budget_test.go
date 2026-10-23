package dun

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// toolRound is a model round that makes one call to name.
func toolRound(id, name string) roundFn {
	return func(_ context.Context, _ []llm.Message, out chan<- llm.StreamChunk) {
		tc := &llm.ToolCall{ID: id, Type: "function"}
		tc.Function.Name, tc.Function.Arguments = name, `{}`
		out <- llm.StreamChunk{ToolCall: tc}
		out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
	}
}

// sentText is one request as text, for counting what the model was shown.
func sentText(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role + ": " + m.Content + "\n")
	}
	return b.String()
}

func TestBudgetDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Minute: "30m", 6 * time.Minute: "6m", 90 * time.Second: "1m30s",
		2 * time.Hour: "2h", 45 * time.Second: "45s", 90 * time.Minute: "1h30m",
	} {
		if got := BudgetDur(d); got != want {
			t.Errorf("BudgetDur(%s) = %q, want %q", d, got, want)
		}
	}
}

// The budget running out while a tool runs does not disturb the tool: the
// notice reaches the model with its NEXT call, once, and the turn after drops
// it (it is about the turn it was sent in).
func TestBudgetNotice_NextCallOnce(t *testing.T) {
	r := &scriptRunner{rounds: []roundFn{toolRound("c1", "slow"), toolRound("c2", "slow")}}
	h := startScripted(t, r)
	calls := 0
	h.Session.Dispatch = withInterrupt(func(ctx context.Context, tc llm.ToolCall) (string, error) {
		calls++
		if calls == 1 {
			h.NoteBudget(30*time.Minute, 6*time.Minute) // the clock, mid-call
		}
		return "done", nil
	})
	if _, err := h.Ask(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	if n := r.calls(); n != 3 {
		t.Fatalf("model called %d times, want 3", n)
	}
	const notice = "This turn has used its 30m time budget. dun cuts the turn off in 6m."
	r.mu.Lock()
	first, second, third := sentText(r.sent[0]), sentText(r.sent[1]), sentText(r.sent[2])
	r.mu.Unlock()
	if strings.Contains(first, notice) {
		t.Fatal("the notice was sent before the budget ran out")
	}
	if strings.Count(second, notice) != 1 {
		t.Fatalf("the call after the budget ran out has the notice %d times, want 1:\n%s", strings.Count(second, notice), second)
	}
	if strings.Count(third, notice) != 1 {
		t.Fatalf("the notice was re-injected (or lost): %d copies in the next call", strings.Count(third, notice))
	}
	// The tool result came first, the notice after it: never between a call
	// and its result.
	if i, j := strings.Index(second, "tool: done"), strings.Index(second, notice); i < 0 || j < i {
		t.Errorf("notice is not after the running call's result:\n%s", second)
	}

	if _, err := h.Ask(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sentText(r.lastSent()), notice) {
		t.Fatal("the next turn still carries the last turn's budget notice")
	}
}

// A notice queued for a turn that ended before its next call is never
// delivered to a later turn.
func TestBudgetNotice_StaleIsDropped(t *testing.T) {
	r := &scriptRunner{}
	h := startScripted(t, r)
	h.NoteBudget(30*time.Minute, 6*time.Minute)
	if _, err := h.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sentText(r.lastSent()), "time budget") {
		t.Fatal("a stale budget notice reached a new turn")
	}
}

// cutWith runs a turn under a parent context and cuts it the way the clock
// does: note, then cancel with the budget as the cause.
func cutWith(h *Harness, inTurn <-chan struct{}) <-chan error {
	parent, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { _, err := h.Ask(parent, "work"); done <- err }()
	go func() {
		<-inTurn
		h.NoteBudgetCut(30*time.Minute, 6*time.Minute)
		cancel(BudgetCut{Budget: 30 * time.Minute, Grace: 6 * time.Minute})
	}()
	return done
}

// A cut during a tool call: the call's result names the budget (not the user),
// and the next turn starts with one line naming the call. The line is not
// repeated in the turns after. The recorded turn error stays out of context.
func TestBudgetCut_NextTurnNoteOnce(t *testing.T) {
	r := &scriptRunner{rounds: []roundFn{toolRound("c1", "exec")}}
	h := startScripted(t, r)
	inTool := make(chan struct{})
	h.Session.Dispatch = withInterrupt(func(ctx context.Context, tc llm.ToolCall) (string, error) {
		close(inTool)
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err := waitErr(t, cutWith(h, inTool)); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	h.RecordTurnError(BudgetCut{Budget: 30 * time.Minute, Grace: 6 * time.Minute})

	res := lastOfKind(entries(t, h), agent.KindToolResult)
	if res == nil || !strings.Contains(res.Content, "ran past its 30m budget plus 6m grace") ||
		strings.Contains(res.Content, "by the user") {
		t.Fatalf("cut call's result: %+v", res)
	}

	const note = "[dun] Your previous turn was cut off after its 30m budget plus 6m grace, during the exec call."
	for i, task := range []string{"go on", "and again"} {
		if _, err := h.Ask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		sent := sentText(r.lastSent())
		if n := strings.Count(sent, note); n != 1 {
			t.Fatalf("turn %d after the cut has the note %d times, want 1:\n%s", i+1, n, sent)
		}
		if !strings.Contains(sent, "Check the state before you continue.") {
			t.Errorf("the note does not say to check the state:\n%s", sent)
		}
		if strings.Contains(sent, "and was cut off") {
			t.Fatalf("the recorded turn error reached the model:\n%s", sent)
		}
		if i == 0 && strings.Index(sent, note) > strings.Index(sent, "go on") {
			t.Errorf("the note comes after the user's message:\n%s", sent)
		}
	}
}

// A cut while the model streams names the model step.
func TestBudgetCut_DuringModelStep(t *testing.T) {
	started := make(chan struct{})
	r := &scriptRunner{rounds: []roundFn{thinkThenHang("hmm", "Partial", started)}}
	h := startScripted(t, r)
	if err := waitErr(t, cutWith(h, started)); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v", err)
	}
	n := lastOfKind(entries(t, h), agent.KindNotification)
	if n == nil || !strings.HasPrefix(n.Content, "[dun] Your previous turn was cut off after its 30m budget plus 6m grace, during a model step") {
		t.Fatalf("note: %+v", n)
	}
}
