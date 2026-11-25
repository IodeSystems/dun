package main

import (
	"context"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// untilEvent is the session's stop conditions and their run (/until;
// harness/until.go).
func untilEvent(h *harness.Harness) *proto.Until {
	s := h.Until()
	ev := &proto.Until{Cap: s.Cap, Turn: s.Turn, Running: s.Running, Paused: s.Paused, Stopped: s.Stopped, Line: s.Line}
	for i, c := range s.Conds {
		pc := proto.UntilCond{Kind: c.Kind, N: c.N, Goal: c.Goal, Check: c.Check}
		if i < len(s.Progress) {
			pc.Progress = s.Progress[i]
		}
		ev.Conds = append(ev.Conds, pc)
	}
	return ev
}

// untilStep is the engine loop's look at the stop conditions after a turn
// (harness.UntilNext). ran: it ran the run's next turn, and ok is how that
// turn went (continueTurn). A run that stopped is said as a notice. While
// dund holds wakes (an upgrade) the next turn waits: the run is owed it,
// after the restart too.
func untilStep(ctx context.Context, h *harness.Harness, em *emitter) (ran, ok bool) {
	st := h.UntilNext(ctx)
	if st.Stopped != "" {
		em.emit(&proto.Notification{Kind: "until", Text: "until: stopped · " + st.Stopped})
		return false, true
	}
	if !st.Continue || !mayWake() {
		return false, true
	}
	h.UntilContinue(ctx)
	return true, continueTurn(ctx, h, em)
}
