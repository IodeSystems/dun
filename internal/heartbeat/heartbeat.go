// Package heartbeat is the "still there" reminder for work that runs outside a
// turn: a background job or a sub-agent.
package heartbeat

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
// quiet; reading what it actually produced stays an explicit ask (jobs).

// schedule is how long silence must last before each successive reminder.
var schedule = []time.Duration{
	time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour,
}

// every is the interval once the schedule above is exhausted.
const every = time.Hour

// Beat tracks when something last spoke and fires while it does not.
//
// The schedule is per-instance rather than read from the package vars at fire
// time: a test shortens it by constructing one, never by writing to a global
// that a running goroutine is reading. The first version did the latter and
// -race caught it.
type Beat struct {
	Sched []time.Duration
	Every time.Duration

	mu   sync.Mutex
	Last time.Time
	// gen is bumped only by Spoke(), so the loop can tell "it said something
	// while I was waiting" (reset the backoff) from "I fired" (advance it).
	gen int
}

func New() *Beat {
	return &Beat{Sched: schedule, Every: every, Last: time.Now()}
}

// Spoke records that the thing produced something on its own, which resets both
// the timer and the backoff — it has just proved it is alive, so the next
// silence deserves the same early attention as the first.
func (b *Beat) Spoke() {
	b.mu.Lock()
	b.Last, b.gen = time.Now(), b.gen+1
	b.mu.Unlock()
}

// interval is how much silence the nth reminder waits for.
func (b *Beat) interval(step int) time.Duration {
	if len(b.Sched) == 0 {
		return b.Every
	}
	if step < len(b.Sched) {
		return b.Sched[step]
	}
	return b.Sched[len(b.Sched)-1] + time.Duration(step-len(b.Sched)+1)*b.Every
}

// Run fires until done reports true. fire is given how long the thing has been
// quiet, which is the only number the reminder is actually about.
func (b *Beat) Run(done func() bool, fire func(quiet time.Duration)) {
	for step := 0; ; {
		b.mu.Lock()
		last, gen := b.Last, b.gen
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
			// once. Not a Spoke(): the thing itself still has not said anything.
			b.Last = time.Now()
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
