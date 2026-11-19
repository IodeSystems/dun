package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/harness"
)

func pickerModel(t *testing.T, sessions ...harness.SessionInfo) tuiModel {
	t.Helper()
	m := buildModel(2, 2)
	m.sessions = sessions
	m.picking = true
	return m
}

// A session id is a timestamp: it says when, never which. The picker leads with
// the opening ask because that is how a conversation is recognised.
func TestSessionPanel_ShowsWhatIdentifiesASession(t *testing.T) {
	m := pickerModel(t,
		harness.SessionInfo{ID: "20260730-153842", ModTime: time.Now().Add(-2 * time.Hour), Entries: 17, Preview: "why is compaction thrashing", Last: "ship the fix"},
		harness.SessionInfo{ID: "20260729-195624", ModTime: time.Now().Add(-26 * time.Hour), Entries: 50},
	)
	m.sessionID = "20260729-195624"
	panel := stripANSI(m.sessionPanel())
	for _, want := range []string{"20260730-153842", "2h ago", "17 entries", "why is compaction thrashing", "last: ship the fix"} {
		if !strings.Contains(panel, want) {
			t.Errorf("picker is missing %q:\n%s", want, panel)
		}
	}
	// A session whose opening message was compacted away says so rather than
	// showing a blank row.
	if !strings.Contains(panel, "no opening message") {
		t.Errorf("an empty preview should be explained:\n%s", panel)
	}
}

func TestSessionPicker_Navigation(t *testing.T) {
	m := pickerModel(t,
		harness.SessionInfo{ID: "a"}, harness.SessionInfo{ID: "b"}, harness.SessionInfo{ID: "c"},
	)
	down := tea.KeyPressMsg{Code: tea.KeyDown}
	nm, _ := m.Update(down)
	m = nm.(tuiModel)
	nm, _ = m.Update(down)
	m = nm.(tuiModel)
	if m.pickSel != 2 {
		t.Fatalf("↓↓ should select the third session, got %d", m.pickSel)
	}
	nm, _ = m.Update(down) // clamps
	m = nm.(tuiModel)
	if m.pickSel != 2 {
		t.Errorf("selection should clamp at the end, got %d", m.pickSel)
	}
	// esc leaves without switching.
	nm, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = nm.(tuiModel)
	if m.picking || cmd != nil {
		t.Error("esc should close the picker without resuming anything")
	}
}

// Switching conversations must not splice two transcripts onto one screen.
func TestSwitchSession_ClearsTheScrollback(t *testing.T) {
	m := buildModel(5, 2)
	m.sessionID = "old"
	if len(m.convo) == 0 {
		t.Fatal("fixture has no scrollback")
	}
	cmd := m.switchSession("20260730-153842")
	if cmd == nil {
		t.Fatal("switching should spawn an engine on the new session")
	}
	if m.sessionID != "20260730-153842" {
		t.Errorf("session id not updated: %q", m.sessionID)
	}
	if m.skipHistory {
		t.Error("the new session's history is a NEW transcript — it must be replayed")
	}
	// Only the "resuming…" line survives; the previous conversation is gone.
	if txt := convoText(m); strings.Count(txt, "block ") > 0 {
		t.Errorf("previous session's scrollback carried over:\n%s", txt)
	}
	if !strings.Contains(convoText(m), "resuming session") {
		t.Errorf("the switch should be visible: %s", convoText(m))
	}
}

// Resuming the session you are already in reloads it: attached again from
// its start, the screen cleared for its replay (USER 2026-11-19: the way back
// when a reconnect left the screen without its conversation).
func TestSwitchSession_SameSessionReloads(t *testing.T) {
	m := buildModel(5, 2)
	m.sessionID = "same"
	var got TuiOpts
	var at resumeAt
	old := attachFn
	attachFn = func(o TuiOpts, after resumeAt) (*dunProc, error) {
		got, at = o, after
		return &dunProc{stdin: discardWC{}}, nil
	}
	t.Cleanup(func() { attachFn = old })
	cmd := m.switchSession("same")
	if cmd == nil {
		t.Fatal("resuming the current session should attach to it again")
	}
	if txt := convoText(m); strings.Count(txt, "block ") > 0 || !strings.Contains(txt, "reloading session same") {
		t.Errorf("screen not cleared for the replay:\n%s", txt)
	}
	if m.skipHistory || m.sessionID != "same" {
		t.Errorf("skipHistory %v, session %q: the replay must show", m.skipHistory, m.sessionID)
	}
	if msg, ok := cmd().(engineUpMsg); !ok || msg.err != nil {
		t.Fatalf("attach: %#v", msg)
	}
	if got.Resume != "same" || at != (resumeAt{}) {
		t.Errorf("attached to %q after %+v; want same from its start", got.Resume, at)
	}
}

// Deliberately closing an engine still produces an EOF from its reader. A
// supervisor that cannot tell that from a crash restarts the engine you just
// replaced — which is exactly what broke /resume the first time.
func TestStaleEOF_IsNotACrash(t *testing.T) {
	m := buildModel(1, 1)
	current := &dunProc{stdin: discardWC{}}
	m.proc = current
	old := &dunProc{stdin: discardWC{}}

	nm, cmd := m.Update(eofMsg{proc: old})
	m = nm.(tuiModel)
	if cmd != nil {
		t.Error("an EOF from a replaced engine must not trigger a restart")
	}
	if strings.Contains(convoText(m), "restarting") {
		t.Errorf("stale EOF reported as a crash:\n%s", convoText(m))
	}
	// The CURRENT engine dying is still a crash.
	if _, cmd = m.Update(eofMsg{proc: current}); cmd == nil {
		t.Error("the live engine's EOF should still be handled")
	}
}

// --pick: the picker is open at start, no session is attached until it is
// answered; esc starts a new session, enter resumes the chosen one.
func TestPick_OpensPickerAtStart(t *testing.T) {
	m := pickerModel(t, harness.SessionInfo{ID: "a"}, harness.SessionInfo{ID: "b"})
	m.pickAtStart = true
	nm, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = nm.(tuiModel)
	if m.picking || m.pickAtStart || cmd == nil {
		t.Errorf("esc should close the picker and attach a new session: picking=%v pickAtStart=%v cmd=%v", m.picking, m.pickAtStart, cmd != nil)
	}
	m = pickerModel(t, harness.SessionInfo{ID: "a"}, harness.SessionInfo{ID: "b"})
	m.pickAtStart = true
	nm, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	if m.picking || m.pickAtStart || cmd == nil || m.sessionID != "a" {
		t.Errorf("enter should resume the selected session: picking=%v session=%q", m.picking, m.sessionID)
	}
}
