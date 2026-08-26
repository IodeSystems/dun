package dun

import (
	"time"

	"github.com/google/uuid"
	"github.com/iodesystems/agentkit/agent"
)

// kindTurnError is a failed turn, left in the transcript for whoever reads the
// log later. Filtered out of Context exactly like kindRecap: it costs the model
// nothing and never renders as conversation.
//
// It must not reach the model. A harness error is not something the model did
// or saw, and feeding one back invites it to explain — or apologise for — a
// failure that happened underneath it. What the model needs after a cut-off
// turn is the conversation, which is already on disk.
//
// It exists because a turn error had no durable record AT ALL: it rendered once
// in the TUI and left with the scrollback, so "why did that turn die?" was
// unanswerable an hour later — the session log knew that a turn had stopped
// mid-tool-call and could not say why.
const kindTurnError agent.EntryKind = "error"

// RecordTurnError persists why a turn failed.
//
// Nil-safe, and deliberately silent on failure: a store that cannot write is
// not worth reporting over the error that got us here.
func (h *Harness) RecordTurnError(err error) {
	if h == nil || err == nil {
		return
	}
	h.store.appendSilent(agent.Entry{
		ID: uuid.New().String(), Kind: kindTurnError,
		Content: err.Error(), CreatedAt: time.Now().UnixNano(),
	})
}
