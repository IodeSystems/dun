package proto

import (
	"reflect"
	"testing"
)

// A turn's events in order, with what StepRound stamps and sends before each.
func TestStepRound(t *testing.T) {
	var st RoundState
	type step struct {
		payload any
		end     *ReasoningEnd
	}
	r1, t1, c1 := &Reasoning{Text: "a"}, &Token{Text: "ok"}, &ToolCall{CallID: "c1", Tool: "read"}
	r2, r3 := &Reasoning{Text: "b"}, &Reasoning{Text: "c"}
	steps := []step{
		{&Request{Phase: "start"}, nil},
		{r1, nil},
		{&Reasoning{Text: "a2"}, nil},
		{t1, &ReasoningEnd{Round: 1}}, // the reply began
		{&Token{Text: "more"}, nil},
		{&Request{Phase: "done"}, nil},
		{c1, nil}, // dispatched after the round's request: still round 1
		{&ToolResult{CallID: "c1"}, nil},
		{&Request{Phase: "start"}, nil},
		{r2, nil},
		{&Request{Phase: "done"}, &ReasoningEnd{Round: 2}}, // thought only, then the stream ended
		{&Request{Phase: "start"}, nil},
		{r3, nil},
		{&Done{Reason: "interrupted"}, &ReasoningEnd{Round: 3, Cut: true}},
		{&Done{}, nil}, // nothing open: no second end
	}
	for i, s := range steps {
		if got := StepRound(&st, s.payload); !reflect.DeepEqual(got, s.end) {
			t.Errorf("step %d (%T): end %+v, want %+v", i, s.payload, got, s.end)
		}
	}
	if r1.Round != 1 || t1.Round != 1 || c1.Round != 1 || r2.Round != 2 || r3.Round != 3 {
		t.Errorf("rounds: %d %d %d %d %d", r1.Round, t1.Round, c1.Round, r2.Round, r3.Round)
	}
}

// Whatever cuts a round short ends its thinking: a question, an error, a
// retry, the next round's start.
func TestStepRound_Ends(t *testing.T) {
	for _, p := range []any{&Ask{}, &Mount{}, &Error{}, &Retry{}, &Message{}, &Request{Phase: "start"}} {
		st := RoundState{Round: 4, Thinking: true}
		if got := StepRound(&st, p); got == nil || got.Round != 4 || got.Cut {
			t.Errorf("%T: %+v", p, got)
		}
	}
	// A live request update does not.
	st := RoundState{Round: 4, Thinking: true}
	if got := StepRound(&st, &Request{Phase: "live"}); got != nil {
		t.Errorf("live: %+v", got)
	}
}
