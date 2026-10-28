package main

import (
	"regexp"
	"strconv"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Messages between agents: a box like a thought's (box.go), with who sent it
// to whom on top, what was said under it, and — for one this session sent —
// what became of it, dim, at the foot.
//
//	✉ agent #2 → you
//	│ The tests pass on **linux**; the mac
//	│ runner is down, so I skipped it.
//
//	✉ you → dun:api
//	│ The schema moved to v3.
//	  delivered · woke it
//
// Four kinds of message, two directions each way:
//
//   - a sub-agent's tell_parent message, which arrives as a notification
//     ("agent #2: …") — agent #2 → you;
//   - this session's jobs(agent:N, tell:"…") — you → agent #N;
//   - this session's dun_msg(to, text) — you → dun:<name>;
//   - another session's dun_msg, a "peer" notification — dun:<name> → you.
//
// The tool call that carried a message IS its box: it is not also a line in
// the run of calls. A sub-agent's report (its status, its answer) is not a
// message; it stays a notification line and a row in the activity strip.
//
// A long message folds the way any box does: capped, scrolled inside, enter
// opens it in the pager.

type agentMsg struct {
	from, to string
	text     string
	// carried: a tool call of this session sent it, so it has a result to
	// wait for and a footer to say it in.
	carried bool
	tool    string
	args    map[string]any
	result  string // the carrying call's result, verbatim (the pager shows it)
	done    bool
	failed  bool
	status  string // the footer: the result's short status

	// The text rendered as markdown at rendW columns.
	rendW    int
	rendered string
}

// msgHeader is the box's top line: ✉ from → to.
func (a *agentMsg) header() string {
	return stSecondary.Render("✉ ") + stMsgName.Render(a.from) + stDim.Render(" → ") + stMsgName.Render(a.to)
}

// footer is the carrying call's status, or "" for a message that came in.
func (a *agentMsg) footer() string {
	switch {
	case !a.carried:
		return ""
	case !a.done:
		return stDim.Render("  sending…")
	case a.failed:
		return stErr.Render("  ✗ " + a.status)
	}
	return stDim.Render("  " + a.status)
}

// plain is the box as text, for search and the screen dump.
func (a *agentMsg) plain() string {
	s := stripANSI(a.header()) + "\n" + strings.TrimSpace(a.text)
	if f := a.footer(); f != "" {
		s += "\n" + strings.TrimSpace(stripANSI(f))
	}
	return s
}

// body is the text as markdown, wrapped to ww columns, without glamour's
// margins: the box's edge is the margin.
func (a *agentMsg) body(ww int) string {
	if a.rendW == ww && a.rendered != "" {
		return a.rendered
	}
	out := renderMarkdown(msgMarkdown(ww), strings.TrimSpace(a.text))
	rows := strings.Split(out, "\n")
	for i, r := range rows {
		rows[i] = ansi.Truncate(ansi.TruncateLeft(r, msgMDMargin, ""), ww, "")
	}
	// A list or a code block opens with a row of only padding.
	for len(rows) > 1 && strings.TrimSpace(stripANSI(rows[0])) == "" {
		rows = rows[1:]
	}
	a.rendW, a.rendered = ww, strings.Join(rows, "\n")
	return a.rendered
}

// stMsgName is a sender or a receiver: bold, in the text's own colour — not
// the accent, which is for where you are.
var stMsgName = lipgloss.NewStyle().Bold(true)

// msgMDMargin is the left margin glamour's standard styles put on a document.
const msgMDMargin = 2

var (
	msgMDMu sync.Mutex
	msgMDs  = map[int]*glamour.TermRenderer{}
)

// msgMarkdown is a renderer whose text is ww columns wide once its margins
// are cut, made once per width.
func msgMarkdown(ww int) *glamour.TermRenderer {
	msgMDMu.Lock()
	defer msgMDMu.Unlock()
	if r, ok := msgMDs[ww]; ok {
		return r
	}
	r := newMarkdown(ww + 2*msgMDMargin)
	msgMDs[ww] = r
	return r
}

// msgEntry is a block for a.
func msgEntry(a *agentMsg) convoEntry {
	e := convoEntry{msg: a, box: &boxState{}}
	e.collapsed = a.plain()
	return e
}

// touchMsg redraws block i after its message changed.
func (m *tuiModel) touchMsg(i int) {
	e := &m.convo[i]
	e.collapsed = e.msg.plain()
	e.invalidateWrap()
}

// ── which calls carry a message ────────────────────────────────────

// carriedMsg is the message a tool call carries, or nil: a jobs tell to a
// sub-agent, or a dun_msg to another session.
func carriedMsg(tool string, args map[string]any) *agentMsg {
	switch tool {
	case "jobs":
		tell := argStr(args, "tell")
		n := agentArg(args["agent"])
		if tell == "" || n == "" || args["kill"] == true || args["resume"] == true {
			return nil
		}
		return &agentMsg{from: "you", to: "agent #" + n, text: tell, carried: true, tool: tool, args: args}
	case "dun_msg":
		to, text := argStr(args, "to"), argStr(args, "text")
		if to == "" || text == "" {
			return nil
		}
		return &agentMsg{from: "you", to: "dun:" + to, text: text, carried: true, tool: tool, args: args}
	}
	return nil
}

func agentArg(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case int:
		return strconv.Itoa(n)
	case string:
		return strings.TrimSpace(n)
	}
	return ""
}

var dunSentRe = regexp.MustCompile(`^delivered to (.+?) \(([^()]*)\): (.*)$`)

// setResult reads the carrying call's result into the footer.
func (a *agentMsg) setResult(result string) {
	a.result, a.done = result, true
	first := strings.TrimSpace(firstLine(strings.TrimSpace(result)))
	if strings.HasPrefix(first, "ERROR") {
		a.failed, a.status = true, clip(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(first, "ERROR"), ":")), 120)
		return
	}
	switch a.tool {
	case "jobs":
		if strings.HasPrefix(first, "Told agent #") {
			a.status = "sent · it is working"
			return
		}
		// Refused: still running, or stopped. Its first sentence says which.
		a.failed, a.status = true, clip(first, 120)
	case "dun_msg":
		mm := dunSentRe.FindStringSubmatch(first)
		if mm == nil {
			a.status = clip(first, 120)
			return
		}
		a.to = "dun:" + mm[1] // the name, where the call may have used the id
		switch {
		case strings.Contains(mm[3], "busy"):
			a.status = "delivered · it is busy; it reads it this turn"
		case strings.Contains(mm[3], "turn started"):
			a.status = "delivered · woke it"
		default:
			a.status = "delivered · not woken; it reads it next turn"
		}
	}
}

// ── which notifications are messages ───────────────────────────────

var (
	childMsgRe = regexp.MustCompile(`(?s)^agent #(\d+): (.+)$`)
	// A peer notification as the TUI gets it live: "<name> (<id>): <text>".
	peerLiveRe = regexp.MustCompile(`(?s)^(.+?) \(([^()]*)\): (.*)$`)
	// The same message as the session log keeps it (dun.PeerLabel).
	peerLogRe = regexp.MustCompile(`(?s)^\[dun msg from (.+?) \(session ([^()]*)\)\] (.*?)(?:\n\(A message from another dun session.*)?$`)
)

// noticeMsg is the message a notification carries, or nil. kind is the
// event's kind ("peer"), "" for a plain notification and in a replay.
func noticeMsg(kind, text string) *agentMsg {
	if kind == "peer" {
		if mm := peerLiveRe.FindStringSubmatch(text); mm != nil {
			return &agentMsg{from: "dun:" + mm[1], to: "you", text: mm[3]}
		}
		return nil
	}
	if mm := childMsgRe.FindStringSubmatch(text); mm != nil {
		return &agentMsg{from: "agent #" + mm[1], to: "you", text: mm[2]}
	}
	if mm := peerLogRe.FindStringSubmatch(text); mm != nil {
		return &agentMsg{from: "dun:" + mm[1], to: "you", text: mm[3]}
	}
	return nil
}

// addMsg puts a message in the conversation, after anything streamed: its
// own block, so the run of calls before it is over.
func (m *tuiModel) addMsg(a *agentMsg) int {
	m.endRun()
	m.convo = append(m.convo, msgEntry(a))
	return len(m.convo) - 1
}
