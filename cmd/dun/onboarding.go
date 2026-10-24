package main

import (
	"regexp"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/cellbuf"
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

// appendNotice puts a status notice in the conversation with the one
// spacing every notice has: a blank row above it and a blank row below it
// (the turn-gap row, so a turn ending next does not add a second). It
// returns the notice's index.
func (m *tuiModel) appendNotice(e convoEntry) int {
	if n := len(m.convo); n > 0 && !m.convo[n-1].blank && !m.convo[n-1].splash {
		m.turnGap() // the splash ends in a blank row of its own
	}
	e.hang = true // a notice that wraps continues under its words, not its ✓
	m.convo = append(m.convo, e)
	i := len(m.convo) - 1
	m.turnGap()
	m.refresh()
	return i
}

// heldLine is said when a question is held for want of a login: what to
// type, then what happens.
const heldLine = "kept your question — type /login, then it comes back"

// needLoginLine is the first-run line: what dun needs, and the command.
// Short enough for a phone's row with its mark (errNoLogin is the engine's
// error, matched by its words, and stays as it is).
const needLoginLine = "not logged in — type /login to connect"

// hangWrap wraps s at w with every row after the first at column 2.
func hangWrap(s string, w int) string {
	rows := strings.Split(cellbuf.Wrap(s, max(1, w), ""), "\n")
	if len(rows) == 1 {
		return s
	}
	rest := strings.Split(cellbuf.Wrap(strings.Join(rows[1:], " "), max(1, w-2), ""), "\n")
	for i, r := range rest {
		r = strings.TrimLeft(r, " ")
		// A row does not start with the separator of a notice's details.
		if strings.HasPrefix(ansi.Strip(r), "· ") {
			r = ansi.Cut(r, 2, ansi.StringWidth(r))
		}
		rest[i] = "  " + r
	}
	return strings.Join(append(rows[:1], rest...), "\n")
}

// warnLine is a line that needs the person, in the one shape: the warning
// mark in column 0 (as ✓ is on a line that worked: the state is not colour
// alone), the text from column 2, in the attention colour. ⚠ is in DejaVu
// Sans Mono and in dun's other warnings (/perf, the status line's spend).
func warnLine(text string) string { return stNote.Render("⚠ " + text) }

// loggedIn reports whether the session has an AI service to send to, as
// far as the engine has said (its settings event); until it has, yes.
func (m *tuiModel) loggedIn() bool { return !m.noLogin }

// sayNeedLogin puts the "not logged in" line at the end of the conversation,
// and takes away the one said before: it is said once, where it is needed
// now. Nothing else moves while no turn can have run.
func (m *tuiModel) sayNeedLogin() {
	m.dropNeedLogin()
	m.convo = append(m.convo, convoEntry{collapsed: warnLine(needLoginLine), needLogin: true, hang: true})
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
	m.sayHeld()
}

// sayHeld puts the kept question's line at the end of the conversation, in
// place of the not-logged-in line. It is an instruction — what to type
// next — so it has the warning's shape and colour (run 5: in the secondary
// grey it was the quietest line on the screen). The login form takes it
// away when it opens; esc puts it back.
func (m *tuiModel) sayHeld() {
	m.dropNeedLogin()
	m.convo = append(m.convo, convoEntry{collapsed: warnLine(heldLine), needLogin: true, hang: true})
	m.refresh()
}

// noteServerMsg puts a server event's message in the conversation as a
// notice: a line that says a server runs in the success colour, a failure in
// the attention colour, the rest plain (replyLine). The result of /mcp add
// replaces its "adding …" line, in place.
func (m *tuiModel) noteServerMsg(action, msg string) {
	lines := strings.Split(msg, "\n")
	for i, l := range lines {
		if strings.Contains(l, " · running") {
			lines[i] = stOK.Render("✓ " + l)
		} else {
			lines[i] = replyLine(l)
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
	m.appendNotice(convoEntry{collapsed: out})
}

// failWords mark a reply that needs attention: it failed, or was refused.
var failWords = regexp.MustCompile(`(?i)\b(error|errors|failed|fails|did not|could not|cannot|can't|refused|unknown|no such|not found|not logged in)\b|usage:`)

// replyLine styles a line the engine said back: the attention colour only
// when it says something failed or is needed; plain otherwise. Orange means
// "blocked, or needs you" — nothing else.
func replyLine(l string) string {
	if failWords.MatchString(l) {
		return stNote.Render(l)
	}
	return l
}

// afterSwitch runs at the ready that follows a model pick: the pick is
// confirmed, and a held message is put back in an empty composer.
func (m *tuiModel) afterSwitch() {
	if m.switchNote != "" {
		m.appendNotice(convoEntry{collapsed: okNotice(m.switchNote)})
		m.switchNote = ""
	}
	if m.heldMsg != "" && m.loggedIn() && m.input.Value() == "" {
		m.input.SetValue(m.heldMsg)
		m.restoredMsg, m.heldMsg = m.heldMsg, ""
	}
}
