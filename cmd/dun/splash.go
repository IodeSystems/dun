package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/iodesystems/dun"
)

// The splash: what this session is — the build, the workspace, the model,
// the tools and the servers — said once, at the top of a fresh session's
// conversation, and on demand by /status. It is where the details the
// header row and the startup lines used to carry live now, so the header can
// be the folder and the model only.
//
// It is a client-side entry like every line the TUI writes itself (m.append):
// the engine never sees it, so it is not in the session log and never sent
// to the model, and a resume does not replay it. A fresh session shows it; a
// resumed one does not (its history is the top of the screen).
//
// Sections are rows of label + value (statusSections); /status can carry
// more of them later without a new layout.

// statusSection is one row of the splash: a label in the label column and
// its value, already styled.
type statusSection struct {
	label, value string
}

// statusSections are the splash's rows, in order.
func (m tuiModel) statusSections() []statusSection {
	var out []statusSection
	out = append(out, statusSection{"workspace", tildePath(m.workspace)})
	model := stDim.Render("none yet — /login, then /model")
	if m.model != "" && m.url != "" {
		model = m.model
		if m.provider != "" {
			model += stDim.Render(" on ") + m.provider
		}
	} else if m.url != "" {
		model = stDim.Render("none yet — /model")
	}
	out = append(out, statusSection{"model", model})
	if n := len(m.tools); n > 0 {
		out = append(out, statusSection{"tools", fmt.Sprintf("%d", n)})
	}
	running, failed, off, offOn, other := splashServers(m.servers)
	if len(running)+len(failed)+len(other) > 0 {
		out = append(out, statusSection{"servers", strings.Join(slices.Concat(running, failed, other), stDim.Render(" · "))})
	}
	if len(off) > 0 {
		out = append(out, statusSection{"off", strings.Join(off, ", ") + stDim.Render(" — "+strings.Join(offOn, ", ")+" (auto: every session)")})
	}
	return out
}

// splashServers sorts the servers into what the splash says of them:
// running (with its tools), failed (with why), off with a switch (the
// optional extras, and their commands), and off with none.
func splashServers(states []dun.ServerState) (running, failed, off, offOn, other []string) {
	sorted := slices.Clone(states)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].ID < sorted[b].ID })
	for _, st := range sorted {
		switch {
		case st.Running:
			s := st.ID + " " + stOK.Render("running")
			if st.Tools > 0 {
				s += stDim.Render(" " + plural(st.Tools, "tool"))
			}
			running = append(running, s)
		case st.Err != "":
			failed = append(failed, st.ID+" "+stErr.Render("did not start"))
		case st.Slash != "":
			off = append(off, st.ID)
			offOn = append(offOn, "/"+st.Slash+" on")
		default:
			other = append(other, st.ID+stDim.Render(" off"))
		}
	}
	return
}

// splashView is the splash as one block: the wordmark and the build, then a
// label column (dim) and a value column, then a blank row that sets it apart
// from what follows.
func (m tuiModel) splashView() string {
	secs := m.statusSections()
	w := 0
	for _, s := range secs {
		w = max(w, len(s.label))
	}
	rows := []string{stBrand.Render("dun") + stDim.Render(" "+version)}
	for _, s := range secs {
		rows = append(rows, "  "+stDim.Render(fmt.Sprintf("%-*s", w, s.label))+"  "+s.value)
	}
	return strings.Join(rows, "\n") + "\n"
}

// showSplash puts the splash at the top of the conversation: a fresh
// session's first entry.
func (m *tuiModel) showSplash() {
	m.convo = slices.Insert(m.convo, 0, convoEntry{collapsed: m.splashView(), splash: true})
	m.refresh()
}

// statusSlash is /status: the splash, now, at the end of the conversation.
func (m *tuiModel) statusSlash() {
	m.convo = append(m.convo, convoEntry{collapsed: m.splashView(), splash: true})
	m.refresh()
}
