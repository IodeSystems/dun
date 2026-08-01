package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/iodesystems/dun"
)

// Switching sessions from inside the TUI.
//
// --continue and --resume only worked at LAUNCH, so changing your mind meant
// quitting, squinting at `dun --sessions` (a bare list of timestamps), and
// starting over. The conversations were always there; there was just no way in.
//
// A session id is a timestamp, which says when but never WHICH — so the picker
// leads with the opening ask. That is how people actually recognise a
// conversation, and it is why this is a picker and not a `/resume <id>` alone
// (which also works, for scripts and for when you know the id).

// resumeSlash implements /resume: bare opens the picker, an argument switches
// straight to that session.
func resumeSlash(m *tuiModel, args []string) tea.Cmd {
	if m.replaying {
		m.append(stDim.Render("this is a replay — there is no engine to resume with"))
		return nil
	}
	if len(args) > 0 {
		return m.switchSession(args[0])
	}
	m.sessions = dun.ListSessionInfo(m.workspace)
	if len(m.sessions) == 0 {
		m.append(stDim.Render("no saved sessions for this workspace yet"))
		return nil
	}
	m.picking, m.pickSel = true, 0
	// Start on the CURRENT session when it is in the list: the common move is
	// "show me the others", and landing on where you already are makes the list
	// orient itself.
	for i, s := range m.sessions {
		if s.ID == m.sessionID {
			m.pickSel = i
			break
		}
	}
	m.input.Blur()
	m.refresh()
	return nil
}

// updatePicking owns the keys while the session list is open.
func (m tuiModel) updatePicking(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c", "q":
		m.picking = false
		m.input.Focus()
		m.refresh()
		return m, nil
	case "up", "k":
		if m.pickSel > 0 {
			m.pickSel--
		}
		m.refresh()
		return m, nil
	case "down", "j":
		if m.pickSel < len(m.sessions)-1 {
			m.pickSel++
		}
		m.refresh()
		return m, nil
	case "enter":
		id := ""
		if m.pickSel >= 0 && m.pickSel < len(m.sessions) {
			id = m.sessions[m.pickSel].ID
		}
		m.picking = false
		m.input.Focus()
		return m, m.switchSession(id)
	}
	return m, nil
}

// switchSession restarts the engine on another conversation.
//
// The scrollback is CLEARED first: it belongs to the session being left, and
// the replacement engine replays the one being joined. Carrying it over would
// splice two conversations into one screen and leave the user reading a
// transcript that never happened.
func (m *tuiModel) switchSession(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	if id == m.sessionID {
		m.append(stDim.Render("already in session " + id))
		m.refresh()
		return nil
	}
	m.proc.close()
	m.convo = nil
	m.cur, m.pendingTool, m.pendingArgs = "", -1, nil
	m.sel, m.blockH = -1, nil
	m.busy, m.asking, m.starting = false, false, true
	m.startingStart = time.Now()
	m.fatalErr, m.exitAnnounced = "", false
	m.restarts, m.restartStart = 0, time.Now()
	m.sessionID = id
	m.skipHistory = false // this one we DO want replayed — it is a new transcript
	m.append(stDim.Render("resuming session " + id + "…"))
	m.refresh()

	o := m.opts
	o.resume, o.cont = id, false
	return restartEngine(o, id, true)
}

// sessionPanel renders the picker in the lower pane, newest first.
func (m tuiModel) sessionPanel() string {
	rows := make([]string, 0, len(m.sessions)+1)
	rows = append(rows, stHeader.Render("sessions")+stDim.Render("  ↑/↓ choose · enter resume · esc cancel"))
	// A long history should not push the input off the screen; show a window
	// around the selection.
	const window = 8
	lo := m.pickSel - window/2
	if lo < 0 {
		lo = 0
	}
	hi := lo + window
	if hi > len(m.sessions) {
		hi = len(m.sessions)
		if lo = hi - window; lo < 0 {
			lo = 0
		}
	}
	for i := lo; i < hi; i++ {
		s := m.sessions[i]
		mark := "  "
		if s.ID == m.sessionID {
			mark = stTool.Render("• ") // where you are now
		}
		line := fmt.Sprintf("%s%s  %s  %s",
			mark, s.ID, stDim.Render(relTime(s.ModTime)), stDim.Render(fmt.Sprintf("%d entries", s.Entries)))
		if p := s.Preview; p != "" {
			line += "\n     " + clip(p, max(20, m.w-12))
		} else {
			line += "\n     " + stDim.Render("(no opening message — compacted or empty)")
		}
		if i == m.pickSel {
			rows = append(rows, addGutter(line, "▎ ", stSel))
		} else {
			rows = append(rows, addGutter(line, "  ", lipgloss.NewStyle()))
		}
	}
	if lo > 0 || hi < len(m.sessions) {
		rows = append(rows, stDim.Render(fmt.Sprintf("     %d-%d of %d", lo+1, hi, len(m.sessions))))
	}
	return strings.Join(rows, "\n")
}

// relTime renders an age the way a person reads one.
func relTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}
