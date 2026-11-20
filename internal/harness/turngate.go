package harness

import (
	"sync"
	"sync/atomic"
)

// turngate.go — how a person's message enters a session that a loop drives.
//
// One mechanism for the main session (cmd/dun/engine.go: its input router
// and its turn loop) and for a sub-agent (subagent.go: tell and work). A
// message that arrives while a turn runs never waits for the turn and is
// never dropped between the turn's end and the loop's look for news:
//
//   - text is queued (SayAs) and read at the next round boundary: inside
//     the next tool result, or in a turn of its own after the last round;
//   - a message with images is held for the turn after the running one (an
//     image cannot ride a tool result), and starts that turn;
//   - with no turn running the gate refuses it, and the caller starts a
//     turn with it.
//
// The check that a turn runs and the queueing are under one lock, and so
// is a loop's decision to go idle (Settle): a message lands before that
// decision, which then sees it, or after, when it starts a turn of its own.

// UserInput is one message: its id (the user event's; "" for a new one),
// its text and the images attached to it.
type UserInput struct {
	ID     string
	Text   string
	Images []ImageInput
}

// Offered is what the gate did with a message.
type Offered int

const (
	OfferIdle   Offered = iota // no turn runs: the caller starts one with it
	OfferQueued                // queued (SayAs) for the running turn
	OfferHeld                  // it has images: held for the turn after this one
)

// TurnGate is the harness's input gate. The zero value is ready.
type TurnGate struct {
	mu     sync.Mutex
	active atomic.Bool // written under mu; read without it by Active
	news   uint64      // messages taken while a turn ran
	held   []UserInput
	heldCh chan struct{} // cap 1: a held message is waiting
}

// Gate is h's input gate.
func (h *Harness) Gate() *TurnGate { return &h.gate }

// Held is ready when a message with images was held (TakeHeld takes it).
func (g *TurnGate) Held() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.heldChLocked()
}

func (g *TurnGate) heldChLocked() chan struct{} {
	if g.heldCh == nil {
		g.heldCh = make(chan struct{}, 1)
	}
	return g.heldCh
}

// Begin marks a turn running: a message from now on is queued for it.
func (g *TurnGate) Begin() {
	g.mu.Lock()
	g.active.Store(true)
	g.mu.Unlock()
}

// End marks the turn over. A loop that looks for news after End (the
// engine's) sees every message queued before it.
func (g *TurnGate) End() {
	g.mu.Lock()
	g.active.Store(false)
	g.mu.Unlock()
}

// Settle ends the turns unless more (given News) reports that there is
// still something to run, or a message is held. Under the gate's lock, so
// a message is either seen by more or refused by Offer (and starts a
// turn). Reports whether the gate went idle.
func (g *TurnGate) Settle(more func(news uint64) bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.held) > 0 || more(g.news) {
		return false
	}
	g.active.Store(false)
	return true
}

// Active reports whether a turn runs, without the gate's lock: a hint for
// a caller that holds a lock of its own (subagent.go, tell); Offer decides.
func (g *TurnGate) Active() bool { return g.active.Load() }

// News counts the messages the gate has taken for a running turn. A loop
// compares two readings to know whether a message came in between.
func (g *TurnGate) News() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.news
}

// Offer gives the running turn msg: queued on h (SayAs), or held when it
// has images. onQueue, when set, runs just before the message is queued,
// under the gate's lock (the engine's echo, which must come first).
func (g *TurnGate) Offer(h *Harness, msg UserInput, onQueue func()) Offered {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.active.Load() {
		return OfferIdle
	}
	g.news++
	if len(msg.Images) > 0 {
		g.held = append(g.held, msg)
		select {
		case g.heldChLocked() <- struct{}{}:
		default:
		}
		return OfferHeld
	}
	if onQueue != nil {
		onQueue()
	}
	id := msg.ID
	if id == "" {
		id = NewMessageID()
	}
	h.SayAs(id, msg.Text)
	return OfferQueued
}

// TakeHeld returns the oldest held message.
func (g *TurnGate) TakeHeld() (UserInput, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.held) == 0 {
		return UserInput{}, false
	}
	m := g.held[0]
	g.held = g.held[1:]
	return m, true
}
