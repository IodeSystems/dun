package tui

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"

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

// The TUI's command encoders decode into the proto commands.
func TestProtoCoversTUICommands(t *testing.T) {
	var buf bytes.Buffer
	p := &dunProc{stdin: nopCloser{&buf}}
	p.send("hi [image 1: a.png 2×1]", []proto.Image{{Path: "/tmp/a.png", Label: "[image 1: a.png 2×1]"}})
	p.controlCmd("docker", "on")
	p.answer("yes")
	p.agentCmd(3, "tell", "go on")
	cs := decodeStrict[proto.Command](t, buf.Bytes())
	if len(cs) != 4 {
		t.Fatalf("decoded %d of 4 commands", len(cs))
	}
	if u := cs[0].Payload.(*proto.User); len(u.Images) != 1 || u.Images[0].Path != "/tmp/a.png" {
		t.Errorf("user: %+v", u)
	}
	if a := cs[3].Payload.(*proto.AgentCmd); a.ID != "3" || a.Content != "go on" {
		t.Errorf("agent: %+v", a)
	}
}

// The TUI reads the wire shape the event is sent in.
func TestReasoningEventReachesTheTUI(t *testing.T) {
	b, err := json.Marshal(proto.NewEvent(&proto.Reasoning{Text: "weighing it"}))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(&dunProc{}, "/ws")
	m = m.handleEvent(evMsg(raw))
	if i := m.liveThinking(); i < 0 || m.convo[i].think.text != "weighing it" {
		t.Errorf("the TUI did not take the event: %d", i)
	}
}
