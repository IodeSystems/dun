package harness

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// A working child is told the way the main session is told mid-turn: the
// message is queued on its harness and read at its next round boundary,
// and its one report covers it (docs/decisions.md, "Telling a working
// sub-agent").

// gated is round, run only once release is closed; started closes when the
// round begins, so the test knows the child is mid-round.
func gated(round roundFn, started, release chan struct{}) roundFn {
	return func(ctx context.Context, msgs []llm.Message, out chan<- llm.StreamChunk) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		round(ctx, msgs, out)
	}
}

// seen records the messages a round was sent, then runs round.
func seen(mu *sync.Mutex, into *[]llm.Message, round roundFn) roundFn {
	return func(ctx context.Context, msgs []llm.Message, out chan<- llm.StreamChunk) {
		mu.Lock()
		*into = msgs
		mu.Unlock()
		round(ctx, msgs, out)
	}
}

// Told during its LAST round (no tool result to carry it): the run goes on
// with a turn for the message, and the report is that turn's answer.
func TestSubAgent_TellMidRunAfterLastRound(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var second []llm.Message
	child := &scriptRunner{rounds: []roundFn{
		gated(textRound("the first answer"), started, release),
		seen(&mu, &second, textRound("the steered answer")),
	}}
	h := startParentOf(t, child)
	sa := spawnKid(t, h, "do the first thing")
	<-started
	h.notes()
	if out := h.AgentTell(1, "do the other thing"); !strings.HasPrefix(out, "Queued for agent #1") {
		t.Fatalf("a working child queues the tell: %s", out)
	}
	close(release)
	waitIdle(t, sa)

	mu.Lock()
	got := llmText(second)
	mu.Unlock()
	if !strings.Contains(got, "do the other thing") || !strings.Contains(got, "the first answer") {
		t.Errorf("the second turn must read the message after the first: %s", got)
	}
	if r := sa.report(); !strings.Contains(r, "the steered answer") {
		t.Errorf("the report must be the answer to the steer: %s", r)
	}
	if n := child.calls(); n != 2 {
		t.Errorf("one turn each, %d rounds", n)
	}
	var told bool
	for _, n := range h.notes() {
		told = told || strings.Contains(n, `The user told agent #1 directly: "do the other thing"`)
	}
	if !told {
		t.Error("the parent must hear the user steered the child")
	}
}

// Told while it works through a tool call: the message rides that call's
// result, inside the same turn — no turn of its own.
func TestSubAgent_TellMidRunRidesTheToolResult(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var second []llm.Message
	child := &scriptRunner{rounds: []roundFn{
		gated(callRound("c1", "tell_parent", `{"status":"reading"}`), started, release),
		seen(&mu, &second, textRound("done, the other way")),
	}}
	h := startParentOf(t, child)
	sa := spawnKid(t, h, "do the first thing")
	<-started
	if out := sa.tell("do it the other way"); !strings.HasPrefix(out, "Queued for agent #1") {
		t.Fatalf("tell: %s", out)
	}
	close(release)
	waitIdle(t, sa)

	mu.Lock()
	got := llmText(second)
	mu.Unlock()
	if !strings.Contains(got, "do it the other way") {
		t.Errorf("the round after the tool must read the message: %s", got)
	}
	if n := child.calls(); n != 2 {
		t.Errorf("delivered inside the turn: want 2 rounds, got %d", n)
	}
	if r := sa.report(); !strings.Contains(r, "done, the other way") {
		t.Errorf("report: %s", r)
	}
}

// A tell right after a run's intake shut waits for the settle and starts
// the next run: it is never dropped between the two.
func TestSubAgent_TellAfterIntakeShutStartsTheNextRun(t *testing.T) {
	child := &scriptRunner{rounds: []roundFn{textRound("first"), textRound("second")}}
	h := startParentOf(t, child)
	sa := spawnKid(t, h, "do a thing")
	waitIdle(t, sa)
	// Hold it in the window between work's last look and the settle.
	sa.mu.Lock()
	sa.startRunLocked()
	sa.mu.Unlock()
	sa.shutIntake()
	go func() {
		time.Sleep(20 * time.Millisecond)
		sa.settle(agentIdle)
	}()
	if out := sa.tell("again"); !strings.HasPrefix(out, "Told agent #1") {
		t.Fatalf("after the settle, a new run: %s", out)
	}
	waitIdle(t, sa)
	if r := sa.report(); !strings.Contains(r, "second") {
		t.Errorf("report: %s", r)
	}
}
