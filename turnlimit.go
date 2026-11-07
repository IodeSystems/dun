package dun

import (
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
)

// turnlimit.go — the cap on tool-call rounds in one turn, said up front and
// warned of before it cuts (USER 2026-11-07). Without the warning the cut
// came mid-loop with nothing written: TB2.1 polyglot-rust-c spent its 150
// rounds adding one "*/" per edit and ended with no file that compiled.

// defaultTurnLimit suits a coding agent working through a large task.
const defaultTurnLimit = 400

// turnLimitWarnAt is the share of the limit at which the model is warned.
const turnLimitWarnAt = 0.8

// turnLimit is the cap: --tool-call-turn-limit (Config.TurnLimit), else
// DUN_MAX_TURNS, else 400.
func (h *Harness) turnLimit() int {
	if h != nil && h.cfg.TurnLimit > 0 {
		return h.cfg.TurnLimit
	}
	if v := os.Getenv("DUN_MAX_TURNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultTurnLimit
}

// turnLimitWarnRound is the round at which the warning is given.
func turnLimitWarnRound(limit int) int {
	n := int(float64(limit) * turnLimitWarnAt)
	if n < 1 {
		n = 1
	}
	return n
}

// systemTurnLimit is the system prompt's line for it.
func systemTurnLimit(limit int) string {
	return fmt.Sprintf("\n\nA turn may make at most %d rounds of tool calls; at %d you are warned, and at %d the turn is cut off.",
		limit, turnLimitWarnRound(limit), limit)
}

// turnRounds counts this turn's rounds; prepareTurn resets it.
type turnRounds struct {
	n      atomic.Int64
	warned atomic.Bool
}

func (r *turnRounds) reset() { r.n.Store(0); r.warned.Store(false) }

// noteTurnRound counts one round (noteUsage) and warns once at the 80% mark.
func (h *Harness) noteTurnRound() {
	limit := h.turnLimit()
	n := int(h.turnRounds.n.Add(1))
	if n < turnLimitWarnRound(limit) || !h.turnRounds.warned.CompareAndSwap(false, true) {
		return
	}
	h.Aside(fmt.Sprintf("[turn limit] %d of this turn's %d tool-call rounds are used; %d are left before the turn is cut off. "+
		"Write your result now (the file, the answer, the fix), check it once, and end the turn. Do not start a new approach.",
		n, limit, limit-n))
}
