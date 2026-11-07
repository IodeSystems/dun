package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun"
)

// A turn that fails after spending rounds reports them; one that spent none
// emits nothing. Without this a max-turns failure left its rounds in no usage
// event and the harbor adapter counted the trial at $0.
func TestFailedTurnReportsUsage(t *testing.T) {
	h := dun.NewTestHarness(dun.Endpoint{URL: "http://127.0.0.1:1/v1", Model: "m"})
	var buf bytes.Buffer
	em := &emitter{w: &buf}
	emitFailedUsage(h, em, agent.TurnResult{Usage: agent.TokenUsage{Turns: 400, Processed: 410, Cached: 73776, Generated: 63}})
	if s := buf.String(); !strings.Contains(s, `"type":"usage"`) || !strings.Contains(s, `"turns":400`) || !strings.Contains(s, `"cached":73776`) {
		t.Fatalf("failed turn's usage not reported: %s", s)
	}
	buf.Reset()
	emitFailedUsage(h, em, agent.TurnResult{})
	if buf.Len() != 0 {
		t.Fatalf("a turn with no rounds emitted usage: %s", buf.String())
	}
}
