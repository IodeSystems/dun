package harness

import (
	"context"
	"path/filepath"
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
	sa.harness().Gate().End() // the run settled at its gate (work)
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

// A child takes images as the session does, through the same gate
// (turngate.go): to a working child the message is held and starts the
// turn after the running one; to an idle child it starts a run.

// hasImage reports whether a round was sent an image part.
func hasImage(msgs []llm.Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == "image_url" && p.ImageURL != nil {
				return true
			}
		}
	}
	return false
}

// partsText is the text parts a round was sent (a message with images
// carries its text there, not in Content).
func partsText(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, p := range m.Parts {
			b.WriteString(p.Text + "\n")
		}
	}
	return b.String()
}

// kidImage writes a PNG and returns the message that attaches it.
func kidImage(t *testing.T, text string) UserInput {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.png")
	writePNG(t, path, 4, 4, false)
	label := "[image 1: a.png]"
	return UserInput{Text: text + " " + label, Images: []ImageInput{{Path: path, Label: label}}}
}

func TestSubAgent_ImageToABusyChild(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var second []llm.Message
	child := &scriptRunner{rounds: []roundFn{
		gated(textRound("the first answer"), started, release),
		seen(&mu, &second, textRound("it is a square")),
	}}
	h := startParentOf(t, child)
	sa := spawnKid(t, h, "do the first thing")
	<-started
	in := kidImage(t, "what is this")
	if out := h.AgentTellImages(1, in.Text, in.Images); !strings.HasPrefix(out, "Queued for agent #1") {
		t.Fatalf("a working child holds the image for its next turn: %s", out)
	}
	close(release)
	waitIdle(t, sa)
	mu.Lock()
	got, img := partsText(second), hasImage(second)
	mu.Unlock()
	if !img || !strings.Contains(got, "what is this") {
		t.Errorf("the next turn must carry the image and its text (image %v): %s", img, got)
	}
	if r := sa.report(); !strings.Contains(r, "it is a square") {
		t.Errorf("the report must answer the image: %s", r)
	}
	if n := child.calls(); n != 2 {
		t.Errorf("one turn each: %d rounds", n)
	}
}

func TestSubAgent_ImageToAnIdleChild(t *testing.T) {
	var mu sync.Mutex
	var second []llm.Message
	child := &scriptRunner{rounds: []roundFn{
		textRound("first"),
		seen(&mu, &second, textRound("a square")),
	}}
	h := startParentOf(t, child)
	sa := spawnKid(t, h, "do a thing")
	waitIdle(t, sa)
	in := kidImage(t, "and this")
	if out := h.AgentTellImages(1, in.Text, in.Images); !strings.HasPrefix(out, "Told agent #1") {
		t.Fatalf("an idle child starts a run: %s", out)
	}
	waitIdle(t, sa)
	mu.Lock()
	img := hasImage(second)
	mu.Unlock()
	if !img {
		t.Error("the run's turn must carry the image")
	}
	if r := sa.report(); !strings.Contains(r, "a square") {
		t.Errorf("report: %s", r)
	}
}

// failRound is a round the provider fails.
func failRound(_ context.Context, _ []llm.Message, out chan<- llm.StreamChunk) {
	out <- llm.StreamChunk{Error: "401 bad key", Done: true}
}

// A message queued while the child's turn fails is delivered and starts a
// turn, as the session's queued message is after an error.
func TestSubAgent_TellQueuedWhenTheChildErrors(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var second []llm.Message
	child := &scriptRunner{rounds: []roundFn{
		gated(failRound, started, release),
		seen(&mu, &second, textRound("recovered")),
	}}
	h := startParentOf(t, child)
	sa := spawnKid(t, h, "do the first thing")
	<-started
	if out := sa.tell("try again"); !strings.HasPrefix(out, "Queued for agent #1") {
		t.Fatalf("tell: %s", out)
	}
	close(release)
	waitIdle(t, sa)
	mu.Lock()
	got := llmText(second)
	mu.Unlock()
	if !strings.Contains(got, "try again") {
		t.Errorf("the turn after the error must read the message: %s (rounds %d)", got, child.calls())
	}
	if r := sa.report(); !strings.Contains(r, "recovered") || strings.Contains(r, "FAILED") {
		t.Errorf("the report is the delivered message's answer: %s", r)
	}
}

// With nothing queued, a failed run ends as before: no retry of its own.
func TestSubAgent_ErrorWithNothingQueuedEnds(t *testing.T) {
	child := &scriptRunner{rounds: []roundFn{failRound}}
	h := startParentOf(t, child)
	sa := spawnKid(t, h, "do the first thing")
	waitIdle(t, sa)
	if r := sa.report(); !strings.Contains(r, "FAILED") {
		t.Errorf("report: %s", r)
	}
	if n := child.calls(); n != 1 {
		t.Errorf("no retry without news: %d rounds", n)
	}
}

// News nobody typed gets at most autoContinues turns a run; a message
// queued once the bound is used is still delivered and starts a turn.
func TestSubAgent_TellAfterTheAutoBoundStartsATurn(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var kid *Harness // set before release
	var sa *subAgent
	news := func(round roundFn) roundFn {
		return func(ctx context.Context, msgs []llm.Message, out chan<- llm.StreamChunk) {
			kid.Notify("a job finished")
			round(ctx, msgs, out)
		}
	}
	rounds := []roundFn{gated(news(textRound("first")), started, release)}
	for i := 0; i < autoContinues-1; i++ {
		rounds = append(rounds, news(textRound("noted")))
	}
	// The last automatic turn: the user speaks during it.
	rounds = append(rounds, func(ctx context.Context, msgs []llm.Message, out chan<- llm.StreamChunk) {
		kid.Notify("a job finished")
		if r := sa.tell("the user's word"); !strings.HasPrefix(r, "Queued for agent #1") {
			t.Errorf("tell: %s", r)
		}
		textRound("noted")(ctx, msgs, out)
	})
	var mu sync.Mutex
	var heard []llm.Message
	rounds = append(rounds, seen(&mu, &heard, news(textRound("heard you"))))
	child := &scriptRunner{rounds: rounds}
	h := startParentOf(t, child)
	sa = spawnKid(t, h, "watch the jobs")
	<-started
	kid = sa.harness()
	close(release)
	waitIdle(t, sa)
	mu.Lock()
	got := llmText(heard)
	mu.Unlock()
	if !strings.Contains(got, "the user's word") {
		t.Errorf("the message after the bound must get a turn: %s", got)
	}
	if r := sa.report(); !strings.Contains(r, "heard you") {
		t.Errorf("report: %s", r)
	}
	// 1 + autoContinues automatic + 1 for the message; the news its last
	// round brought gets no turn: the bound holds.
	if n, want := child.calls(), autoContinues+2; n != want {
		t.Errorf("%d rounds, want %d", n, want)
	}
}
