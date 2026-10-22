package proto

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// Log numbers one session's events and fans them out to subscribers.
//
// It keeps the last cap events. A subscriber names the last seq it saw and
// gets everything after it, then the live stream, with no gap and no repeat
// between the two: the replay and the registration happen under one lock. A
// subscriber that asks from before the oldest kept event gets ErrGap and
// must start from a snapshot (State, and the conversation from the session
// file).
//
// A subscriber that does not keep up is dropped, not waited for: one stalled
// phone must not stall the engine or the other clients. Its channel closes
// with Err ErrSlow, and it resubscribes after the last seq it read.
type Log struct {
	session string
	cap     int

	mu     sync.Mutex
	ring   []Event // ring[(seq-1)%cap] holds seq, for the last cap seqs
	next   uint64  // the seq the next Append gets
	subs   map[*Subscription]struct{}
	latest map[string]Event // stateTypes' last event each
	now    func() time.Time
}

var (
	ErrGap    = errors.New("proto: events after that seq are no longer kept")
	ErrFuture = errors.New("proto: seq not reached yet")
	ErrSlow   = errors.New("proto: subscriber fell behind and was dropped")
	ErrClosed = errors.New("proto: log closed")
)

// stateTypes are the events whose latest one is the session's current state
// of that kind: a client arriving late needs only the last of each.
var stateTypes = map[string]bool{
	"session": true, "workspace": true, "ready": true, "server": true,
	"catalog": true, "agents": true, "jobs": true, "context_cost": true,
	"usage": true, "settings": true, "engine": true, "commands": true,
}

func NewLog(session string, capacity int) *Log {
	if capacity < 1 {
		capacity = 1
	}
	return &Log{
		session: session, cap: capacity, ring: make([]Event, capacity), next: 1,
		subs: map[*Subscription]struct{}{}, latest: map[string]Event{}, now: time.Now,
	}
}

// Append numbers ev, stamps it and sends it to every subscriber. The event
// returned is the one sent.
func (l *Log) Append(ev Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	ev.V = Version
	ev.Session = l.session
	ev.Seq = l.next
	if ev.TS.IsZero() {
		ev.TS = l.now()
	}
	l.next++
	l.ring[(ev.Seq-1)%uint64(l.cap)] = ev
	if stateTypes[ev.Type] {
		l.latest[ev.Type] = ev
	}
	for s := range l.subs {
		select {
		case s.c <- ev:
		default:
			s.err = ErrSlow
			l.dropLocked(s)
		}
	}
	return ev
}

// Subscribers is how many subscriptions are live.
func (l *Log) Subscribers() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.subs)
}

// Last is the seq of the newest event, 0 before the first.
func (l *Log) Last() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.next - 1
}

// State is the latest event of each state type, in seq order.
func (l *Log) State() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, 0, len(l.latest))
	for _, ev := range l.latest {
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Subscribe returns the kept events after seq after, and a subscription that
// receives every later one. buffer is how many events may wait unread before
// the subscriber is dropped.
func (l *Log) Subscribe(after uint64, buffer int) ([]Event, *Subscription, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.subs == nil {
		return nil, nil, ErrClosed
	}
	last := l.next - 1
	if after > last {
		return nil, nil, ErrFuture
	}
	oldest := uint64(1)
	if last > uint64(l.cap) {
		oldest = last - uint64(l.cap) + 1
	}
	if after+1 < oldest {
		return nil, nil, ErrGap
	}
	replay := make([]Event, 0, last-after)
	for seq := after + 1; seq <= last; seq++ {
		replay = append(replay, l.ring[(seq-1)%uint64(l.cap)])
	}
	if buffer < 1 {
		buffer = 1
	}
	s := &Subscription{l: l, c: make(chan Event, buffer)}
	l.subs[s] = struct{}{}
	return replay, s, nil
}

// Close ends every subscription with ErrClosed.
func (l *Log) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for s := range l.subs {
		s.err = ErrClosed
		l.dropLocked(s)
	}
	l.subs = nil
}

func (l *Log) dropLocked(s *Subscription) {
	if _, ok := l.subs[s]; !ok {
		return
	}
	delete(l.subs, s)
	close(s.c)
}

// Subscription is one subscriber's live stream.
type Subscription struct {
	l   *Log
	c   chan Event
	err error // set under l.mu before c closes
}

// C delivers events in seq order; it closes when the subscription ends.
func (s *Subscription) C() <-chan Event { return s.c }

// Err says why C closed: ErrSlow, ErrClosed, or nil after Close.
func (s *Subscription) Err() error {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	return s.err
}

// Close ends the subscription.
func (s *Subscription) Close() {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	if s.l.subs != nil {
		s.l.dropLocked(s)
	}
}
