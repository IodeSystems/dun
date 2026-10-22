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

// The emitter frames every payload: v, ts, the type, and the session once
// it is known.
func TestEmitterFramesEvents(t *testing.T) {
	var buf bytes.Buffer
	em := &emitter{w: &buf}
	em.emit(&proto.Error{Error: "early", Fatal: true})
	em.setSession("s1")
	em.emit(retryEvent(dun.RetryNote{Scope: "request", Kind: "429", Delay: time.Second, Capacity: 4, Queue: "rejected"}))
	emitTurnError(context.Background(), nil, em, errors.New("boom"))
	evs := decodeStrict[proto.Event](t, buf.Bytes())
	if len(evs) != 3 {
		t.Fatalf("decoded %d of 3 events", len(evs))
	}
	if evs[0].Session != "" || evs[1].Session != "s1" || evs[1].V != proto.Version || evs[1].TS.IsZero() {
		t.Errorf("envelope: %+v / %+v", evs[0], evs[1])
	}
	r := evs[1].Payload.(*proto.Retry)
	if r.DelayMS != 1000 || r.Queue != "rejected" || r.Capacity != 4 {
		t.Errorf("retry: %+v", r)
	}
	if e := evs[2].Payload.(*proto.Error); e.Error != "boom" || e.Fatal {
		t.Errorf("error: %+v", e)
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

// Reasoning goes out as its own event, decodes strictly, and the TUI reads
// the wire shape it was sent in.
func TestReasoningEventReachesTheTUI(t *testing.T) {
	var buf bytes.Buffer
	em := &emitter{w: &buf}
	em.emit(&proto.Reasoning{Text: "weighing it"})
	evs := decodeStrict[proto.Event](t, buf.Bytes())
	if len(evs) != 1 || evs[0].Type != "reasoning" {
		t.Fatalf("got %+v", evs)
	}
	if r := evs[0].Payload.(*proto.Reasoning); r.Text != "weighing it" {
		t.Errorf("reasoning: %+v", r)
	}
	var raw map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &raw); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(&dunProc{}, "/ws")
	m = m.handleEvent(evMsg(raw))
	if i := m.liveThinking(); i < 0 || m.convo[i].think.text != "weighing it" {
		t.Errorf("the TUI did not take the event: %d", i)
	}
}
