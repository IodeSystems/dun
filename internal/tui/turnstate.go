package tui

import (
	"slices"
	"time"
)

// turn_state is where the session's turn is (proto.TurnState, sent by the
// engine on every change and kept in a snapshot): busy, the turn's clock,
// an interrupt under way, the open question, a provider wait and the queue
// are read from it. Nothing here is inferred from tokens, tool calls or a
// done (USER 2026-11-14; docs/session-events.md).
func (m tuiModel) applyTurnState(ev evMsg) tuiModel {
	phase := str(ev["phase"])
	m.busy = phase != "" && phase != "idle"
	m.interrupting = phase == "interrupting"
	switch since := evNum(ev["since"]); {
	case !m.busy:
		m.busyStart = time.Time{}
	case since > 0:
		m.busyStart = time.UnixMilli(int64(since))
	case m.busyStart.IsZero():
		m.busyStart = time.Now()
	}
	queued, _ := ev["queued"].([]any)
	m.queuedMsgs = len(queued)
	m.notesWaiting = int(evNum(ev["notes_waiting"]))
	m.netSince, m.netNext = time.Time{}, time.Time{}
	if w, ok := ev["net_wait"].(map[string]any); ok {
		m.netSince, m.netNext = time.UnixMilli(int64(evNum(w["since"]))), time.UnixMilli(int64(evNum(w["next"])))
	}
	m.reconcileQueued(queued)
	// The question: a client that arrived after it was asked (a snapshot
	// past the replay window) opens it from here; one still showing a
	// question the turn no longer waits on closes it.
	switch phase {
	case "waiting_answer":
		if a, ok := ev["ask"].(map[string]any); ok && !m.asking {
			ask := evMsg{"type": "ask"}
			for k, v := range a {
				ask[k] = v
			}
			m = m.applyEvent(ask)
		}
	case "interrupting":
		// The question stays until the turn's done.
	default:
		if m.asking {
			m = m.clearAsk()
		}
	}
	// A provider wait: the banner, also for a client that missed the retry
	// event; a round whose stream died loses its text (discard).
	if r, ok := ev["retry"].(map[string]any); ok && phase == "retrying" {
		if ev["discard"] == true {
			if replyShown(m.cur) {
				// Its text goes (it is being regenerated), its rows do not: a
				// marker holds the height it had.
				m.convo = append(m.convo, convoEntry{collapsed: StDim.Render("↺ the reply was cut off — regenerating"),
					maxH: m.curMaxH, maxKey: m.curMaxKey})
			}
			m.cur, m.curMaxH, m.curMaxKey = "", 0, 0
		}
		if m.retry == "" {
			m.retry = m.retryBanner(evMsg(r))
		}
	} else if m.retry != "" {
		m.clearRetry()
	}
	if !m.busy {
		m.activeTool = ""
	}
	m.refresh()
	return m
}

// reconcileQueued makes the queued rows the state's queue: a message the
// state lists and the screen does not show (a client that arrived late) is
// added; a row the queue no longer holds, with no delivered or withdrawn
// for it (those come before the state that drops it), was lost with its
// engine, and says so.
func (m *tuiModel) reconcileQueued(queued []any) {
	ids := make([]string, 0, len(queued))
	for _, q := range queued {
		o, _ := q.(map[string]any)
		id, text := str(o["id"]), str(o["text"])
		ids = append(ids, id)
		i := slices.IndexFunc(m.convo, func(e convoEntry) bool { return e.msgID == id })
		switch {
		case i < 0:
			m.convo = append(m.convo, queuedEntry(id, text))
		case m.convo[i].lost: // an earlier state raced its queueing: queued after all
			m.convo[i] = queuedEntry(id, text)
		}
	}
	if len(ids) > 0 {
		return
	}
	// The queue is empty and a row is still queued: only when no turn runs
	// is that a message lost (with its engine); mid-turn it may be one
	// echoed a moment before the engine queued it.
	for i := range m.convo {
		e := &m.convo[i]
		if !e.provisional || e.msgID == "" || m.busy {
			continue
		}
		e.provisional, e.lost = false, true
		e.collapsed = StDim.Render("› " + e.provisionalText + " (not delivered: the engine stopped)")
		e.invalidateWrap()
	}
}

// queuedEntry is the row of a message waiting for the running turn.
func queuedEntry(id, text string) convoEntry {
	return convoEntry{
		collapsed:       StDim.Render("› " + text + " (queued)"),
		provisional:     true,
		provisionalText: text,
		userText:        text,
		msgID:           id,
	}
}
