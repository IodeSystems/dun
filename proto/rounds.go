package proto

// Rounds: which model round a streamed event belongs to, and where its
// thinking ends (docs/session-events.md, "Thinking rounds"). Clients used to
// infer both: a round's thinking ended at whichever came first of a token, a
// tool call, a question, a retry, the turn's end; each client kept its own
// list of those. The engine keeps the one list here, follows its own events
// as it writes them, and says it.

// ReasoningEnd: round Round's thinking is over. Sent once per round that
// thought, before the first event of that round that is not reasoning (or
// before the next round's request, the turn's end, a question, a retry).
// Cut: the turn was interrupted mid-thought.
type ReasoningEnd struct {
	Round int  `json:"round"`
	Cut   bool `json:"cut,omitempty"`
}

func init() { event[ReasoningEnd]("reasoning_end") }

// RoundState is the engine's place in the turn's rounds: the round now (1
// is the session's first; 0 before any) and whether it is thinking.
type RoundState struct {
	Round    int
	Thinking bool
}

// StepRound stamps payload with the round it belongs to (reasoning, token,
// tool_call) and returns the reasoning_end to send before it, if it ends the
// round's thinking. A round starts at its request's "start". Called with
// the events in the order they are written.
func StepRound(st *RoundState, payload any) *ReasoningEnd {
	end := func(cut bool) *ReasoningEnd {
		if !st.Thinking {
			return nil
		}
		st.Thinking = false
		return &ReasoningEnd{Round: st.Round, Cut: cut}
	}
	switch p := payload.(type) {
	case *Request:
		switch p.Phase {
		case "start":
			e := end(false)
			st.Round++
			return e
		case "done":
			return end(false)
		}
	case *Reasoning:
		p.Round = st.Round
		st.Thinking = true
	case *Token:
		p.Round = st.Round
		return end(false)
	case *ToolCall:
		p.Round = st.Round
		return end(false)
	case *Done:
		return end(p.Reason == "interrupted")
	case *Message, *Ask, *Mount, *Error, *Retry:
		return end(false)
	}
	return nil
}
