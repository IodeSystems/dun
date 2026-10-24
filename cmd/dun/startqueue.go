package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iodesystems/dun/proto"
)

// Before the session is ready. The TUI draws its first frame before it has
// a session (runTUI attaches from Init), and a new engine can take a while
// to come up. Until it is ready:
//
//   - the status line says what it waits for, with the time it has waited:
//     the session (dund creating it), then the engine;
//   - a message typed is kept, shown dim in the conversation, and sent when
//     the engine's settings arrive (the event after ready, which says whether
//     there is anything to send to — if not, it is held for /login like any
//     other message typed before a login);
//   - ctrl+c quits, as always.
//
// After ready, a server still starting (poly-lsp indexing a big repo) is in
// the splash ("starting…") and after "ready" on the status line, with its
// elapsed time; its tools join when it is up.

// keptMsg is a message typed before ready.
type keptMsg struct {
	text string
	imgs []proto.Image
}

// keptLine is what a kept message says under it until it is sent.
const keptLine = " (sent when the session is ready)"

// keepUntilReady keeps a message typed before ready.
func (m tuiModel) keepUntilReady(v string, imgs []proto.Image) tuiModel {
	m.history = append(m.history, v)
	m.histIdx = len(m.history)
	m.input.Reset()
	m.suggestions = nil
	m.startQueue = append(m.startQueue, keptMsg{text: v, imgs: imgs})
	m.convo = append(m.convo, convoEntry{collapsed: stDim.Render("› " + v + keptLine), startKept: true})
	m.scrollPinned = true
	m.refresh()
	return m
}

// sendKept sends what was kept, in the order it was typed: the first starts
// a turn, the rest ride it as messages typed mid-turn do. Without a login it
// is held for after /login instead (onboarding.go).
func (m tuiModel) sendKept() tuiModel {
	if len(m.startQueue) == 0 {
		return m
	}
	q := m.startQueue
	m.startQueue = nil
	m.convo = slices.DeleteFunc(m.convo, func(e convoEntry) bool { return e.startKept })
	if m.sel >= len(m.convo) {
		m.sel = -1
	}
	if !m.loggedIn() {
		texts := make([]string, len(q))
		for i, k := range q {
			texts[i] = k.text
		}
		m.holdUnsent(strings.Join(texts, "\n\n"))
		return m
	}
	for _, k := range q {
		if !m.proc.send(k.text, k.imgs) {
			m.append(stErr.Render("no engine right now — not sent: " + oneLine(k.text)))
			continue
		}
		m.busy = true
	}
	m.scrollPinned = true
	m.refresh()
	return m
}

// startQueuedHint is the status line's count of kept messages, or "".
func (m tuiModel) startQueuedHint() string {
	if n := len(m.startQueue); n > 0 {
		return " · " + plural(n, "message") + " kept"
	}
	return ""
}

// startingNamesAfter is how long "starting session…" may spin before it
// names what it waits for.
const startingNamesAfter = 3 * time.Second

// startingWhat is what the session waits for before it is ready. Past
// startingNamesAfter, the attach says what it is waiting on (attachNow):
// dund, or the session — and when dund is another build, that build and
// the command that replaces it, since an older dund's engine names its
// session only once its slowest server is up.
func (m tuiModel) startingWhat() string {
	if m.proc == nil {
		w := attachNow.Load()
		if w == nil || m.startingStart.IsZero() || time.Since(m.startingStart) < startingNamesAfter {
			return "starting session…"
		}
		switch {
		case !w.session:
			return "waiting for dund, the session service…"
		case w.dundBuild != "":
			// Narrow, the words go before the fix does.
			old := "an older build (" + w.dundBuild + ")"
			if w.dundBuild == "?" {
				old = "an older build"
			}
			return firstFit(m.w-12, "waiting for the session · dund is "+old+" — "+w.fix,
				"dund is "+old+" — "+w.fix, "old dund — "+w.fix)
		}
		return "waiting for the session · dund is starting its engine…"
	}
	if names, _ := m.serversStarting(); names != "" {
		return "waiting for " + names + "…"
	}
	return "starting the engine…"
}

// serversStartingTail is " · poly-lsp starting 34s" after "ready" while a
// server is still starting, or "".
func (m tuiModel) serversStartingTail() string {
	names, since := m.serversStarting()
	if names == "" {
		return ""
	}
	return fmt.Sprintf(" · %s starting %s", names, time.Since(since).Round(time.Second))
}

// serversStarting names the servers still starting, and when the first of
// them began.
func (m tuiModel) serversStarting() (string, time.Time) {
	var ids []string
	var first time.Time
	for _, st := range sortedServers(m.servers) {
		if !st.Starting {
			continue
		}
		ids = append(ids, st.ID)
		if t, ok := m.srvSince[st.ID]; ok && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	if first.IsZero() {
		first = time.Now()
	}
	return strings.Join(ids, ", "), first
}

// noteStarting records when each server still starting began, on this
// clock: the engine says how long it has been at it, and the TUI counts on
// from there. A new map, not an edit: model copies share the old one.
func (m *tuiModel) noteStarting(list []any) {
	next := map[string]time.Time{}
	for _, s := range list {
		o, ok := s.(map[string]any)
		if !ok || o["starting"] != true {
			continue
		}
		id := str(o["id"])
		if t, ok := m.srvSince[id]; ok {
			next[id] = t
			continue
		}
		next[id] = time.Now().Add(-time.Duration(evNum(o["startingSeconds"]) * float64(time.Second)))
	}
	m.srvSince = next
}

// splashSays reports whether a server event only says what the live splash
// now shows: a server that came up in the background. A failure is still
// said in the conversation, with its reason.
func (m tuiModel) splashSays(ev evMsg) bool {
	if ev["background"] != true || len(m.convo) == 0 || !m.convo[0].liveSplash {
		return false
	}
	return !strings.Contains(str(ev["message"]), "did not start")
}
