package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/iodesystems/dun"
)

// The splash: what a person acts on in this session — the folder, the
// model, the servers, and the optional extras — at the top of a fresh
// session's conversation. /status says everything (the build and the tool
// count too), at the end of the conversation, as of when it was asked.
//
// Both are client-side entries like every line the TUI writes itself
// (m.append): the engine never sees them, so they are not in the session log
// and never sent to the model, and a resume does not replay them. A fresh
// session shows the splash; a resumed one does not (its history is the top of
// the screen).
//
// The splash is LIVE: refresh draws it again from the state as it is now (a
// server added, started or stopped; a model picked), so it never says what is
// no longer true. It is always the same number of rows (splashRows), each cut
// to the width, so a change rewrites a row in place and nothing below it
// moves.

// statusSection is one row of the splash: a label in the label column and
// its value, already styled.
type statusSection struct {
	label, value string
}

// splashRows is how many rows the splash's sections are, whatever they say.
const splashRows = 4

// extraWhat is what an optional extra does, in a word or two, by server id.
var extraWhat = map[string]string{"poly-lsp": "code navigation", "raglit": "project search"}

// splashSections are the splash's rows, in order: always splashRows of them.
func (m tuiModel) splashSections(w int) []statusSection {
	return []statusSection{
		{"folder", tildePath(m.workspace)},
		{"model", m.modelValue()},
		{"servers", m.serversValue()},
		{"extras", m.extrasValue(w - 11)},
	}
}

// statusSections are /status's rows: the splash's, with the build and the
// tool count.
func (m tuiModel) statusSections(w int) []statusSection {
	secs := m.splashSections(w)
	out := []statusSection{{"build", version}, secs[0], secs[1]}
	out = append(out, statusSection{"tools", fmt.Sprintf("%d", len(m.tools))})
	return append(out, secs[2:]...)
}

func (m tuiModel) modelValue() string {
	switch {
	case m.model != "" && m.url != "":
		v := m.model
		if m.provider != "" {
			v += stDim.Render(" on ") + m.provider
		}
		return v
	case m.url != "":
		return stDim.Render("none yet — /model")
	}
	return stDim.Render("none yet")
}

// serversValue is every server that is not an optional extra: running (with
// its tools), failed, or off. The extras have their own row.
func (m tuiModel) serversValue() string {
	running, failed, other := splashServers(m.servers)
	all := slices.Concat(running, failed, other)
	if len(all) == 0 {
		return stDim.Render("none yet")
	}
	return strings.Join(all, stDim.Render(" · "))
}

// splashServers sorts the servers that are not optional extras into what
// the splash says of them: running (with its tools), failed (with why), and
// off. An extra that failed to start is here, as failed.
func splashServers(states []dun.ServerState) (running, failed, other []string) {
	for _, st := range sortedServers(states) {
		switch {
		case st.Slash != "" && st.Err == "":
			continue // an optional extra: the extras row
		case st.Starting:
			other = append(other, st.ID+stDim.Render(" starting…"))
		case st.Running:
			s := st.ID + " " + stOK.Render("running")
			if st.Tools > 0 {
				s += stDim.Render(" · " + plural(st.Tools, "tool"))
			}
			running = append(running, s)
		case st.Err != "":
			failed = append(failed, st.ID+" "+stErr.Render("did not start"))
		default:
			other = append(other, st.ID+stDim.Render(" off"))
		}
	}
	return
}

func sortedServers(states []dun.ServerState) []dun.ServerState {
	sorted := slices.Clone(states)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].ID < sorted[b].ID })
	return sorted
}

// extrasValue is the optional extras in plain words — what each does and
// whether it is on — and where to read more:
//
//	code navigation (poly-lsp) and project search (raglit) are off · /help poly-lsp for more
func (m tuiModel) extrasValue(w int) string {
	var on, off, starting []string
	first := ""
	for _, st := range sortedServers(m.servers) {
		if st.Slash == "" || st.Err != "" {
			continue
		}
		what := st.ID
		if w, ok := extraWhat[st.ID]; ok {
			what = w + " (" + st.ID + ")"
		}
		if st.Starting {
			// The id alone: "starting…" must fit a phone's row, and what
			// it does is said once it is on.
			starting = append(starting, st.ID)
			continue
		}
		if st.Running {
			on = append(on, what)
			continue
		}
		off = append(off, what)
		if first == "" {
			first = st.Slash
		}
	}
	if len(on)+len(off)+len(starting) == 0 {
		return stDim.Render("none")
	}
	say := func(names []string, state string) string {
		verb := " is "
		if len(names) > 1 {
			verb = " are "
		}
		return andList(names) + verb + state
	}
	var parts []string
	if len(off) > 0 {
		parts = append(parts, say(off, "off"))
	}
	if len(starting) > 0 {
		parts = append(parts, say(starting, "starting…"))
	}
	if len(on) > 0 {
		parts = append(parts, say(on, stOK.Render("on")))
	}
	v := strings.Join(parts, stDim.Render(" · "))
	if first != "" {
		// "for more" goes first when the row is narrow: the command stays.
		more := " · /help " + first + " for more"
		if ansi.StringWidth(v+more) > w {
			more = " · /help " + first
		}
		v += stDim.Render(more)
	}
	return v
}

// andList joins names as a sentence does: "a", "a and b", "a, b and c".
func andList(names []string) string {
	if len(names) <= 1 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// sectionsView is rows of label + value — a label column (dim) and a value
// column, each row cut to w cells so it stays one row — then a blank row
// that sets the block apart from what follows.
func sectionsView(secs []statusSection, w int) string {
	lw := 0
	for _, s := range secs {
		lw = max(lw, len(s.label))
	}
	rows := make([]string, 0, len(secs))
	for _, s := range secs {
		rows = append(rows, fitText("  "+stDim.Render(fmt.Sprintf("%-*s", lw, s.label))+"  "+s.value, w))
	}
	return strings.Join(rows, "\n") + "\n"
}

// splashView is the splash as the state is now, at width w.
func (m tuiModel) splashView(w int) string { return sectionsView(m.splashSections(w), w) }

// splashW is the width the splash's rows are cut to: one column less than
// the pane, so the selection gutter never wraps one.
func (m tuiModel) splashW() int { return max(20, m.vp.Width-1) }

// showSplash puts the splash at the top of the conversation: a fresh
// session's first entry, drawn again from live state at every refresh.
func (m *tuiModel) showSplash() {
	m.convo = slices.Insert(m.convo, 0, convoEntry{collapsed: m.splashView(m.splashW()), splash: true, liveSplash: true})
	m.refresh()
}

// liveSplashNow brings the live splash up to date; refresh calls it before
// it draws. A changed row is the same row with new words.
func (m *tuiModel) liveSplashNow() {
	if len(m.convo) == 0 || !m.convo[0].liveSplash {
		return
	}
	if v := m.splashView(m.splashW()); v != m.convo[0].collapsed {
		m.convo[0].collapsed = v
		m.convo[0].invalidateWrap()
	}
}

// statusSlash is /status: everything, as of now, at the end of the
// conversation.
func (m *tuiModel) statusSlash() {
	m.convo = append(m.convo, convoEntry{collapsed: sectionsView(m.statusSections(m.splashW()), m.splashW()), splash: true})
	m.refresh()
}
