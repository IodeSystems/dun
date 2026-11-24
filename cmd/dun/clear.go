package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

// /clear while a turn runs (USER 2026-11-24: "/clear takes a while").
//
// Before, the clear was done at once on the reader goroutine and the turn
// ran on: the screen cleared, but the session stayed busy until the turn
// ended (16s for an exec still in its grace, measured), a message typed
// after the clear rode the OLD turn, and that turn's tool calls and reply
// were written into the cleared conversation.
//
// Now a clear with a turn running interrupts it and says so (proto.Clearing,
// naming what the turn was doing); the turn loop clears the conversation the
// moment the turn is over (clearing.take), before it runs anything else. A
// message sent meanwhile is queued as any message sent mid-turn is (the
// gate), is not lifted into the interrupted turn (liftQueued), survives the
// clear (Reset keeps the queue) and runs in the cleared conversation. With
// no turn running the clear is done at once, as before. reset carries how
// long it took (proto.Timing).

// clearing is a /clear waiting for the running turn to stop.
type clearing struct {
	mu      sync.Mutex
	pending *harness.PhaseClock // the clear's clock; nil: none waits
	waiting string              // what the turn was doing, the clock's phase
	choice  clearChoice         // what becomes of the jobs and the plan
}

// begin starts a clear: when stop reports a turn it stopped, the clear
// waits for the loop (true); otherwise there is nothing to wait for
// (false), and the caller clears now. Under the lock, so the loop's take
// sees either no clear or one whose turn was told to stop.
func (c *clearing) begin(clk *harness.PhaseClock, waiting string, choice clearChoice, stop func() bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil {
		return true // a second /clear while the first waits: the same clear
	}
	if !stop() {
		return false
	}
	c.pending, c.waiting, c.choice = clk, waiting, choice
	return true
}

// take returns the clear that waits, once, the phase it waited in and its
// choice.
func (c *clearing) take() (*harness.PhaseClock, string, clearChoice) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clk, w, ch := c.pending, c.waiting, c.choice
	c.pending, c.waiting, c.choice = nil, "", clearChoice{}
	return clk, w, ch
}

// waitingFor is what the running turn is doing, in words, for the clearing
// event: its tool, a question it waits on, a provider wait, or the reply.
func (t *turnTracker) waitingFor() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.st.Tool != "":
		return t.st.Tool
	case t.st.Phase == proto.PhaseWaitingAnswer:
		return "the open question"
	case t.st.Phase == proto.PhaseRetrying:
		return "a provider retry"
	}
	return "the model's reply"
}

// clearNow clears the conversation and says so, with the clear's timing:
// the jobs stopped or kept, the open items dropped or kept as c says, and
// what is kept told to the new conversation (harness.ClearKept).
func clearNow(h *harness.Harness, em *emitter, clk *harness.PhaseClock, c clearChoice) {
	if c.jobs == "stop" {
		h.StopJobsForClear("user") // again: the stopped turn may have started one
	}
	if c.plan == "drop" {
		h.DropOpenWork(harness.ClearDroppedWhy)
	}
	h.DropIntake() // unacknowledged messages never carry over, kept or not
	jobs, items := clearKept(h, c)
	h.Reset()
	h.ClearKept(jobs, items)
	clk.Mark("clear")
	em.emit(&proto.ResetEvent{Timing: clk.Timing()})
}

// newClear is a clear's clock, started now.
func newClear() *harness.PhaseClock { return harness.NewPhaseClock(time.Now()) }

// ── what outlives the conversation (USER 2026-11-24) ────────────────────
//
// "/clear should confirm with the user about background operations if
// running: stop, keep (new agent will be notified about them) — same with
// plan items". A bare /clear with background jobs running, or open asks or
// plan items, is not done: the engine says what it found (proto.
// ClearConfirm) and the client asks. The answer comes back as the slash
// line with the choice on it; a client that does not ask shows the
// message, which names that line. With nothing to ask about the clear is
// done at once. Asked while a turn runs, the turn runs on until the
// answer; the interrupt and the clear come with the answer.

// clearUsage is /clear's argument form.
const clearUsage = "usage: /clear [keep|drop] [jobs=keep|stop] [plan=keep|drop]"

// clearChoice is what becomes of each category: jobs keep|stop, plan
// keep|drop; "" not said (asked about when there is something).
type clearChoice struct{ jobs, plan string }

// parseClear reads /clear's arguments: keep (both kept), drop or stop (jobs
// stopped, plan dropped), jobs=… and plan=…. stop and drop are one word for
// both categories.
func parseClear(args []string) (clearChoice, error) {
	var c clearChoice
	word := func(v string) string {
		switch v {
		case "keep":
			return "keep"
		case "stop", "drop":
			return "drop"
		}
		return ""
	}
	for _, a := range args {
		k, v, kv := strings.Cut(strings.ToLower(a), "=")
		switch {
		case !kv && word(k) != "":
			c.jobs, c.plan = word(k), word(k)
		case kv && k == "jobs" && word(v) != "":
			c.jobs = word(v)
		case kv && k == "plan" && word(v) != "":
			c.plan = word(v)
		default:
			return clearChoice{}, fmt.Errorf("%s (not %q)", clearUsage, a)
		}
	}
	if c.jobs == "drop" {
		c.jobs = "stop"
	}
	return c, nil
}

// clearQuestion is what a /clear with choice c must ask before it clears,
// nil when nothing: the running jobs when c does not say, the open asks and
// plan items when c does not say.
func clearQuestion(h *harness.Harness, c clearChoice) *proto.ClearConfirm {
	var q proto.ClearConfirm
	if c.jobs == "" {
		if js := h.RunningJobs(); len(js) > 0 {
			q.Jobs = jobsProto(js)
		}
	}
	if c.plan == "" {
		for _, it := range h.ClearItems() {
			q.Plan = append(q.Plan, proto.ClearItem{Kind: it.Kind, ID: it.ID, Title: it.Title})
		}
	}
	if len(q.Jobs) == 0 && len(q.Plan) == 0 {
		return nil
	}
	q.Message = clearMessage(&q)
	return &q
}

// clearMessage is the question in words, for a client that does not ask.
func clearMessage(q *proto.ClearConfirm) string {
	var found, forms []string
	if n := len(q.Jobs); n > 0 {
		found = append(found, textfmt.Plural(n, "background job")+" running")
		forms = append(forms, "jobs=keep|stop")
	}
	if n := len(q.Plan); n > 0 {
		w := fmt.Sprintf("%d open asks and plan items", n)
		if n == 1 {
			w = "1 open ask or plan item"
		}
		found = append(found, w)
		forms = append(forms, "plan=keep|drop")
	}
	return "not cleared: " + strings.Join(found, ", ") + ". Say what becomes of them: /clear " +
		strings.Join(forms, " ") + " (keep: the new conversation is told about them), or /clear keep, /clear drop for all"
}

// clearKept is what a clear with choice c carries over, read as it clears:
// the jobs still running unless stopped, the open items unless dropped. A
// category c does not name is kept (there was nothing to ask about when the
// clear began, or an older client sent it).
func clearKept(h *harness.Harness, c clearChoice) ([]harness.JobInfo, []harness.ClearItem) {
	var jobs []harness.JobInfo
	var items []harness.ClearItem
	if c.jobs != "stop" {
		jobs = h.RunningJobs()
	}
	if c.plan != "drop" {
		items = h.ClearItems()
	}
	return jobs, items
}
