package harness

import (
	"strings"
	"sync"
)

// midreply.go — the person's words mid-turn are answered first (USER
// 2026-11-18). A person typed "What is your goal here? what are you doing?"
// during a turn; the model (Gemini via OpenRouter) read it only as text
// inside a tool result, under the plan board, and kept calling tools.
//
//   - Framing: a lifted person message is the last thing in the result,
//     under one header that says the person wrote it now and asks for a
//     reply in text (midTurnFrame).
//   - The system prompt says the same once (systemMidTurn), where it is
//     cached.
//   - Missed reply: if the round after the lift writes no text and only
//     calls tools, one keyed aside (midTurnNudge) rides that round's
//     results. Once per lifted message.
//   - Nothing pushes back: the forced plan call waits while a reply is due
//     (planForceOpts), and a text-only reply right after the lift ends the
//     turn with no turn-end check (onIdle).
//
// All of it costs nothing when nobody typed: the state is set only by a lift.

// midTurnHead heads the person's lifted messages. Model-facing.
const midTurnHead = "[the person wrote mid-turn; reply to them in text before your next tool call, then go on if it still makes sense]:"

// midTurnNudge is the aside when the round after a lift wrote no text.
const midTurnNudge = "The person asked you something mid-turn; reply to them now."

// midTurnKey keys the aside, so a second one replaces the first.
const midTurnKey = "mid-turn-reply"

// midTurnFrame frames the person's messages, verbatim and in order: one
// header, then each message in «».
func midTurnFrame(texts []string) string {
	var b strings.Builder
	b.WriteString(midTurnHead)
	sep := " "
	if len(texts) > 1 {
		sep = "\n"
	}
	for _, t := range texts {
		b.WriteString(sep)
		b.WriteString("«" + t + "»")
	}
	return b.String()
}

// deliverMidTurn puts the framed person's words in front of the model. Today
// it appends them to the tool result, last; it is the one place a real
// role-user message after the round's results would replace it.
func (h *Harness) deliverMidTurn(result, text string) string {
	if result == "" {
		return text
	}
	return result + "\n\n" + text
}

// midReply is the reply a lifted person message is owed; prepareTurn resets it.
type midReply struct {
	mu sync.Mutex
	// waiting: a person's message was lifted; the next round is the reply's.
	waiting bool
	// nudged: the aside was given for the message lifted last.
	nudged bool
	// answered: the round just done was a text-only reply right after a
	// lift, for the turn-end check to accept.
	answered bool
}

func (m *midReply) reset() {
	m.mu.Lock()
	m.waiting, m.nudged, m.answered = false, false, false
	m.mu.Unlock()
}

// midLifted notes a person's message lifted into a tool result.
func (h *Harness) midLifted() {
	h.mid.mu.Lock()
	h.mid.waiting, h.mid.nudged = true, false
	h.mid.mu.Unlock()
}

// midWaiting reports whether the request about to be sent is the reply's.
func (h *Harness) midWaiting() bool {
	h.mid.mu.Lock()
	defer h.mid.mu.Unlock()
	return h.mid.waiting
}

// midRound is told of each round's response (OnToolCalls, which agentkit
// calls for every round, with no calls too): calls is how many tool calls
// it made. The round's text is what the tee captured (roundCapture).
func (h *Harness) midRound(calls int) {
	h.mid.mu.Lock()
	h.mid.answered = false
	if !h.mid.waiting {
		h.mid.mu.Unlock()
		return
	}
	content, _, _, _ := h.round.snapshot()
	text := strings.TrimSpace(content) != ""
	nudge := false
	switch {
	case calls == 0:
		h.mid.waiting, h.mid.answered = false, text
	case text: // it replied and went on
		h.mid.waiting = false
	case !h.mid.nudged:
		h.mid.nudged, nudge = true, true // still waiting: the next round is the reply's
	default:
		h.mid.waiting = false
	}
	h.mid.mu.Unlock()
	if nudge {
		h.noteMu.Lock()
		h.queue = append(h.queue, queued{kind: queuedServiceQuiet, text: midTurnNudge, key: midTurnKey, turnScoped: true})
		h.noteMu.Unlock()
	}
}

// midAnswered reports, once, whether the round just done was a text-only
// reply to a lifted person message: the turn ends on it.
func (h *Harness) midAnswered() bool {
	h.mid.mu.Lock()
	defer h.mid.mu.Unlock()
	a := h.mid.answered
	h.mid.answered = false
	return a
}
