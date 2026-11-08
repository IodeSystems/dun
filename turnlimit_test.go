package dun

import (
	"context"
	"strings"
	"testing"
)

// The limit: the flag, else DUN_MAX_TURNS, else 400.
func TestTurnLimit_FlagEnvDefault(t *testing.T) {
	t.Setenv("DUN_MAX_TURNS", "")
	h := &Harness{}
	if got := h.turnLimit(); got != 400 {
		t.Errorf("default %d", got)
	}
	t.Setenv("DUN_MAX_TURNS", "150")
	if got := h.turnLimit(); got != 150 {
		t.Errorf("env %d", got)
	}
	h.cfg.TurnLimit = 20
	if got := h.turnLimit(); got != 20 {
		t.Errorf("flag %d", got)
	}
	if s := systemTurnLimit(20); !strings.Contains(s, "at most 20 rounds") || !strings.Contains(s, "at 16 you are warned") {
		t.Errorf("prompt line: %s", s)
	}
}

// Warned once, at 80%, with the rounds left; a new turn counts again.
func TestTurnLimit_WarnsOnceAt80Percent(t *testing.T) {
	h := newNoteHarness(t)
	h.cfg.TurnLimit = 10
	warnings := func() (n int, last string) {
		for _, q := range queuedTexts(h) {
			if strings.Contains(q, "[turn limit]") {
				n++
				last = q
			}
		}
		return
	}
	for i := 1; i <= 7; i++ {
		h.noteTurnRound()
	}
	if n, _ := warnings(); n != 0 {
		t.Fatalf("warned before 80%%: %d", n)
	}
	h.noteTurnRound() // round 8
	n, last := warnings()
	if n != 1 || !strings.Contains(last, "8 of this turn's 10") || !strings.Contains(last, "2 are left") {
		t.Fatalf("warning at round 8: %d %q", n, last)
	}
	h.noteTurnRound()
	h.noteTurnRound()
	if n, _ := warnings(); n != 1 {
		t.Errorf("warned %d times in one turn", n)
	}
	// A new turn (prepareTurn: reset, drop the old turn's asides) warns again.
	h.turnRounds.reset()
	h.dropTurnScoped()
	if n, _ := warnings(); n != 0 {
		t.Fatalf("the old turn's warning outlived it: %d", n)
	}
	for i := 1; i <= 8; i++ {
		h.noteTurnRound()
	}
	if n, _ := warnings(); n != 1 {
		t.Errorf("a new turn was not warned again: %d", n)
	}
}

// A started harness with tools says its limit in the system prompt.
func TestTurnLimit_InSystemPrompt(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, err := Start(context.Background(), Config{Workspace: ws, Servers: []Server{}, Exec: &HostShell{Dir: ws}, TurnLimit: 150})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if !strings.Contains(h.Session.System, "at most 150 rounds of tool calls; at 120 you are warned") {
		t.Errorf("no turn limit line in the system prompt")
	}
	if h.Session.MaxTurns != 150 {
		t.Errorf("session MaxTurns %d", h.Session.MaxTurns)
	}
}

// A warning queued in the round that ended a turn does not ride the next one.
func TestTurnLimit_StaleWarningDropped(t *testing.T) {
	h := newNoteHarness(t)
	h.cfg.TurnLimit = 10
	for i := 1; i <= 8; i++ {
		h.noteTurnRound() // round 8 queues the warning; the turn then ends
	}
	h.Aside("[plan] keep me")
	h.turnRounds.reset()
	h.dropTurnScoped()
	for _, q := range queuedTexts(h) {
		if strings.HasPrefix(q, turnLimitTag) {
			t.Errorf("stale warning still queued: %q", q)
		}
	}
	if got := queuedTexts(h); len(got) != 1 || got[0] != "[plan] keep me" {
		t.Errorf("other asides must stay: %q", got)
	}
}
