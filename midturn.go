package dun

import (
	"os"
	"sort"

	"github.com/iodesystems/agentkit/agent"
)

// EndedMidTurn reports whether a session's transcript stops inside a turn:
// its last entry is a user message nothing answered, or a tool call or result
// with no reply after it. A turn that ended — a reply, an interrupt's note,
// a recorded turn error — is not mid-turn. dund uses it on start to tell a
// session that was working when the service died without recording a stop
// (a crash). A missing or empty file is not mid-turn.
func EndedMidTurn(sessionFile string) bool {
	if _, err := os.Stat(sessionFile); err != nil {
		return false
	}
	st, err := openSessionStore(sessionFile)
	if err != nil {
		return false
	}
	// The raw entries, not Context: a recorded turn error is left out of
	// the conversation, and here it is what says the turn ended.
	st.mu.Lock()
	entries := make([]agent.Entry, 0, len(st.entries))
	for _, e := range st.entries {
		if e.Kind != kindRecap {
			entries = append(entries, e)
		}
	}
	st.mu.Unlock()
	if len(entries) == 0 {
		return false
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].CreatedAt < entries[j].CreatedAt })
	switch entries[len(entries)-1].Kind {
	case agent.KindUser, agent.KindToolCall, agent.KindToolResult:
		return true
	}
	return false
}
