package dun

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// A child asks its parent by ENDING its turn with the question (USER
// 2026-10-27). These tests spawn a real child on a scripted model: the turn
// end is the only delivery path, so it has to be the real one.

// textRound is a model round that replies with text and calls nothing.
func textRound(text string) roundFn {
	return func(_ context.Context, _ []llm.Message, out chan<- llm.StreamChunk) {
		out <- llm.StreamChunk{Content: text}
		out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
	}
}

// startParentOf starts a root harness whose children run on child.
func startParentOf(t *testing.T, child *scriptRunner) *Harness {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []Server{}, Client: &scriptRunner{},
		ClientFor:   func(string) (agent.LLMRunner, error) { return child, nil },
		SessionFile: filepath.Join(dir, "s.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func spawnKid(t *testing.T, h *Harness, prompt string) *subAgent {
	t.Helper()
	sa, err := h.spawnAgent(context.Background(), prompt, "kid", "")
	if err != nil {
		t.Fatal(err)
	}
	return sa
}

func toolNamesOf(h *Harness) []string {
	var out []string
	for _, d := range h.Session.Tools {
		out = append(out, d.Function.Name)
	}
	return out
}

func TestSubAgent_ToolListHasTellParentNotAskParent(t *testing.T) {
	h := startParentOf(t, &scriptRunner{rounds: []roundFn{textRound("done")}})
	sa := spawnKid(t, h, "say done")
	waitIdle(t, sa)
	names := strings.Join(toolNamesOf(sa.h), " ")
	if !strings.Contains(names, "tell_parent") {
		t.Errorf("a child must have tell_parent: %s", names)
	}
	if strings.Contains(names, "ask_parent") {
		t.Errorf("ask_parent must not be listed: %s", names)
	}
	if role := roleSystem(true); !strings.Contains(role, "end your turn with the question") {
		t.Errorf("the child's role must say how to ask: %q", role)
	}
}

// The whole exchange: the child's turn ends with a question, the question
// reaches the parent as a notification, the parent's tell starts the child's
// next turn, and the child sees the answer with its context intact.
func TestSubAgent_QuestionIsATurnEndAndTellAnswersIt(t *testing.T) {
	var second []llm.Message
	child := &scriptRunner{rounds: []roundFn{
		textRound("Which season should the haiku be about?"),
		func(ctx context.Context, msgs []llm.Message, out chan<- llm.StreamChunk) {
			second = msgs
			textRound("Snow on the pine")(ctx, msgs, out)
		},
	}}
	h := startParentOf(t, child)
	sa := spawnKid(t, h, "write a haiku; ask your parent which season first")
	waitIdle(t, sa)

	var note string
	for _, n := range h.notes() {
		if strings.Contains(n, "agent #1 is idle") {
			note = n
		}
	}
	if !strings.Contains(note, "Which season should the haiku be about?") {
		t.Fatalf("the parent must get the question's text, not just 'idle': %q", note)
	}
	if !strings.Contains(note, `jobs(agent:1, tell:"…")`) {
		t.Errorf("the notification must say how to answer: %q", note)
	}

	id := sa.num
	out := agentMonitor(context.Background(), h, &id, 0, "winter", true, false, false)
	if !strings.Contains(out, "Told agent #1") || !strings.Contains(out, "Snow on the pine") {
		t.Fatalf("tell must start the next turn and wait must report it: %s", out)
	}
	if len(second) == 0 {
		t.Fatal("the child never ran a second turn")
	}
	if last := second[len(second)-1]; last.Role != "user" || !strings.Contains(last.Content, "winter") {
		t.Errorf("the child's next message must be the answer, got %s: %q", last.Role, last.Content)
	}
	if !strings.Contains(sentText(second), "Which season should the haiku be about?") {
		t.Errorf("the child must keep its context across the answer: %s", sentText(second))
	}
}

// ask_parent is a hidden alias: an old-style call makes the question the
// child's report and tells it to end its turn. It does not block.
func TestSubAgent_AskParentAliasReportsTheQuestion(t *testing.T) {
	var after []llm.Message
	child := &scriptRunner{rounds: []roundFn{
		func(_ context.Context, _ []llm.Message, out chan<- llm.StreamChunk) {
			tc := &llm.ToolCall{ID: "q1", Type: "function"}
			tc.Function.Name, tc.Function.Arguments = "ask_parent", `{"question":"Which season?"}`
			out <- llm.StreamChunk{ToolCall: tc}
			out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
		},
		func(ctx context.Context, msgs []llm.Message, out chan<- llm.StreamChunk) {
			after = msgs
			textRound("")(ctx, msgs, out)
		},
	}}
	h := startParentOf(t, child)
	sa := spawnKid(t, h, "write a haiku")
	waitIdle(t, sa)

	if len(after) == 0 || !strings.Contains(after[len(after)-1].Content, "End your turn now") {
		t.Errorf("the alias's result must tell the child to end its turn: %v", after)
	}
	var note string
	for _, n := range h.notes() {
		if strings.Contains(n, "agent #1 is idle") {
			note = n
		}
	}
	if !strings.Contains(note, "answer: Which season?") {
		t.Errorf("the alias's question must be the child's report: %q", note)
	}
}
