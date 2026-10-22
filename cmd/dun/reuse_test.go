package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// waitDetached waits until dund counts no client in session id.
func waitDetached(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list, err := dundSessions()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.ID == id && s.Attached == 0 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s still has a client attached", id)
}

// A plain dun resumes the workspace's newest session nobody is in, and says
// so first; one that somebody is in is passed over; --new always creates.
func TestAttachReusesTheIdleSession(t *testing.T) {
	testHome(t)
	ws := t.TempDir()
	startTestDund(t)
	o := tuiOpts{workspace: ws}

	first, err := attachSession(o, resumeAt{})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitFor(t, first, "ready"); slices.Contains(got, "reused") {
		t.Fatalf("the first dun in a workspace reused something: %v", got)
	}

	// Another TUI while the first is attached: a session of its own.
	second, err := attachSession(o, resumeAt{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, second, "ready")
	if second.session == first.session {
		t.Fatal("a second TUI joined the session the first is attached to")
	}

	// The first leaves (ctrl+c detaches): the next dun lands in its session,
	// the newest one nobody is in, and the first line says which.
	first.close()
	waitDetached(t, first.session)
	third, err := attachSession(o, resumeAt{})
	if err != nil {
		t.Fatal(err)
	}
	if third.session != first.session {
		t.Fatalf("resumed %s, want the idle %s", third.session, first.session)
	}
	msg := (<-third.ch).(evMsg)
	if msg["type"] != "reused" || !strings.Contains(str(msg["text"]), "resumed "+first.session) ||
		!strings.Contains(str(msg["text"]), "/new for a fresh session") {
		t.Fatalf("first event: %v", msg)
	}
	waitFor(t, third, "ready") // the session replayed from its start

	// --new: a fresh session although one is idle.
	third.close()
	waitDetached(t, third.session)
	o.fresh = true
	fresh, err := attachSession(o, resumeAt{})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitFor(t, fresh, "ready"); slices.Contains(got, "reused") || fresh.session == first.session || fresh.session == second.session {
		t.Fatalf("--new reused: %s %v", fresh.session, got)
	}
}

// --new is the client's choice: it reaches the --tui a served terminal runs,
// never the engine (whose flags dund checks).
func TestNewFlagStaysWithTheClient(t *testing.T) {
	o := tuiOpts{workspace: "/ws", fresh: true, ship: true}
	if slices.Contains(engineArgs(o), "--new") {
		t.Errorf("engine args: %v", engineArgs(o))
	}
	if !slices.Contains(procArgs(o, "--tui"), "--new") {
		t.Errorf("--tui args: %v", procArgs(o, "--tui"))
	}
}

// /new leaves the session on screen for a fresh one: scrollback and
// suggestions go, and nothing points back at the old session, so a failed
// attach cannot retry into it.
func TestNewSlashStartsAFreshSession(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = resized(m, 80, 24)
	m = m.handleEvent(evMsg{"type": "session", "id": "20261022-100001"})
	m = m.handleEvent(evMsg{"type": "user", "content": "hi"})
	m = m.handleEvent(evMsg{"type": "suggestions", "items": []any{map[string]any{"text": "next", "prob": 0.5}}})
	m.opts.workspace = "/ws"
	cmd := newSlash(&m, nil)
	if cmd == nil {
		t.Fatal("/new started nothing")
	}
	if m.sessionID != "" || m.everUp || !m.opts.fresh || m.opts.resume != "" || m.opts.cont {
		t.Fatalf("still tied to the old session: id=%q everUp=%v opts=%+v", m.sessionID, m.everUp, m.opts)
	}
	if len(m.suggestions) != 0 || m.asked() {
		t.Fatal("the old conversation's suggestions or messages are still on screen")
	}
	if !m.starting {
		t.Error("not waiting for the new engine")
	}
}

// /resume to another session drops the suggestions of the one left.
func TestSwitchSessionDropsSuggestions(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = resized(m, 80, 24)
	m = m.handleEvent(evMsg{"type": "session", "id": "20261022-100001"})
	m = m.handleEvent(evMsg{"type": "user", "content": "hi"})
	m = m.handleEvent(evMsg{"type": "suggestions", "items": []any{map[string]any{"text": "next", "prob": 0.5}}})
	if len(m.suggestions) == 0 {
		t.Fatal("setup: no suggestions")
	}
	m.switchSession("20261022-100002")
	if len(m.suggestions) != 0 || m.suggestActive() {
		t.Fatal("suggestions carried over to another session")
	}
}
