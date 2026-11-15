package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// The phases a turn goes through, as the engine's own events move them; one
// turn_state per change and none for an event that changes nothing.
func TestTurnStatePhases(t *testing.T) {
	var buf bytes.Buffer
	em := &emitter{w: &buf}
	em.turn = newTurnTracker(em, func() *dun.Harness { return nil })
	em.turn.now = func() time.Time { return time.UnixMilli(1000) }
	for _, p := range []any{
		&proto.Turn{Kind: "user"},
		&proto.Token{Text: "a"}, // no change
		&proto.ToolCall{CallID: "c1", Tool: "ask_user"},
		&proto.Ask{Question: "which?", Options: []string{"a", "b"}},
		&proto.Answered{Value: "a"},
		&proto.ToolResult{CallID: "c1", Tool: "ask_user"},
		&proto.Retry{Scope: "turn", Kind: "interrupted", Text: "wait"},
		&proto.Token{Text: "b"},
		&proto.Done{},
	} {
		em.emit(p)
	}
	em.turn.interrupting() // no turn: nothing
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var ev map[string]any
		_ = json.Unmarshal([]byte(line), &ev)
		if ev["type"] != "turn_state" {
			continue
		}
		s := ev["phase"].(string)
		if ev["ask"] != nil {
			s += "+ask"
		}
		if ev["discard"] == true {
			s += "+discard"
		}
		if tool, _ := ev["tool"].(string); tool != "" {
			s += "+" + tool
		}
		got = append(got, s)
	}
	want := []string{"working", "working+ask_user", "waiting_answer+ask+ask_user", "working+ask_user",
		"working", "retrying+discard", "working", "idle"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("phases\n got %v\nwant %v", got, want)
	}
}

// An interrupt holds the phase until the turn's done; a question asked
// meanwhile does not take it back to waiting.
func TestTurnStateInterrupting(t *testing.T) {
	st := proto.TurnState{Phase: proto.PhaseIdle}
	now := time.UnixMilli(5)
	proto.StepTurnState(&st, &proto.Turn{}, now)
	if st.Phase != proto.PhaseWorking || st.Since != 5 {
		t.Fatalf("%+v", st)
	}
	st.Phase = proto.PhaseInterrupting
	proto.StepTurnState(&st, &proto.Ask{Question: "q"}, now)
	proto.StepTurnState(&st, &proto.Retry{Kind: "5xx"}, now)
	if st.Phase != proto.PhaseInterrupting {
		t.Fatalf("interrupt lost: %+v", st)
	}
	proto.StepTurnState(&st, &proto.Done{Reason: "interrupted"}, now)
	if st.Phase != proto.PhaseIdle || st.Since != 0 || st.Ask != nil {
		t.Fatalf("not idle: %+v", st)
	}
	proto.StepTurnState(&st, &proto.Retry{Kind: "giveup"}, now)
	if st.Phase != proto.PhaseIdle {
		t.Fatalf("%+v", st)
	}
}
