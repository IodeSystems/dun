package harness

import (
	"context"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun/internal/memory"
)

// Memory triggers — the memory fork run without being asked for, when the
// words of a turn say something should be kept and the turn did not keep it
// (memory.ScanTrigger holds the patterns and how they were measured).

// triggerMax bounds how much of a user message goes into the fork's
// instruction: the ask is usually a sentence; a pasted document that
// happens to say "remember" is not.
const triggerMax = 1200

// triggerTimeout bounds a triggered fork, which no caller waits on.
const triggerTimeout = 2 * time.Minute

// checkMemoryTriggers looks at the entries since the last check and runs
// the memory fork when they call for it. It returns the fork's line for the
// user, or "" when nothing fired. Called after a turn, outside turnMu.
func (h *Harness) checkMemoryTriggers(ctx context.Context) string {
	// A child's "user" is its parent: the parent's words are not the
	// person's, and the parent runs its own triggers.
	if h.cfg.DisableMemoryTriggers || h.isChild() || h.memoryStore() == nil || h.store == nil {
		h.markTriggerScan()
		return ""
	}
	t := memory.ScanTrigger(h.entriesSinceTriggerScan())
	if t.Called || (len(t.Asks) == 0 && !t.Claimed) {
		return ""
	}
	what := strings.Join(t.Asks, "\n\n")
	if len(what) > triggerMax {
		what = what[:triggerMax] + "…"
	}
	why := "you said remember"
	if len(t.Asks) == 0 {
		why = "the reply said it saved something, with no memory call"
	}
	msg, err := h.Remember(ctx, what)
	if err != nil {
		return "memory (" + why + "): nothing saved — " + err.Error()
	}
	return "memory (" + why + "): " + msg
}

// entriesSinceTriggerScan returns the entries newer than the last scan and
// moves the mark past them, so each message is looked at once.
func (h *Harness) entriesSinceTriggerScan() []agent.Entry {
	all, _ := h.store.Context(context.Background(), "")
	h.trigMu.Lock()
	defer h.trigMu.Unlock()
	var out []agent.Entry
	mark := h.trigMark
	for _, e := range all {
		if e.CreatedAt > h.trigMark {
			out = append(out, e)
		}
		if e.CreatedAt > mark {
			mark = e.CreatedAt
		}
	}
	h.trigMark = mark
	return out
}

// markTriggerScan moves the mark to now's newest entry without scanning: a
// resumed session's history was someone else's to act on.
func (h *Harness) markTriggerScan() {
	if h.store != nil {
		h.entriesSinceTriggerScan()
	}
}
