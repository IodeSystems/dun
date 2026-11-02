package dun

import (
	"sync"
	"time"
)

// serverwake.go — a server's message starting a turn.
//
// A notifications/message reaches the model in its next tool result, so a running
// turn hears it at once and an idle session does not hear it at all until someone
// types. For a server whose news is something to act on (a proposal was decided, a
// request can be claimed), that is the wrong way round: nothing is running, and
// that is when it matters.
//
// So a server can be set to wake an idle session: "wake": true in mcp.json. Off by
// default, and the user's switch, not the server's, because every wake is a turn
// somebody pays for. Only what would reach the model anyway wakes it: index, setup
// and daemon reports, and JSON notices, are handled before this.
//
// And it is rate limited, because a turn can make a server speak, which can wake a
// turn. A message over the limit is not lost: it is queued, and the next turn has
// it. One within the gap gets a wake when the gap ends.

const (
	serverWakeGap = 30 * time.Second // between one server's wakes
	serverWakeMax = 20               // one server's wakes an hour
)

// serverWake is one server's wakes.
type serverWake struct {
	last    time.Time
	hour    time.Time // when this hour's count began
	count   int
	waiting bool // a wake is set for the end of the gap
}

// allow says whether a wake may go now; if not, how long until one may. 0 and
// false means none this hour.
func (w *serverWake) allow(now time.Time) (bool, time.Duration) {
	if now.Sub(w.hour) >= time.Hour {
		w.hour, w.count = now, 0
	}
	if w.count >= serverWakeMax {
		return false, 0
	}
	if since := now.Sub(w.last); !w.last.IsZero() && since < serverWakeGap {
		return false, serverWakeGap - since
	}
	w.last = now
	w.count++
	return true, 0
}

// serverWakes is the session's wakes, by server.
type serverWakes struct {
	mu sync.Mutex
	by map[string]*serverWake
}

func (s *serverWakes) of(id string) *serverWake {
	if s.by == nil {
		s.by = map[string]*serverWake{}
	}
	w := s.by[id]
	if w == nil {
		w = &serverWake{}
		s.by[id] = w
	}
	return w
}

// wakeForServer wakes the session if id is set to, within the limit. A wake on a
// session that is already working is no cost: Continue finds nothing left.
//
// On its own goroutine, because a notification arrives on the MCP client's read
// goroutine, and the server list's lock can be held by a start that is waiting
// on that same goroutine.
func (h *Harness) wakeForServer(id string) {
	go func() {
		s, err := h.knownServer(id)
		if err != nil || !s.Wake {
			return
		}
		h.serverWakeNow(id)
	}()
}

func (h *Harness) serverWakeNow(id string) {
	h.wakes.mu.Lock()
	defer h.wakes.mu.Unlock()
	w := h.wakes.of(id)
	ok, after := w.allow(time.Now())
	switch {
	case ok:
		h.signalWake()
	case after > 0 && !w.waiting:
		w.waiting = true
		time.AfterFunc(after, func() {
			h.wakes.mu.Lock()
			w.waiting = false
			h.wakes.mu.Unlock()
			h.serverWakeNow(id)
		})
	case after == 0:
		h.noteServer(id, "wake: %d an hour reached; its messages wait for the next turn", serverWakeMax)
	}
}

func (h *Harness) signalWake() {
	select {
	case h.wake <- struct{}{}:
	default: // a turn is already due
	}
}
