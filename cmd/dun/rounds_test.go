package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/iodesystems/dun/proto"
)

// The emitter stamps rounds and writes reasoning_end where a round's
// thinking ends, in order, so every path that emits gets it.
func TestEmitter_Rounds(t *testing.T) {
	var buf bytes.Buffer
	em := &emitter{w: &buf}
	em.emit(&proto.Request{Phase: "start"})
	em.emit(&proto.Reasoning{Text: "hm"})
	em.emit(&proto.Token{Text: "ok"})
	em.emit(&proto.Request{Phase: "done"})
	em.emit(&proto.ToolCall{CallID: "c1", Tool: "eval"})
	var got []string
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var ev struct {
			Type  string `json:"type"`
			Round int    `json:"round"`
		}
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type != "request" {
			got = append(got, ev.Type+":"+string(rune('0'+ev.Round)))
		}
	}
	want := "reasoning:1 reasoning_end:1 token:1 tool_call:1"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %v\nwant %s", got, want)
	}
}
