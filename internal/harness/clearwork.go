package harness

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/taskgraph"
)

// clearwork.go — what /clear asks about before it clears (USER 2026-11-24:
// "/clear should confirm with the user about background operations if
// running: stop, keep (new agent will be notified about them) — same with
// plan items"). Reset clears the conversation only: background jobs run on,
// and the asks and the plan are sidecars kept under the same session id. So
// the engine (cmd/dun clear.go) asks first, and does what the person chose
// with these: StopJobsForClear, DropOpenWork, or ClearKept to tell the new
// conversation what it carries over.

// ClearItem is one open ask or plan item /clear asks about.
type ClearItem struct {
	Kind  string // "ask", "node"
	ID    string // the ask's number, the node's id
	Title string
}

// ClearDroppedWhy is the reason an ask or plan node dropped by /clear gives.
const ClearDroppedWhy = "the conversation was cleared"

// ClearItems is the open asks and the open plan nodes, what the model is
// shown in the [asks] and [plan] blocks. Not the intake (messages the model
// never acknowledged; USER 2026-11-24: always dropped on /clear, never
// asked about: DropIntake). The fold's ask node is not one either: it is
// the cleared conversation's, and Reset drops it.
func (h *Harness) ClearItems() []ClearItem {
	if h.isChild() {
		return nil
	}
	var out []ClearItem
	if h.asksOn() {
		h.asks.mu.Lock()
		d := h.asksLocked()
		for _, r := range d.Asks {
			if r.Outcome == "" {
				out = append(out, ClearItem{Kind: "ask", ID: strconv.Itoa(r.N), Title: r.Title})
			}
		}
		h.asks.mu.Unlock()
	}
	g, err := planStore{h}.View(context.Background())
	if err != nil {
		return out
	}
	for _, n := range g.Nodes {
		if planOpen(n) && !strings.Contains(n.Details, askTag) {
			out = append(out, ClearItem{Kind: "node", ID: n.ID, Title: n.Title})
		}
	}
	return out
}

// DropIntake empties the intake: the person's messages the model never
// acknowledged. /clear always does this, whatever else it keeps.
func (h *Harness) DropIntake() {
	if h.isChild() {
		return
	}
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	d := h.asksLocked()
	if len(d.Intake) > 0 {
		d.Intake = nil
		h.asksSaveLocked()
	}
	if h.asks.cur == askIntakeID {
		h.asks.cur = ""
	}
}

// DropOpenWork drops what ClearItems lists (and the intake): each open
// ask is closed as dropped (its /journal entry written), each open plan node
// is dropped, all with why.
func (h *Harness) DropOpenWork(why string) {
	if h.isChild() {
		return
	}
	now := time.Now()
	var closed []AskLedger
	h.asks.mu.Lock()
	d := h.asksLocked()
	changed := len(d.Intake) > 0
	d.Intake = nil
	for _, r := range d.Asks {
		if r.Outcome != "" {
			continue
		}
		r.Outcome, r.Note, r.Closed, r.Blocked, r.Journaled = "dropped", why, now, "", true
		closed = append(closed, r.AskLedger)
		changed = true
	}
	h.asks.cur = ""
	if changed {
		h.asksSaveLocked()
	}
	h.asks.mu.Unlock()
	for _, a := range closed {
		h.askJournal(context.Background(), a)
	}

	dropped := 0
	err := h.planNow().store.Update(context.Background(), func(g *taskgraph.Graph) error {
		for _, n := range g.Nodes {
			if planOpen(n) && g.SetStatus(n.ID, taskgraph.Dropped, why) == nil {
				dropped++
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("dun: clear: plan nodes not dropped: %v", err)
	}
	if dropped > 0 {
		h.planChanged()
	}
}

// StopJobsForClear kills every job still running without waiting for it,
// and mutes it first: its completion notice would land in the cleared
// conversation, about a job it never started. It returns how many it
// stopped. The jobs pane still shows each one end (finish pushes the list).
func (h *Harness) StopJobsForClear(by string) int {
	n := 0
	for _, j := range h.liveJobs() {
		j.mu.Lock()
		j.mon.Mute = true
		j.mu.Unlock()
		if j.kill(by) {
			n++
		}
	}
	return n
}

// ClearKept tells the cleared conversation what it carries over from the
// one before: the jobs still running (their notices arrive later) and the
// open asks and plan items. An aside: it rides the first turn after the
// clear and wakes none. Nothing kept, nothing said.
func (h *Harness) ClearKept(jobs []JobInfo, items []ClearItem) {
	if text := ClearKeptNote(jobs, items); text != "" {
		h.AsideWith(text, AsideOpts{Key: "clear-kept", Tag: "clear"})
	}
}

// ClearKeptNote is ClearKept's text, "" with nothing kept.
func ClearKeptNote(jobs []JobInfo, items []ClearItem) string {
	if len(jobs) == 0 && len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[clear] The person cleared the conversation and kept these from before it.")
	if len(jobs) > 0 {
		b.WriteString("\nBackground jobs still running (each one's completion notice arrives here as usual):")
		for _, j := range jobs {
			fmt.Fprintf(&b, "\n- #%d `%s` (started %s)", j.ID, ShortCommand(j.Command, 80),
				time.Unix(j.Started, 0).Local().Format("15:04"))
		}
	}
	if len(items) > 0 {
		b.WriteString("\nOpen asks and plan items carried over (the [asks] and [plan] blocks have them):")
		for _, it := range items {
			fmt.Fprintf(&b, "\n- %s", ClearItemLine(it))
		}
	}
	return b.String()
}

// ClearItemLine is one item as a short line: "ask 2 fix the flaky test",
// "plan n4 write the docs".
func ClearItemLine(it ClearItem) string {
	switch it.Kind {
	case "node":
		return "plan " + it.ID + " " + it.Title
	}
	return it.Kind + " " + it.ID + " " + it.Title
}
