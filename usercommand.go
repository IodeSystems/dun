package dun

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iodesystems/agentkit/agent"
)

// kindCommand is a command the person ran (/file add …, /rag on …) and what
// dun answered, kept in the session (USER 2026-10-28: "user commands stored
// in the session, filtered out for llm, but present for visibility" — so a
// report can show "/file add DIR, then reads could not reach it"). Filtered
// out of the model's context like kindRecap; replayed into the scrollback on
// resume (HistoryItem kind "command").
const kindCommand agent.EntryKind = "command"

// RecordCommand keeps line (as typed: "/file add ~/notes") and reply in the
// session. Nil-safe and silent on failure, like RecordTurnError.
func (h *Harness) RecordCommand(line, reply string) {
	line = strings.TrimSpace(line)
	if h == nil || h.store == nil || line == "" {
		return
	}
	content := line
	if r := strings.TrimSpace(reply); r != "" {
		content += "\n" + r
	}
	h.store.appendSilent(agent.Entry{
		ID: uuid.New().String(), Kind: kindCommand,
		Content: content, CreatedAt: time.Now().UnixNano(),
	})
}
