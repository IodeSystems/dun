package dun

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// A transcript whose last entry is an unanswered user message or a tool
// round ended mid-turn; one ending in a reply, a turn error or an
// interrupt's note did not.
func TestEndedMidTurn(t *testing.T) {
	for _, c := range []struct {
		name string
		last agent.EntryKind
		want bool
	}{
		{"user", agent.KindUser, true},
		{"tool call", agent.KindToolCall, true},
		{"tool result", agent.KindToolResult, true},
		{"reply", agent.KindAssistant, false},
		{"turn error", kindTurnError, false},
		{"interrupt note", agent.KindNotification, false},
	} {
		f := filepath.Join(t.TempDir(), "s.jsonl")
		st, err := openSessionStore(f)
		if err != nil {
			t.Fatal(err)
		}
		_ = st.Append(context.Background(), "dun", agent.Entry{ID: "1", Kind: agent.KindUser, Content: "hi", CreatedAt: 1})
		_ = st.Append(context.Background(), "dun", agent.Entry{ID: "2", Kind: c.last, Content: "{}", CreatedAt: 2})
		st.flushLocked()
		if got := EndedMidTurn(f); got != c.want {
			t.Errorf("%s: EndedMidTurn = %v", c.name, got)
		}
	}
	if EndedMidTurn(filepath.Join(t.TempDir(), "none.jsonl")) {
		t.Error("a missing transcript is not mid-turn")
	}
}
