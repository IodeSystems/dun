package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/iodesystems/dun"
)

func pickerModel(t *testing.T, sessions ...dun.SessionInfo) tuiModel {
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
		dun.SessionInfo{ID: "20260730-153842", ModTime: time.Now().Add(-2 * time.Hour), Entries: 17, Preview: "why is compaction thrashing"},
		dun.SessionInfo{ID: "20260729-195624", ModTime: time.Now().Add(-26 * time.Hour), Entries: 50},
	)
	m.sessionID = "20260729-195624"
	panel := m.sessionPanel()
	for _, want := range []string{"20260730-153842", "2h ago", "17 entries", "why is compaction thrashing"} {
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
		dun.SessionInfo{ID: "a"}, dun.SessionInfo{ID: "b"}, dun.SessionInfo{ID: "c"},
	)
	down := tea.KeyMsg{Type: tea.KeyDown}
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
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
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

// Resuming the session you are already in is a no-op, not a pointless restart.
func TestSwitchSession_SameSessionIsANoop(t *testing.T) {
	m := buildModel(1, 1)
	m.sessionID = "same"
	if cmd := m.switchSession("same"); cmd != nil {
		t.Error("switching to the current session should not restart the engine")
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
