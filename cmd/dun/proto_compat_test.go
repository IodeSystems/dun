package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// decodeStrict decodes every NDJSON line as T with proto.Strict on: a field
// the engine or the TUI writes that the proto struct lacks fails here.
func decodeStrict[T any](t *testing.T, ndjson []byte) []T {
	t.Helper()
	proto.Strict = true
	defer func() { proto.Strict = false }()
	var out []T
	sc := bufio.NewScanner(bytes.NewReader(ndjson))
	for sc.Scan() {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Errorf("%s: %v", sc.Text(), err)
			continue
		}
		out = append(out, v)
	}
	return out
}

// The engine's event builders, as they are today, decode into the proto
// structs with no field left over. Moving the engine onto the structs (S1)
// must not change what a client reads.
func TestProtoCoversEngineEvents(t *testing.T) {
	var buf bytes.Buffer
	em := &emitter{w: &buf}
	em.emit(retryEvent(dun.RetryNote{Scope: "request", Kind: "429", Attempt: 2, Status: 429,
		Delay: time.Second, Elapsed: 2 * time.Second, Budget: time.Minute, Reason: "rate", Detail: "slow down",
		Capacity: 4, InFlight: 4, Waiting: 2, Queue: "rejected", ServerAsked: true}))
	em.emit(retryEvent(dun.RetryNote{Scope: "turn", Kind: "recovered"}))
	emitTurnError(context.Background(), nil, em, errors.New("boom"))
	em.emit(event{"type": "agents", "agents": agentsToAny([]dun.AgentInfo{{ID: 1, Model: "m", State: "running",
		Status: "s", Prompt: "p", Tokens: 3, Seconds: 4, Blocked: true}})})
	em.emit(event{"type": "jobs", "jobs": jobsToAny([]dun.JobInfo{{ID: 1, Command: "make", Log: "/l", State: "ok",
		Bytes: 9, Started: 1, Ended: 2, Code: 1, Muted: true}})})
	em.emit(event{"type": "notification", "kind": "docs", "found": 2, "surfaced": 1,
		"docs":  docsToAny([]dun.DocHitInfo{{Title: "t", DocID: "d", Line: "l", Score: 0.5, Inline: true}}),
		"bytes": 10, "tokens": 3, "latency_ms": int64(12)})
	bd := dun.SystemBreakdown{Exact: true, Prompt: 100, Shared: 5, Total: 200,
		Parts: []dun.ContextPart{{Name: "shell", Tokens: 50}}}
	cc := event{"type": "context_cost", "system_tokens": bd.Total, "system_exact": bd.Exact,
		"system_prompt": bd.Prompt, "system_shared": bd.Shared, "system_parts": systemPartsEvent(bd)}
	addWindow(cc, dun.WindowBudget{Window: 1000, Reserved: 100, Cap: 900, PromptBudget: 800, Prompt: 300,
		Overhead: 20, CharsPerToken: 3.5, Measured: true, Rounds: 4, Cuts: 1, Folds: 1})
	em.emit(cc)
	em.emit(event{"type": "server", "id": "docs", "action": "on", "message": "m",
		"servers": serversToAny([]dun.ServerState{{ID: "docs", Running: true, Auto: true, Tools: 3, Err: "e"}}),
		"tools":   []string{"a"}})
	em.emit(event{"type": "suggestions", "items": []any{map[string]any{"text": "t", "prob": 0.5}}})

	evs := decodeStrict[proto.Event](t, buf.Bytes())
	if len(evs) != 9 {
		t.Fatalf("decoded %d of 9 events", len(evs))
	}
	r := evs[0].Payload.(*proto.Retry)
	if r.DelayMS != 1000 || r.Queue != "rejected" || r.Waiting != 2 {
		t.Errorf("retry: %+v", r)
	}
	c := evs[6].Payload.(*proto.ContextCost)
	if c.Window == nil || c.Window.Window != 1000 || c.SystemParts[0].Name != "shell" {
		t.Errorf("context_cost: %+v", c)
	}
}

// The TUI's command encoders decode into the proto commands.
func TestProtoCoversTUICommands(t *testing.T) {
	var buf bytes.Buffer
	p := &dunProc{stdin: nopCloser{&buf}}
	p.send("hi")
	p.sendReset()
	p.serverCmd("rag", "on")
	p.controlCmd("docker", "on")
	p.answer("yes")
	p.agentCmd(3, "tell", "go on")
	cs := decodeStrict[proto.Command](t, buf.Bytes())
	if len(cs) != 6 {
		t.Fatalf("decoded %d of 6 commands", len(cs))
	}
	if a := cs[5].Payload.(*proto.AgentCmd); a.ID != "3" || a.Content != "go on" {
		t.Errorf("agent: %+v", a)
	}
}
