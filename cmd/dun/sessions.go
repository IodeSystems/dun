package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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
	m.addServiceSessions()
	if len(m.sessions) == 0 {
		m.append(stDim.Render("no saved sessions yet"))
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

// addServiceSessions adds the sessions dund runs in OTHER workspaces after
// this workspace's, and notes which have a live engine. A failure to reach
// dund leaves the list as the files say.
func (m *tuiModel) addServiceSessions() {
	m.sessWS, m.sessLive = map[string]string{}, map[string]bool{}
	list, err := dundSessions()
	if err != nil {
		return
	}
	have := map[string]bool{}
	for _, s := range m.sessions {
		have[s.ID] = true
	}
	infos := map[string][]dun.SessionInfo{} // per workspace, read once
	for _, s := range list {
		m.sessLive[s.ID] = s.Running
		if have[s.ID] || s.Workspace == m.workspace {
			continue
		}
		m.sessWS[s.ID] = s.Workspace
		if _, ok := infos[s.Workspace]; !ok {
			infos[s.Workspace] = dun.ListSessionInfo(s.Workspace)
		}
		info := dun.SessionInfo{ID: s.ID}
		for _, in := range infos[s.Workspace] {
			if in.ID == s.ID {
				info = in
			}
		}
		m.sessions = append(m.sessions, info)
	}
}

// updatePicking owns the keys while the session list is open.
func (m tuiModel) updatePicking(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	m.leaveSession()
	m.sessionID = id
	m.skipHistory = false // this one we DO want replayed — it is a new transcript
	if ws := m.sessWS[id]; ws != "" {
		// A session dund runs in another workspace: follow it there.
		m.workspace, m.opts.workspace = ws, ws
		m.append(stDim.Render("workspace " + ws))
	}
	m.append(stDim.Render("resuming session " + id + "…"))
	m.refresh()

	o := m.opts
	o.resume, o.cont = id, false
	return restartEngine(o, id, true, resumeAt{}) // another session: from its start
}

// leaveSession detaches from the session on screen and clears what belongs
// to it — scrollback, the turn, its suggestions — before another is joined.
func (m *tuiModel) leaveSession() {
	m.proc.close()
	m.convo = nil
	m.cur, m.pendingTool = "", -1
	m.sel = -1
	m.busy, m.starting = false, true
	m.dropAsk()
	m.convoHold = 0 // the conversation is about to be replaced
	m.startingStart = time.Now()
	m.fatalErr, m.exitAnnounced = "", false
	m.restarts, m.restartStart = 0, time.Now()
	m.suggestions, m.suggestSel = nil, 0
}

// newSlash is /new: a fresh session in this workspace. The one left keeps
// running for whoever else is in it (or until the idle stop), and a plain
// dun can come back to it.
func newSlash(m *tuiModel, _ []string) tea.Cmd {
	if m.replaying {
		m.append(stDim.Render("this is a replay — there is no engine to start a session with"))
		return nil
	}
	m.leaveSession()
	// No id to reattach to until the new engine names one, and a retry must
	// not fall back to the session just left.
	m.sessionID, m.everUp = "", false
	m.skipHistory = false
	m.opts.resume, m.opts.cont, m.opts.fresh = "", false, true
	m.append(stDim.Render("starting a fresh session…"))
	m.refresh()
	return restartEngine(m.opts, "", false, resumeAt{})
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
			mark, s.ID, stDim.Render(relTime(s.ModTime)), stDim.Render(commaf("%d entries", s.Entries)))
		if m.sessLive[s.ID] {
			line += stTool.Render("  running")
		}
		if ws := m.sessWS[s.ID]; ws != "" {
			line += stDim.Render("  " + ws)
		}
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
