package harness

import (
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
)

// midreply.go — the person's words mid-turn are answered first (USER
// 2026-11-18). A person typed "What is your goal here? what are you doing?"
// during a turn; the model (Gemini via OpenRouter) read it only as text
// inside a tool result, under the plan board, and kept calling tools.
//
//   - Delivery: a message typed while a tool ran is a user message of its
//     own, verbatim, after that round's tool results (deliverMidTurn).
//     agentkit (v0.36.0) claims it at the next round and renders it after
//     the round's last result; a template that refuses a user message after
//     tool results folds it into the result instead
//     (ProviderConfig.FoldUserIntoToolResult).
//   - Missed reply: if the round after the delivery writes no text and only
//     calls tools, one aside (midTurnNudge) rides that round's results.
//     Once per delivered message.
//   - Nothing pushes back: the forced plan call waits while a reply is due
//     (planForceOpts), and a text-only reply right after the delivery ends
//     the turn with no turn-end check (onIdle).
//
// All of it costs nothing when nobody typed: the state is set only by a
// delivery.

// midTurnTag marks a person's message delivered mid-turn, so a resumed
// client shows it where it landed, as typed while the agent worked.
const midTurnTag = "mid-turn"

// turnInput reports whether e is a user message that starts a turn: a user
// entry that is not a message delivered mid-turn. Code that finds the turn's
// start by the newest user entry reads it through this, so a mid-turn
// message stays inside the turn it was typed in.
func turnInput(e agent.Entry) bool {
	return e.Kind == agent.KindUser && e.Tag != midTurnTag
}

// midTurnUser reports whether e is a person's message delivered mid-turn.
func midTurnUser(e agent.Entry) bool {
	return e.Kind == agent.KindUser && e.Tag == midTurnTag
}

// midTurnNudge is the aside when the round after a delivery wrote no text.
const midTurnNudge = "The person asked you something mid-turn; reply to them now."

// midTurnKey keys the aside.
const midTurnKey = "mid-turn-reply"

// deliverMidTurn sends q, a message typed while a tool ran, to the model:
// a pending user entry, as a queued message is saved (flushQueued). The
// turn's next round claims it, and the request carries it as a user message
// after the round's tool results; the turn goes on. Called once the result
// is stored (deliverLifted), so it sorts after it in the history too.
func (h *Harness) deliverMidTurn(q queued) {
	h.store.publish(agent.Entry{
		ID: q.id, Kind: agent.KindUser, Tag: midTurnTag, Content: q.text, CreatedAt: time.Now().UnixNano(),
	})
	if PersonText(q.text) {
		h.mid.mu.Lock()
		h.mid.waiting, h.mid.nudged = true, false
		h.mid.mu.Unlock()
	}
}

// midReply is the reply a delivered person message is owed; prepareTurn
// resets it.
type midReply struct {
	mu sync.Mutex
	// waiting: a person's message was delivered; the next round is the
	// reply's.
	waiting bool
	// nudged: the aside was given for the message delivered last.
	nudged bool
	// answered: the round just done was a text-only reply right after a
	// delivery, for the turn-end check to accept.
	answered bool
}

func (m *midReply) reset() {
	m.mu.Lock()
	m.waiting, m.nudged, m.answered = false, false, false
	m.mu.Unlock()
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
// reply to a delivered person message: the turn ends on it.
func (h *Harness) midAnswered() bool {
	h.mid.mu.Lock()
	defer h.mid.mu.Unlock()
	a := h.mid.answered
	h.mid.answered = false
	return a
}
