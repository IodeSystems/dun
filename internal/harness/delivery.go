package harness

import (
	"github.com/google/uuid"
	"github.com/iodesystems/agentkit/agent"
)

// A person's message has an id from the moment the engine takes it: the
// user event carries it, and every later event about the message names it
// (delivered, withdrawn, turn_state's queued list). Clients match rows by
// id, never by text (USER 2026-11-14: "work the TUI does by inference moves
// into dund/the engine as explicit events").

// NewMessageID is a fresh id for a person's message.
func NewMessageID() string { return uuid.New().String() }

// kindDelivered is a message the person typed while the agent worked, that
// a tool result carried to the model, in logs written before such a
// message became a user entry of its own (midreply.go). The model read it
// inside that result, so it is not conversation for the model (filtered out
// of Context like kindCommand); it is conversation for the person, so a
// resumed client shows it where it landed — after the result that carried
// it (HistoryItem kind "user", queued true). Its ID is the message's id.
// Nothing writes it now.
const kindDelivered agent.EntryKind = "delivered"

// QueuedMsg is a person's message buffered for the running turn.
type QueuedMsg struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Delivery is what OnDelivered reports: the person's messages, by id, that
// reached the model. CallID is the tool call whose result carried them, ""
// when a turn read them as its input (flushQueued).
type Delivery struct {
	IDs    []string
	CallID string
}

// stashLifted keeps the person's messages a tool result is about to carry,
// until that result is in the store (deliverLifted): recorded before it,
// they would sort before it in the history and in a resumed scrollback.
// Called with noteMu held.
func (h *Harness) stashLiftedLocked(items []queued) {
	for _, q := range items {
		if q.kind == queuedUser {
			h.lifted = append(h.lifted, q)
		}
	}
}

// deliverLifted sends the messages typed while the tool of result e ran,
// after it (deliverMidTurn), and says so (OnDelivered). Dispatch is one call
// at a time, so the result stored next is the one they waited for.
func (h *Harness) deliverLifted(e agent.Entry) {
	h.noteMu.Lock()
	items := h.lifted
	h.lifted = nil
	h.noteMu.Unlock()
	if len(items) == 0 {
		return
	}
	ids := make([]string, 0, len(items))
	for _, q := range items {
		h.deliverMidTurn(q)
		ids = append(ids, q.id)
	}
	h.delivered(Delivery{IDs: ids, CallID: e.ToolCallID})
}

func (h *Harness) delivered(d Delivery) {
	if len(d.IDs) > 0 && h.cfg.OnDelivered != nil {
		h.cfg.OnDelivered(d)
	}
}
