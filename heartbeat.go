package dun

import (
	"sync"
	"time"
)

// "Still there" — the reminder that separates working from wedged.
//
// Two things in dun run outside a turn and are unbounded by design: a
// background job (exempt from the foreground deadline, because being long IS
// the point) and a sub-agent (resident until dismissed, with no concurrency
// cap). Both were SILENT while they ran, and silence is the one signal that
// cannot distinguish progress from a hang. The foreground deadline solves this
// by killing; neither of these may be killed, so they have to speak instead.
//
// Two properties make it a heartbeat rather than noise:
//
//   - It is DEBOUNCED by anything the thing itself said. A job reporting
//     progress, a child setting a status, an answer arriving — each is already
//     evidence of life, so each resets the clock. A chatty job never triggers a
//     reminder at all, which is the correct amount of reminding for something
//     you are already hearing from.
//   - It BACKS OFF while quiet: 1m, 5m, 10m, 30m, 1h, then hourly. The value of
//     the reminder is highest early, because most wedges are immediate, and its
//     cost is highest late, where an hourly line is a rounding error against
//     something that has been running all afternoon.
//
// It carries no output. One line saying what it is and how long it has been
// quiet; reading what it actually produced stays an explicit ask (exec_monitor,
// agent_monitor).

// hbSchedule is how long silence must last before each successive reminder.
var hbSchedule = []time.Duration{
	time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour,
}

// hbEvery is the interval once the schedule above is exhausted.
const hbEvery = time.Hour

// heartbeat tracks when something last spoke and fires while it does not.
//
// The schedule is per-instance rather than read from the package vars at fire
// time: a test shortens it by constructing one, never by writing to a global
// that a running goroutine is reading. The first version did the latter and
// -race caught it.
type heartbeat struct {
	sched []time.Duration
	every time.Duration

	mu   sync.Mutex
	last time.Time
	// gen is bumped only by spoke(), so the loop can tell "it said something
	// while I was waiting" (reset the backoff) from "I fired" (advance it).
	gen int
}

func newHeartbeat() *heartbeat {
	return &heartbeat{sched: hbSchedule, every: hbEvery, last: time.Now()}
}

// spoke records that the thing produced something on its own, which resets both
// the timer and the backoff — it has just proved it is alive, so the next
// silence deserves the same early attention as the first.
func (b *heartbeat) spoke() {
	b.mu.Lock()
	b.last, b.gen = time.Now(), b.gen+1
	b.mu.Unlock()
}

// interval is how much silence the nth reminder waits for.
func (b *heartbeat) interval(step int) time.Duration {
	if len(b.sched) == 0 {
		return b.every
	}
	if step < len(b.sched) {
		return b.sched[step]
	}
	return b.sched[len(b.sched)-1] + time.Duration(step-len(b.sched)+1)*b.every
}

// run fires until done reports true. fire is given how long the thing has been
// quiet, which is the only number the reminder is actually about.
func (b *heartbeat) run(done func() bool, fire func(quiet time.Duration)) {
	for step := 0; ; {
		b.mu.Lock()
		last, gen := b.last, b.gen
		b.mu.Unlock()

		iv := b.interval(step)
		if wait := time.Until(last.Add(iv)); wait > 0 {
			time.Sleep(wait)
		}
		if done() {
			return
		}

		b.mu.Lock()
		spoke := b.gen != gen
		if !spoke {
			// Measure the NEXT interval from this reminder, not from the silence
			// that earned it, or the backoff would fire the whole schedule at
			// once. Not a spoke(): the thing itself still has not said anything.
			b.last = time.Now()
		}
		b.mu.Unlock()
		if spoke {
			step = 0 // debounced: it is alive, start the schedule over
			continue
		}
		fire(iv)
		step++
	}
}
