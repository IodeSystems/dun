package proto

import "time"

// StepTurnState is the turn state after one engine event: the engine's
// rule (cmd/dun turnstate.go), here so a test can play the engine. The queue
// is the harness's and is not touched.
func StepTurnState(st *TurnState, payload any, now time.Time) {
	idle := func() { *st = TurnState{Phase: PhaseIdle, Queued: st.Queued} }
	working := func() {
		if st.Phase == PhaseIdle {
			st.Since = now.UnixMilli()
		}
		if st.Phase != PhaseInterrupting {
			st.Phase = PhaseWorking
		}
		st.Retry, st.Discard = nil, false
	}
	asking := func() {
		working()
		if st.Phase != PhaseInterrupting {
			st.Phase = PhaseWaitingAnswer
		}
	}
	switch p := payload.(type) {
	case *Turn:
		working()
	case *Done, *Error:
		idle()
	case *Ask:
		asking()
		a := *p
		st.Ask, st.Mount = &a, nil
	case *Mount:
		asking()
		mt := *p
		st.Mount, st.Ask = &mt, nil
	case *Answered:
		if st.Phase == PhaseWaitingAnswer {
			st.Phase = PhaseWorking
		}
		st.Ask, st.Mount = nil, nil
	case *Retry:
		switch p.Kind {
		case "giveup":
			idle() // the turn's error follows
		case "recovered":
			if st.Phase == PhaseRetrying {
				working()
			}
		default:
			if st.Phase == PhaseIdle || st.Phase == PhaseInterrupting {
				return
			}
			r := *p
			st.Phase, st.Retry = PhaseRetrying, &r
			// A turn-scope retry redoes a round whose stream died: the text
			// it streamed is thrown away and streamed again.
			st.Discard = p.Scope == "turn"
		}
	case *ToolCall:
		if st.Phase == PhaseRetrying || st.Phase == PhaseIdle {
			working()
		}
		st.Tool, st.CallID = p.Tool, p.CallID
	case *ToolResult:
		st.Tool, st.CallID = "", ""
	case *Token, *Reasoning:
		if st.Phase == PhaseRetrying {
			working()
		}
	}
}
