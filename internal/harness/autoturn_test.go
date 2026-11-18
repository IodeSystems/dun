package harness

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startAuto is an interactive root session that keeps asks, settle off.
func startAuto(t *testing.T, r *nudgeRunner) *Harness {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	h := startLedger(t, r, dir, filepath.Join(dir, "s.jsonl"), func(c *Config) { c.SettleDelay = -1 })
	t.Cleanup(h.Close)
	return h
}

// wakeTurn is one automatic turn: a note and the Continue it wakes.
func wakeTurn(h *Harness, text string) error {
	h.Notify(text)
	_, err := h.Continue(context.Background())
	return err
}

// With an ask open, automatic turns run as before; with none open, three an
// hour, then a wake is a note: no request, counted, and the person's next
// message resets the count and reads the notes.
func TestAutoBudget_OpenAskRunsHeldAfterThreeAndPersonResets(t *testing.T) {
	r := &nudgeRunner{}
	h := startAuto(t, r)
	if _, err := h.Ask(context.Background(), "watch the build"); err != nil {
		t.Fatal(err)
	}
	if !h.openAskNow() {
		t.Fatal("the person's message opened no ask")
	}
	for i := range 5 {
		if err := wakeTurn(h, "job report "+string(rune('a'+i))); err != nil {
			t.Fatalf("wake %d with the ask open: %v", i, err)
		}
	}
	if out := planCall(t, h, "plan_close", `{"outcome":"done","id":"1","account":"watched"}`); !strings.Contains(out, "done") {
		t.Fatalf("close: %s", out)
	}
	for i := range 3 {
		if err := wakeTurn(h, "job report "+string(rune('f'+i))); err != nil {
			t.Fatalf("wake %d, no ask, within the budget: %v", i, err)
		}
	}
	n := r.calls()
	for i, text := range []string{"job held one", "job held two"} {
		if err := wakeTurn(h, text); !errors.Is(err, ErrTurnHeld) {
			t.Fatalf("wake past the budget: %v, want ErrTurnHeld", err)
		}
		if got := h.NotesWaiting(); got != i+1 {
			t.Errorf("notes waiting %d, want %d", got, i+1)
		}
	}
	if r.calls() != n {
		t.Fatalf("a held wake sent %d requests", r.calls()-n)
	}
	if h.Pending() != 0 {
		t.Errorf("held news still pending: %d", h.Pending())
	}
	// dund's "[automatic]" message is automatic too: held.
	if _, err := h.Ask(context.Background(), "[automatic] continue the cut turn"); !errors.Is(err, ErrTurnHeld) || h.NotesWaiting() != 3 {
		t.Fatalf("[automatic] past the budget: %v, %d notes", err, h.NotesWaiting())
	}
	if _, err := h.Ask(context.Background(), "what happened?"); err != nil {
		t.Fatal(err)
	}
	if h.NotesWaiting() != 0 {
		t.Errorf("notes after the person's message: %d", h.NotesWaiting())
	}
	for _, w := range []string{"job held one", "job held two", "continue the cut turn"} {
		if !r.sentHas(n, w) {
			t.Errorf("the person's turn did not read %q", w)
		}
	}
	// The count started again: three more automatic turns run (the ask the
	// message opened is closed first).
	if out := planCall(t, h, "plan_close", `{"outcome":"done","id":"2","account":"told"}`); !strings.Contains(out, "done") {
		t.Fatalf("close: %s", out)
	}
	for i := range 3 {
		if err := wakeTurn(h, "after "+string(rune('a'+i))); err != nil {
			t.Fatalf("wake %d after the reset: %v", i, err)
		}
	}
}

// A blocked ask is not open: the budget applies. A loop's firing is not an
// ask and is never held.
func TestAutoBudget_BlockedAskAndLoopFirings(t *testing.T) {
	r := &nudgeRunner{}
	h := startAuto(t, r)
	if _, err := h.Ask(context.Background(), "deploy it"); err != nil {
		t.Fatal(err)
	}
	if out := planCall(t, h, "plan_close", `{"outcome":"block","id":"1","reason":"which region?"}`); !strings.Contains(out, "blocked") {
		t.Fatalf("block: %s", out)
	}
	if h.openAskNow() {
		t.Fatal("a blocked ask counts as open")
	}
	for range 3 {
		if err := wakeTurn(h, "tick"); err != nil {
			t.Fatal(err)
		}
	}
	if err := wakeTurn(h, "tick"); !errors.Is(err, ErrTurnHeld) {
		t.Fatalf("fourth wake with only a blocked ask: %v", err)
	}
	nodes := len(planGraph(t, h).Nodes)
	for range 2 {
		if _, err := h.Ask(context.Background(), "[loop 4, run 2, every 20m: loop_ctl can stop it] check the deploy"); err != nil {
			t.Fatalf("a loop firing was held: %v", err)
		}
	}
	if g := planGraph(t, h); len(g.Nodes) != nodes {
		t.Errorf("a loop firing opened an ask: %s", g.List())
	}
	if h.NotesWaiting() != 1 {
		t.Errorf("a loop firing reset the notes: %d", h.NotesWaiting())
	}
}

// The budget's window is an hour, rolling.
func TestAutoBudget_WindowSlides(t *testing.T) {
	h := startAuto(t, &nudgeRunner{})
	now := time.Now()
	h.auto.now = func() time.Time { return now }
	for range autoTurnsPerHour {
		if !h.autoAllow() {
			t.Fatal("refused within the budget")
		}
	}
	if h.autoAllow() {
		t.Fatal("allowed past the budget")
	}
	now = now.Add(autoTurnWindow + time.Second)
	if !h.autoAllow() {
		t.Error("still refused an hour later")
	}
}

// Settle is armed by the person's turn only: not a [scheduled], [loop] or
// [automatic] message.
func TestSettle_NotArmedByAutomaticMessages(t *testing.T) {
	var rounds []roundFn
	for range 4 {
		rounds = append(rounds, toolRound("c", "x"), textRound("done"))
	}
	h := startSettle(t, &scriptRunner{rounds: rounds}, "", nil)
	for _, msg := range []string{
		"[scheduled 10:00 for 10:05] run the report",
		"[loop 3, run 2, every 20m: loop_ctl can stop it] check the build",
		"[automatic] continue the cut turn",
	} {
		if _, err := h.Ask(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
		if settleArmed(h) {
			t.Fatalf("armed by %q", msg)
		}
	}
	if _, err := h.Ask(context.Background(), "do the work"); err != nil {
		t.Fatal(err)
	}
	if !settleArmed(h) {
		t.Error("the person's turn did not arm")
	}
}

// A [loop] or [scheduled] firing is not the person's message.
func TestPersonText(t *testing.T) {
	for text, want := range map[string]bool{
		"fix the test":                       true,
		"[wip] fix":                          false, // a bracketed tag: misread, accepted (asks.go)
		"[automatic] continue":               false,
		"[scheduled 10:00 for 10:05] run it": false,
		"[loop 4, run 2, every 20m: loop_ctl can stop it] check": false,
		"  ": false,
	} {
		if got := PersonText(text); got != want {
			t.Errorf("PersonText(%q) = %v, want %v", text, got, want)
		}
	}
}
