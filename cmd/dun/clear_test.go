package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/proto"
)

// slowStopRunner streams a partial reply on the first call and, cancelled,
// takes stopIn to stop (a provider slow to close its stream); every later
// call answers "ok" and records the messages it was sent.
type slowStopRunner struct {
	stopIn time.Duration
	mu     sync.Mutex
	n      int
	last   []llm.Message
}

func (r *slowStopRunner) ChatStream(ctx context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
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
		out <- llm.StreamChunk{Content: "Starting with the old task"}
		<-ctx.Done()
		time.Sleep(r.stopIn)
		select {
		case out <- llm.StreamChunk{Error: ctx.Err().Error()}:
		case <-time.After(time.Second):
		}
	}()
	return out, nil
}

// engineRig runs runProgrammatic over pipes: send a command, wait for an
// event.
type engineRig struct {
	t      *testing.T
	h      *harness.Harness
	inW    io.Writer
	events chan map[string]any
	seen   []map[string]any
}

func newEngineRig(t *testing.T, r agent.LLMRunner, opts ...func(*harness.Config)) *engineRig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DUN_HOME", filepath.Join(home, ".dun"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	evR, evW := io.Pipe()
	em := &emitter{w: evW}
	em.turn = newTurnTracker(em, func() *harness.Harness { return nil })
	cfg := harness.Config{
		Workspace: home, Servers: []mcpserver.Server{}, Client: r,
		SessionFile: filepath.Join(home, "s.jsonl"),
		OnToken:     func(s string) { em.emit(&proto.Token{Text: s}) },
		OnDelivered: func(d harness.Delivery) { em.emit(&proto.Delivered{IDs: d.IDs, CallID: d.CallID}) },
	}
	for _, o := range opts {
		o(&cfg)
	}
	h, err := harness.Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	em.turn.h = func() *harness.Harness { return h }
	inR, inW := io.Pipe()
	go runProgrammatic(ctx, h, em, newInputStreamFrom(inR), nil, "", closeCtx{})
	rig := &engineRig{t: t, h: h, inW: inW, events: make(chan map[string]any, 512)}
	go func() {
		sc := bufio.NewScanner(evR)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				rig.events <- ev
			}
		}
	}()
	return rig
}

func (g *engineRig) send(payload any) {
	g.t.Helper()
	b, err := json.Marshal(proto.NewCommand("", payload))
	if err != nil {
		g.t.Fatal(err)
	}
	if _, err := g.inW.Write(append(b, '\n')); err != nil {
		g.t.Fatal(err)
	}
}

func (g *engineRig) waitFor(what string, match func(map[string]any) bool) map[string]any {
	g.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-g.events:
			g.seen = append(g.seen, ev)
			if match(ev) {
				return ev
			}
		case <-deadline:
			var types []string
			for _, ev := range g.seen {
				types = append(types, fmt.Sprint(ev["type"]))
			}
			g.t.Fatalf("timed out waiting for %s; saw %s", what, strings.Join(types, " "))
		}
	}
}

func isType(typ string) func(map[string]any) bool {
	return func(ev map[string]any) bool { return ev["type"] == typ }
}

// /clear with a turn running: the turn is interrupted, the engine says what
// the clear waits for (clearing), a message sent meanwhile is queued, the
// reset comes when the turn is over and says how long it took, and the
// queued message runs in the cleared conversation — none of the old turn
// in its request.
//
// The old task is an unacknowledged message, which /clear drops without
// asking, so a bare /clear interrupts the turn at once.
func TestEngineClearWaitsForTheTurn(t *testing.T) {
	r := &slowStopRunner{stopIn: 300 * time.Millisecond}
	g := newEngineRig(t, r)
	g.waitFor("ready", isType("ready"))

	g.send(&proto.User{Content: "the old task"})
	g.waitFor("the partial reply", isType("token"))
	g.send(&proto.ControlCmd{ID: "slash", Action: "clear"})
	cl := g.waitFor("clearing", isType("clearing"))
	if cl["waiting"] != "the model's reply" {
		t.Errorf("clearing waits for %v, want the model's reply", cl["waiting"])
	}
	g.send(&proto.User{Content: "the new task"})
	q := g.waitFor("the queued message", func(ev map[string]any) bool { return ev["type"] == "user" && ev["queued"] == true })
	id := q["id"]
	reset := g.waitFor("reset", isType("reset"))
	tm, _ := reset["timing"].(map[string]any)
	if tm == nil || tm["total_ms"].(float64) < 250 {
		t.Errorf("reset timing = %v, want the ~300ms the turn took to stop", reset["timing"])
	}
	if ps, _ := tm["phases"].([]any); len(ps) == 0 || ps[0].(map[string]any)["name"] != "stopping the model's reply" {
		t.Errorf("reset phases = %v, want stopping the model's reply first", tm["phases"])
	}
	d := g.waitFor("the queued message delivered", isType("delivered"))
	if ids, _ := d["ids"].([]any); len(ids) != 1 || ids[0] != id {
		t.Errorf("delivered %v, want [%v]", d["ids"], id)
	}
	g.waitFor("the new turn's done", func(ev map[string]any) bool { return ev["type"] == "done" && ev["reason"] == nil })

	r.mu.Lock()
	var b strings.Builder
	for _, m := range r.last {
		if m.Role != "system" {
			b.WriteString(m.Role + ": " + m.Content + "\n")
		}
	}
	r.mu.Unlock()
	got := b.String()
	// The asks board lists the person's messages across a clear (asks.go,
	// as before this change); the conversation itself has none of the old
	// turn: not its message, its partial reply or its interrupt marker.
	if !strings.Contains(got, "user: the new task\n") || strings.Contains(got, "user: the old task\n") ||
		strings.Contains(got, "Starting with") || strings.Contains(got, "was cleared") {
		t.Fatalf("the cleared conversation's request:\n%s", got)
	}
}

// /clear with no turn running is done at once, as before: no clearing.
func TestEngineClearIdle(t *testing.T) {
	g := newEngineRig(t, &slowStopRunner{})
	g.waitFor("ready", isType("ready"))
	g.send(&proto.ControlCmd{ID: "slash", Action: "clear"})
	g.waitFor("reset", isType("reset"))
	for _, ev := range g.seen {
		if ev["type"] == "clearing" {
			t.Fatal("an idle /clear sent clearing")
		}
	}
}
