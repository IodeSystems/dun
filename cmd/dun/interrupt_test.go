package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// hangRunner thinks, then waits for its context to be cancelled on the first
// call; every later call answers "ok" and records the messages it was sent.
type hangRunner struct {
	mu   sync.Mutex
	n    int
	last []llm.Message
}

func (r *hangRunner) ChatStream(ctx context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.mu.Lock()
	r.n++
	first := r.n == 1
	r.last = msgs
	r.mu.Unlock()
	out := make(chan llm.StreamChunk)
	go func() {
		defer close(out)
		if !first {
			out <- llm.StreamChunk{Content: "ok"}
			out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
			return
		}
		out <- llm.StreamChunk{Reasoning: "Hmm. This is a bad idea"}
		out <- llm.StreamChunk{Content: "Starting with"}
		<-ctx.Done()
		select {
		case out <- llm.StreamChunk{Error: ctx.Err().Error()}:
		case <-time.After(time.Second):
		}
	}()
	return out, nil
}

// The engine end to end: an `interrupt` command, encoded by proto, stops the
// turn; it ends with done{reason: interrupted}; the message queued during it
// starts the next turn at once, and that turn's request carries the partial,
// the marker and the message.
func TestEngineInterrupt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DUN_HOME", filepath.Join(home, ".dun"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := &hangRunner{}
	evR, evW := io.Pipe()
	em := &emitter{w: evW}
	h, err := dun.Start(ctx, dun.Config{
		Workspace: home, Servers: []dun.Server{}, Client: r,
		SessionFile: filepath.Join(home, "s.jsonl"),
		OnToken:     func(s string) { em.emit(&proto.Token{Text: s}) },
		OnReasoning: func(s string) { em.emit(&proto.Reasoning{Text: s}) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	inR, inW := io.Pipe()
	in := newInputStreamFrom(inR)
	go runProgrammatic(ctx, h, em, in, nil, "", closeCtx{})

	events := make(chan map[string]any, 256)
	go func() {
		sc := bufio.NewScanner(evR)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				events <- ev
			}
		}
	}()
	waitFor := func(match func(map[string]any) bool, what string) map[string]any {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev := <-events:
				if match(ev) {
					return ev
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	send := func(payload any) {
		t.Helper()
		b, err := json.Marshal(proto.NewCommand("", payload))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(func(ev map[string]any) bool { return ev["type"] == "ready" }, "ready")
	send(&proto.Interrupt{})
	waitFor(func(ev map[string]any) bool {
		return ev["type"] == "control" && ev["id"] == "interrupt" && strings.Contains(ev["message"].(string), "nothing to interrupt")
	}, "the idle interrupt's answer")

	send(&proto.User{Content: "do the thing"})
	waitFor(func(ev map[string]any) bool { return ev["type"] == "token" }, "the partial reply")
	send(&proto.User{Content: "no, do the other thing"})
	waitFor(func(ev map[string]any) bool { return ev["type"] == "queued" }, "the queued message")
	send(&proto.Interrupt{})
	waitFor(func(ev map[string]any) bool { return ev["type"] == "done" && ev["reason"] == "interrupted" }, "done{interrupted}")
	waitFor(func(ev map[string]any) bool { return ev["type"] == "turn" && ev["kind"] == "continue" }, "the next turn")
	waitFor(func(ev map[string]any) bool { return ev["type"] == "done" && ev["reason"] == nil }, "the next turn's done")

	r.mu.Lock()
	var b strings.Builder
	for _, m := range r.last {
		b.WriteString(m.Role + ": " + m.Content + "\n")
	}
	r.mu.Unlock()
	got := b.String()
	ip, im, iq := strings.Index(got, "assistant: Starting with"), strings.Index(got, "[the user interrupted this turn]"),
		strings.Index(got, "no, do the other thing")
	if ip < 0 || im < ip || iq < im {
		t.Fatalf("the next request lacks partial → marker → queued message:\n%s", got)
	}
}
