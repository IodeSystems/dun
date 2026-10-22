package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// teaRuntime is a small stand-in for Bubble Tea's: Update on one goroutine, every
// Cmd on its own — which is the property the bug needed (two Cmds reading one
// channel deliver in the order they happen to finish).
type teaRuntime struct {
	m    tuiModel
	msgs chan tea.Msg
}

func (r *teaRuntime) run(c tea.Cmd) {
	if c == nil {
		return
	}
	go func() {
		msg := c()
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				r.run(c)
			}
			return
		}
		if msg != nil {
			r.msgs <- msg
		}
	}()
}

func (r *teaRuntime) send(msg tea.Msg) { r.msgs <- msg }

// until applies messages until done says so, or fails after a while.
func (r *teaRuntime) until(t *testing.T, done func(tuiModel) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !done(r.m) {
		select {
		case msg := <-r.msgs:
			nm, c := r.m.Update(msg)
			r.m = nm.(tuiModel)
			r.run(c)
		case <-deadline:
			t.Fatalf("timed out; reply so far: %q", r.m.cur)
		}
	}
}

// A connection replaced while it is still delivering (a /reconnect, a
// session or model switch) must not leave two readers on the new one: they
// would deliver its events out of order, and a streamed reply would be
// stored shuffled — the scrambled text the user saw.
func TestTUI_ReplacedConnectionDoesNotShuffleEvents(t *testing.T) {
	old := &dunProc{stdin: discardWC{}, ch: make(chan tea.Msg, 256)}
	cur := &dunProc{stdin: discardWC{}, ch: make(chan tea.Msg, 256)}
	m := newTUIModel(old, "/ws")
	m = resized(m, 100, 30)
	m.starting = false
	r := &teaRuntime{m: m, msgs: make(chan tea.Msg, 1024)}
	r.run(waitEvent(old)) // the old connection's reader, as Init arms it

	// The replacement comes up while the old one still has events on the way.
	r.send(engineUpMsg{proc: cur, reattach: true})
	r.until(t, func(m tuiModel) bool { return m.proc == cur })
	old.ch <- evMsg{"type": "token", "text": "STALE"}

	var want strings.Builder
	toks := make([]string, 400)
	for i := range toks {
		toks[i] = fmt.Sprintf("w%03d ", i)
		want.WriteString(toks[i])
	}
	go func() { // the engine streaming, as the attach reader feeds the channel
		for _, tok := range toks {
			cur.ch <- evMsg{"type": "token", "text": tok}
		}
	}()
	r.until(t, func(m tuiModel) bool { return len(m.cur) >= want.Len() })
	if r.m.cur != want.String() {
		t.Fatalf("the reply arrived out of order or with the old connection's events:\n got %.200q\nwant %.200q", r.m.cur, want.String())
	}
	old.ch <- eofMsg{proc: old} // the old reader ends; nothing to re-arm
}
