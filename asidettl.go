package dun

import (
	"os"
	"strconv"
	"time"
)

// asidettl.go — an aside's delivery rule (USER 2026-11-07). An aside never
// causes a turn: it rides a tool result or the next turn that runs anyway.
// That is right for news that can wait, and wrong for an agent that ended its
// turn to WAIT on a job that then went silent: the "still running, quiet for
// 5m" heartbeat was an aside, so nothing woke the agent to kill a wedged job.
//
// So each aside can say:
//   - Key: a newer aside with the same key replaces the queued one (the 5m
//     heartbeat replaces the 1m one), keeping the earlier escalation time, so
//     a stream of updates cannot postpone the wake forever.
//   - EscalateAfter: undelivered by then, it becomes a notification and wakes
//     the agent. Delivered first (mid-turn, in a tool result), it never does.
//   - TurnScoped: dropped when the next turn starts (it describes this one).

// AsideOpts is an aside's delivery rule; the zero value is a plain aside.
type AsideOpts struct {
	Key           string
	EscalateAfter time.Duration
	TurnScoped    bool
}

// AsideWith queues text as an aside with the delivery rule o.
func (h *Harness) AsideWith(text string, o AsideOpts) {
	now := time.Now()
	q := queued{kind: queuedAside, text: text, key: o.Key, turnScoped: o.TurnScoped}
	if o.EscalateAfter > 0 {
		q.escalateAt = now.Add(o.EscalateAfter)
	}
	h.noteMu.Lock()
	replaced := false
	if q.key != "" {
		for i := range h.queue {
			old := &h.queue[i]
			if old.key != q.key || (old.kind != queuedAside && old.kind != queuedNotification) {
				continue
			}
			// Keep the earlier escalation; an already-escalated one stays a
			// notification, with the newer text.
			if !old.escalateAt.IsZero() && (q.escalateAt.IsZero() || old.escalateAt.Before(q.escalateAt)) {
				q.escalateAt = old.escalateAt
			}
			q.kind = old.kind
			*old = q
			replaced = true
			break
		}
	}
	if !replaced {
		h.queue = append(h.queue, q)
	}
	h.noteMu.Unlock()
	if !q.escalateAt.IsZero() && q.kind == queuedAside {
		time.AfterFunc(time.Until(q.escalateAt), h.escalateDue)
	}
}

// escalateDue turns every queued aside past its escalation time into a
// notification, and wakes the driver once for them.
func (h *Harness) escalateDue() {
	now := time.Now()
	promoted := false
	h.noteMu.Lock()
	for i := range h.queue {
		q := &h.queue[i]
		if q.kind == queuedAside && !q.escalateAt.IsZero() && !now.Before(q.escalateAt) {
			q.kind = queuedNotification
			promoted = true
		}
	}
	h.noteMu.Unlock()
	if promoted {
		select {
		case h.wake <- struct{}{}:
		default: // a turn is already due
		}
	}
}

// dropTurnScoped drops the asides that describe the turn that just ended.
// Called where a turn starts (prepareTurn).
func (h *Harness) dropTurnScoped() {
	h.drain(func(q queued) bool { return q.turnScoped })
}

// asideEscalate is how long a heartbeat about a silent job or child waits
// for a turn to carry it before it wakes the agent: --aside-escalate-ms
// (Config.AsideEscalate), else DUN_ASIDE_ESCALATE_MS, else 5s.
func (h *Harness) asideEscalate() time.Duration {
	if h.cfg.AsideEscalate > 0 {
		return h.cfg.AsideEscalate
	}
	if v := os.Getenv("DUN_ASIDE_ESCALATE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 5 * time.Second
}
