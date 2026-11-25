package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/iodesystems/dun/internal/sesslist"
	"github.com/iodesystems/dun/internal/textfmt"
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
// (which also works, for scripts and for when you know the id). Under it is
// the latest ask, when it differs: with many sessions open, the end says
// what each became.

// resumeSlash implements /resume: bare opens the picker, an argument switches
// straight to that session.
func resumeSlash(m *tuiModel, args []string) tea.Cmd {
	if m.replaying {
		m.append(StDim.Render("this is a replay — there is no engine to resume with"))
		return nil
	}
	if len(args) > 0 {
		return m.switchSession(args[0])
	}
	m.sessions, _, _ = sesslist.Collect(sesslist.Opts{WS: m.workspace, All: true, N: pickerListN, Dund: dundSessions})
	m.sessWS = map[string]string{}
	for _, s := range m.sessions {
		if !s.Local {
			m.sessWS[s.ID] = s.Workspace
		}
	}
	if len(m.sessions) == 0 {
		m.append(StDim.Render("no saved sessions yet"))
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
func (m tuiModel) updatePicking(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c", "q":
		m.picking = false
		m.input.Focus()
		m.refresh()
		if m.pickAtStart { // --pick dismissed: a new session in the workspace given
			m.pickAtStart = false
			return m, restartEngine(m.opts, "", false, resumeAt{})
		}
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
		m.picking, m.pickAtStart = false, false
		m.input.Focus()
		if id == "" && m.proc == nil {
			return m, restartEngine(m.opts, "", false, resumeAt{})
		}
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
	// The session on screen is attached again from its start: what shows is
	// rebuilt from dund's replay, as when it is opened fresh — the way back
	// when the screen lost its conversation (USER 2026-11-19, after an
	// upgrade). It used to be refused ("already in session").
	same := id == m.sessionID
	m.leaveSession()
	m.sessionID = id
	m.skipHistory = false // this one we DO want replayed — it is a new transcript
	if ws := m.sessWS[id]; ws != "" && !same {
		// A session dund runs in another workspace: follow it there.
		m.workspace, m.opts.Workspace = ws, ws
		m.append(StDim.Render("workspace " + ws))
	}
	if same {
		m.append(StDim.Render("reloading session " + id + "…"))
	} else {
		m.append(StDim.Render("resuming session " + id + "…"))
	}
	m.refresh()

	o := m.opts
	o.Resume, o.Cont = id, false
	return restartEngine(o, id, true, resumeAt{}) // another session: from its start
}

// leaveSession detaches from the session on screen and clears what belongs
// to it — scrollback, the turn, its suggestions — before another is joined.
func (m *tuiModel) leaveSession() {
	m.proc.close()
	m.convo = nil
	m.cur = ""
	m.sel = -1
	m.busy, m.starting = false, true
	m.dropAsk()
	m.dropHold() // the conversation is about to be replaced
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
		m.append(StDim.Render("this is a replay — there is no engine to start a session with"))
		return nil
	}
	m.leaveSession()
	// No id to reattach to until the new engine names one, and a retry must
	// not fall back to the session just left.
	m.sessionID, m.everUp = "", false
	m.skipHistory = false
	m.splashDone = false // a fresh session opens with its splash
	m.opts.Resume, m.opts.Cont, m.opts.Fresh = "", false, true
	m.append(StDim.Render("starting a fresh session…"))
	m.refresh()
	return restartEngine(m.opts, "", false, resumeAt{})
}

// pickerListN bounds the rows /resume lists: the files behind the rest are
// never opened, which is what keeps the picker quick.
const pickerListN = 60

// sessionPanel renders the picker in the lower pane: this workspace's
// sessions first, then the others under a divider, one line each.
func (m tuiModel) sessionPanel() string {
	rows := make([]string, 0, len(m.sessions)+2)
	rows = append(rows, stHeader.Render("sessions")+StDim.Render("  ↑/↓ choose · enter resume · esc cancel"))
	window := 12
	if m.h > 0 {
		window = min(12, max(3, m.h-10))
	}
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
	now := time.Now()
	for i := lo; i < hi; i++ {
		s := m.sessions[i]
		switch {
		case i == 0 && s.Local:
			rows = append(rows, StDim.Render("  this workspace"))
		case !s.Local && (i == 0 || m.sessions[i-1].Local):
			rows = append(rows, StDim.Render("  other workspaces"))
		}
		mark := "  "
		if s.ID == m.sessionID {
			mark = StTool.Render("• ") // where you are now
		}
		status := textfmt.Pad(s.Status, 7)
		if s.Status == "running" {
			status = StTool.Render(status)
		} else {
			status = StDim.Render(status)
		}
		line := mark + s.ID + "  " + status + "  " + StDim.Render(textfmt.Pad(sesslist.Age(s.LastActive, now), 8)) + "  "
		used := 2 + len(s.ID) + 2 + 7 + 2 + 8 + 2
		if !s.Local {
			const dirW = 24
			line += StDim.Render(textfmt.Pad(sesslist.ClipLeft(sesslist.TildeDir(s.Workspace), dirW), dirW)) + "  "
			used += dirW + 2
		}
		ask := textfmt.OneLine(s.Last)
		if ask == "" {
			ask = "(no request)"
		}
		line += textfmt.ClipRune(ask, max(10, m.w-used-5))
		if i == m.pickSel {
			rows = append(rows, addGutter(line, "▎ ", stSel))
		} else {
			rows = append(rows, addGutter(line, "  ", lipgloss.NewStyle()))
		}
	}
	if lo > 0 || hi < len(m.sessions) {
		rows = append(rows, StDim.Render(fmt.Sprintf("     %d-%d of %d", lo+1, hi, len(m.sessions))))
	}
	return strings.Join(rows, "\n")
}
