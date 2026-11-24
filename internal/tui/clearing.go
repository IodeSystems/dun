package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A /clear that waits for the running turn to stop (dun's clearing event,
// cmd/dun/clear.go): one row that turns with the spinner and names what the
// turn was doing, until the reset that follows. A message sent meanwhile is
// queued by the engine (its queued row, turn_state's queue) and runs in the
// cleared conversation: the reset keeps the queued rows.
//
// Timing (ready's startup, reset's timing): when an operation took longer
// than slowOp, one dim line says where the time went.

// clearingRow is the index of the clear-in-progress row, or -1.
func (m *tuiModel) clearingRow() int {
	return slices.IndexFunc(m.convo, func(e convoEntry) bool { return e.clearing != "" })
}

// clearingLine is the row's text at the spinner's current frame.
func (m *tuiModel) clearingLine(waiting string) string {
	return strings.TrimRight(m.spin.View(), " ") + " " + StDim.Render("clearing: waiting for "+waiting+" to stop…")
}

// startClearing shows the row (once).
func (m *tuiModel) startClearing(waiting string) {
	if waiting == "" {
		waiting = "the turn"
	}
	m.flushCur()
	if i := m.clearingRow(); i >= 0 {
		return
	}
	m.convo = append(m.convo, convoEntry{collapsed: m.clearingLine(waiting), clearing: waiting})
}

// turnClearing steps the row's spinner; it reports whether there is a row.
func (m *tuiModel) turnClearing() bool {
	i := m.clearingRow()
	if i < 0 {
		return false
	}
	if l := m.clearingLine(m.convo[i].clearing); l != m.convo[i].collapsed {
		m.convo[i].collapsed = l
		m.convo[i].invalidateWrap()
	}
	return true
}

// keptByReset is what a reset leaves of the conversation: the rows of
// messages still queued, which run in the cleared conversation, and the
// messages typed before ready.
func keptByReset(convo []convoEntry) []convoEntry {
	var kept []convoEntry
	for _, e := range convo {
		if (e.provisional && e.msgID != "") || e.startKept {
			kept = append(kept, e)
		}
	}
	return kept
}

// slowOp is how long a start or a clear may take before the TUI says where
// the time went.
const slowOp = 3 * time.Second

// opPhase is one step of a timed operation (proto.Phase).
type opPhase struct {
	name string
	d    time.Duration
}

// timingOf reads a proto.Timing from an event: its total and its phases.
func timingOf(v any) (time.Duration, []opPhase) {
	t, ok := v.(map[string]any)
	if !ok {
		return 0, nil
	}
	var ps []opPhase
	list, _ := t["phases"].([]any)
	for _, p := range list {
		if o, ok := p.(map[string]any); ok {
			ps = append(ps, opPhase{name: str(o["name"]), d: time.Duration(evNum(o["ms"])) * time.Millisecond})
		}
	}
	return time.Duration(evNum(t["total_ms"])) * time.Millisecond, ps
}

// slowLine is "start took 4.2s: built-in servers 1.5s, tools 0.6s": what
// took total, by its longest phases (at most three, each a tenth of the
// total or more). "" when total is under slowOp.
func slowLine(what string, total time.Duration, phases []opPhase) string {
	if total < slowOp {
		return ""
	}
	ps := slices.Clone(phases)
	slices.SortStableFunc(ps, func(a, b opPhase) int { return cmp.Compare(b.d, a.d) })
	var named []string
	for _, p := range ps {
		if len(named) == 3 || p.d < total/10 {
			break
		}
		named = append(named, fmt.Sprintf("%s %s", p.name, secs(p.d)))
	}
	line := what + " took " + secs(total)
	if len(named) > 0 {
		line += ": " + strings.Join(named, ", ")
	}
	return line
}

// secs is d as "4.2s".
func secs(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }

// startLine is the slow-start line for a ready event: the engine's phases,
// and, when this TUI watched the start (since), the time before the engine
// began timing it — dund, and the engine's own launch.
func startLine(ev evMsg, since time.Time) string {
	total, phases := timingOf(ev["startup"])
	if !since.IsZero() {
		if wall := time.Since(since); wall > total {
			phases = append([]opPhase{{name: "launching the engine", d: wall - total}}, phases...)
			total = wall
		}
	}
	return slowLine("start", total, phases)
}

// freshEvent reports whether ev was written a moment ago: a timing is said
// for an operation this TUI watched, not for one a reattach replays.
func freshEvent(ev evMsg) bool {
	ts, err := time.Parse(time.RFC3339Nano, str(ev["ts"]))
	return err == nil && time.Since(ts) < 5*time.Second
}
