package main

import (
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
)

// The first run: dun with no AI service logged in. The screen's one
// prominent line is what it needs (errNoLogin), in the attention colour; a message
// sent before then is held rather than sent, and comes back in the composer
// once a model is picked — the user sends it, dun does not.
//
// Colour, one meaning each: the accent (212, pink) is where you are — the
// focus, the selection, the active surface; stOK (green) is a thing that
// worked; stNote (orange) is what needs doing; stErr (red) is a failure.

var stOK = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))

// okNotice is a status notice — a thing that worked — in the one shape they
// all share: a mark, what happened, then its details, joined by " · ".
// It starts in column 0 with the mark, apart from the indented answers.
func okNotice(what string, details ...string) string {
	return stOK.Render("✓ " + strings.Join(append([]string{what}, details...), " · "))
}

// heldLine is said when a question is held for want of a login.
const heldLine = "kept your question — it comes back after /login"

// loggedIn reports whether the session has an AI service to send to, as
// far as the engine has said (its settings event); until it has, yes.
func (m *tuiModel) loggedIn() bool { return !m.noLogin }

// sayNeedLogin puts the "not logged in" line at the end of the conversation,
// and takes away the one said before: it is said once, where it is needed
// now. Nothing else moves while no turn can have run.
func (m *tuiModel) sayNeedLogin() {
	m.dropNeedLogin()
	m.convo = append(m.convo, convoEntry{collapsed: stNote.Render(errNoLogin.Error()), needLogin: true})
	m.refresh()
}

// dropNeedLogin takes the "not logged in" line away: said again elsewhere,
// or no longer true (a login).
func (m *tuiModel) dropNeedLogin() {
	if m.busy || m.pendingTool >= 0 {
		return
	}
	m.convo = slices.DeleteFunc(m.convo, func(e convoEntry) bool { return e.needLogin })
	if m.sel >= len(m.convo) {
		m.sel = -1
	}
	m.refresh()
}

// holdUnsent is a message typed before any login: it is shown, not sent —
// there is nothing to send it to — and kept for after /login and /model.
// The line under it says so, in place of the "not logged in" line (which
// names /login too): a person who is told nothing believes it is lost.
func (m *tuiModel) holdUnsent(v string) {
	m.appendUser(v)
	m.heldMsg = v
	m.dropNeedLogin()
	m.convo = append(m.convo, convoEntry{collapsed: stNote.Render(heldLine), needLogin: true})
	m.refresh()
}

// noteServerMsg puts a server event's message in the conversation: a line
// that says a server runs in the success colour, the rest in the notice
// colour. The result of /mcp add replaces its "adding …" line.
func (m *tuiModel) noteServerMsg(action, msg string) {
	lines := strings.Split(msg, "\n")
	for i, l := range lines {
		if strings.Contains(l, " · running") {
			lines[i] = stOK.Render("✓ " + l)
		} else {
			lines[i] = stNote.Render(l)
		}
	}
	out := strings.Join(lines, "\n")
	if action == "add" {
		for i := len(m.convo) - 1; i >= 0; i-- {
			if m.convo[i].adding != "" {
				m.convo[i].collapsed, m.convo[i].adding = out, ""
				m.convo[i].invalidateWrap()
				m.refresh()
				return
			}
		}
	}
	m.append(out)
}

// afterSwitch runs at the ready that follows a model pick: the pick is
// confirmed, and a held message is put back in an empty composer.
func (m *tuiModel) afterSwitch() {
	if m.switchNote != "" {
		m.append(okNotice(m.switchNote))
		m.switchNote = ""
	}
	if m.heldMsg != "" && m.loggedIn() && m.input.Value() == "" {
		m.input.SetValue(m.heldMsg)
		m.heldMsg = ""
	}
}
