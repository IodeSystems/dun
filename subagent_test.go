package dun

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testAgent is a child with no harness — enough to exercise the state machine,
// which is where every bug in this file has been. Spawning a real one needs an
// LLM; none of these questions do.
func testAgent(t *testing.T, num int, prompt string) (*Harness, *subAgent) {
	t.Helper()
	h := testHarness(t)
	sa := &subAgent{num: num, prompt: prompt, parent: h, hb: fastHeartbeat()}
	return h, sa
}

// E1, the regression that made a wedged child indistinguishable from a working
// one: tell() restarted a child without clearing `ended`, and snapshot measures
// to `ended` whenever it is set — so every later report gave the duration of the
// child's FIRST task. Live, three polls minutes apart all said "2m1s".
func TestSubAgent_EachRunHasItsOwnClock(t *testing.T) {
	_, sa := testAgent(t, 1, "count the lines")

	sa.mu.Lock()
	sa.startRunLocked()
	sa.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	sa.settle(agentIdle)

	_, _, _, first := sa.snapshot()
	if first <= 0 {
		t.Fatalf("a finished run must have a duration, got %s", first)
	}
	time.Sleep(30 * time.Millisecond)
	if _, _, _, again := sa.snapshot(); again != first {
		t.Errorf("a settled run's duration must not keep growing: %s then %s", first, again)
	}

	// A second task is a second run, measured from ITS start.
	sa.mu.Lock()
	sa.startRunLocked()
	sa.mu.Unlock()
	if _, _, _, second := sa.snapshot(); second >= first {
		t.Errorf("a new run must restart the clock: first %s, new run already %s", first, second)
	}
}

// agentkit reports Usage.Total as a RUNNING total, so += double-counted every
// earlier turn each time a run finished.
func TestSubAgent_TokensAreAssignedNotAccumulated(t *testing.T) {
	_, sa := testAgent(t, 2, "read the log")

	sa.setTokens(1000)
	sa.setTokens(2500)
	if got := sa.spent(); got != 2500 {
		t.Fatalf("tokens must track the cumulative total, got %d", got)
	}
}

// A child that answers is IDLE, not gone: it stays resident and re-askable, and
// only quit() ends it.
func TestSubAgent_IdleIsNotDismissed(t *testing.T) {
	_, sa := testAgent(t, 3, "check something")
	sa.mu.Lock()
	sa.startRunLocked()
	sa.mu.Unlock()
	sa.setFinal("42")
	sa.settle(agentIdle)

	state, _, final, _ := sa.snapshot()
	if state != agentIdle || final != "42" {
		t.Fatalf("want idle with an answer, got %s / %q", state, final)
	}
	if r := sa.report(); !strings.Contains(r, "42") || !strings.Contains(r, "re-askable") {
		t.Errorf("the report must lead with the answer and say it is still there: %q", r)
	}
}

// The reminder covers an IDLE child too. One resident child nobody dismissed
// holds its harness forever, and "idle for 40 minutes" is the shape of a leak.
func TestSubAgent_HeartbeatRemindsAboutAnIdleChild(t *testing.T) {
	h, sa := testAgent(t, 4, "summarize the build log")
	sa.mu.Lock()
	sa.startRunLocked()
	sa.mu.Unlock()
	sa.settle(agentIdle)

	go sa.heartbeat()
	defer sa.settle(agentDismissed)

	var seen string
	waitFor(t, 3*time.Second, func() bool {
		for _, n := range h.notes() {
			if strings.Contains(n, "#4") && strings.Contains(n, "idle") {
				seen = n
				return true
			}
		}
		return false
	})
	if !strings.Contains(seen, "quit:true") {
		t.Errorf("a reminder about an idle child must say how to dismiss it: %q", seen)
	}
}

// A blocked child is the loudest case: it never resolves itself, and the parent
// is the only thing that can.
func TestSubAgent_HeartbeatCallsOutABlockedChild(t *testing.T) {
	h, sa := testAgent(t, 5, "pick a strategy")
	sa.mu.Lock()
	sa.startRunLocked()
	sa.answer, sa.question = make(chan string, 1), "which branch?"
	sa.mu.Unlock()

	go sa.heartbeat()
	defer sa.settle(agentDismissed)

	waitFor(t, 3*time.Second, func() bool {
		for _, n := range h.notes() {
			if strings.Contains(n, "WAITING FOR YOU") && strings.Contains(n, "which branch?") {
				return true
			}
		}
		return false
	})
}

// Debounced by anything the child says — including a status, which never
// notifies but is still proof of life. A child narrating its progress is
// exactly the one that should never be asked whether it is alive.
func TestSubAgent_HeartbeatIsDebouncedByStatus(t *testing.T) {
	h, sa := testAgent(t, 6, "long job")
	sa.mu.Lock()
	sa.startRunLocked()
	sa.mu.Unlock()

	go sa.heartbeat()
	defer sa.settle(agentDismissed)

	deadline := time.Now().Add(120 * time.Millisecond)
	for time.Now().Before(deadline) {
		sa.setStatus("still reading")
		time.Sleep(5 * time.Millisecond)
	}
	for _, n := range h.notes() {
		if strings.Contains(n, "still running") {
			t.Fatalf("a child reporting status must not also be reminded about: %q", n)
		}
	}

	// Go quiet and it comes back.
	waitFor(t, 3*time.Second, func() bool {
		for _, n := range h.notes() {
			if strings.Contains(n, "#6") {
				return true
			}
		}
		return false
	})
}

// A dismissed child stops being reminded about — otherwise every agent a
// session ever spawned would keep talking for the life of the session.
func TestSubAgent_HeartbeatStopsWhenDismissed(t *testing.T) {
	h, sa := testAgent(t, 7, "done with this")
	go sa.heartbeat()

	waitFor(t, 3*time.Second, func() bool { return len(h.notes()) > 0 })
	sa.settle(agentDismissed)

	time.Sleep(80 * time.Millisecond)
	h.notes()
	time.Sleep(80 * time.Millisecond)
	if n := h.notes(); len(n) != 0 {
		t.Errorf("a dismissed child must stop reminding: %q", n)
	}
}

// A child's transcript lives beside its parent's, and the id has to survive the
// round trip — restoreAgents recovers a resumed session's children from it.
func TestChildSessionFile_RoundTrips(t *testing.T) {
	base := filepath.Join(t.TempDir(), "20260801-120000.jsonl")
	path := childSessionFile(base, 3)
	if !strings.HasSuffix(path, ".sub3.jsonl") {
		t.Fatalf("unexpected child path: %q", path)
	}
	n, ok := subNumFromPath(base, path)
	if !ok || n != 3 {
		t.Fatalf("id did not survive the round trip: %d %v", n, ok)
	}
	if _, ok := subNumFromPath(base, base); ok {
		t.Error("the parent's own transcript must not read as a child's")
	}
	// An in-memory session gets in-memory children, not a surprise file.
	if p := childSessionFile("", 1); p != "" {
		t.Errorf("an in-memory parent must not invent a path: %q", p)
	}
}

// oneLine is what stops a child's 30k-line answer from becoming the parent's
// context window by accident.
func TestOneLine_FlattensAndClips(t *testing.T) {
	got := oneLine("first\n\n   second\tthird   ", 100)
	if got != "first second third" {
		t.Errorf("whitespace should collapse, got %q", got)
	}
	if got := oneLine(strings.Repeat("x", 50), 10); len([]rune(got)) != 11 || !strings.HasSuffix(got, "…") {
		t.Errorf("long text must be clipped and marked, got %q", got)
	}
}

// Role decides the tool set, and that IS the depth-1 enforcement.
func TestRoleSystem_TellsEachSideWhatItMayDo(t *testing.T) {
	child := roleSystem(true)
	if !strings.Contains(child, "tell_parent") || !strings.Contains(child, "cannot spawn agents") {
		t.Errorf("a child must be told how to report and that it cannot delegate: %q", child)
	}
	if strings.Contains(child, "ask_user") {
		t.Error("a child must not be told about a tool it does not have")
	}
	root := roleSystem(false)
	if !strings.Contains(root, "agent_monitor") || !strings.Contains(root, "Dismiss") {
		t.Errorf("a root must be told how to steer and to clean up: %q", root)
	}
}

// Found on a live run: the dismissal notice said "Its transcript is at ."
// while the file sat on disk perfectly well. quit() dropped the harness before
// asking it where the transcript was — and the transcript is precisely what is
// left of a child once it has been dismissed.
func TestSubAgent_QuitKeepsTheTranscriptPath(t *testing.T) {
	h, sa := testAgent(t, 8, "count the lines")
	path := childSessionFile(h.cfg.SessionFile, 8)
	sa.h = &Harness{cfg: Config{SessionFile: path}}

	out := sa.quit()
	if !strings.Contains(out, path) {
		t.Fatalf("the dismissal must name the transcript that outlives the child: %q", out)
	}
	if got := sa.transcript(); got != path {
		t.Errorf("the path must survive dismissal, got %q", got)
	}
	// A child that never had a transcript says nothing rather than "at .".
	_, bare := testAgent(t, 9, "no session file")
	if out := bare.quit(); strings.Contains(out, "transcript is at .") {
		t.Errorf("an in-memory child must not claim an empty path: %q", out)
	}
}
