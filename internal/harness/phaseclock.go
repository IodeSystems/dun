package harness

import (
	"sync"
	"time"

	"github.com/iodesystems/dun/proto"
)

// PhaseClock times an operation in named phases: the engine's start (from
// its process start to ready) and a /clear. Mark ends the phase running
// since the last mark. A client shows where the time went when the whole
// is long (proto.Timing). Safe on any goroutine; the zero value is not
// ready, use NewPhaseClock.
type PhaseClock struct {
	mu     sync.Mutex
	start  time.Time
	last   time.Time
	phases []proto.Phase
}

// NewPhaseClock starts a clock at start.
func NewPhaseClock(start time.Time) *PhaseClock {
	return &PhaseClock{start: start, last: start}
}

// Mark ends the phase name: the time since the last mark is its. A phase
// marked twice adds up. Nil-safe.
func (c *PhaseClock) Mark(name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	d := now.Sub(c.last).Milliseconds()
	c.last = now
	for i := range c.phases {
		if c.phases[i].Name == name {
			c.phases[i].MS += d
			return
		}
	}
	c.phases = append(c.phases, proto.Phase{Name: name, MS: d})
}

// Timing is the clock read now: the total since start, and the phases
// marked so far. Nil-safe (nil).
func (c *PhaseClock) Timing() *proto.Timing {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return &proto.Timing{TotalMS: time.Since(c.start).Milliseconds(), Phases: append([]proto.Phase(nil), c.phases...)}
}
