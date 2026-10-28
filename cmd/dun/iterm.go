package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// iTerm2 tabs (owner, 2026-10-27): the same attention states as the tmux
// window (tmux.go), shown in the tab's title and colour.
//
//   - title (OSC 1): "<icon> dun · <workspace>", no icon when idle. The
//     title before dun is pushed on the xterm title stack (CSI 22;1t) before
//     the first change and popped (CSI 23;1t) at exit.
//   - colour (iTerm2's OSC 6): amber working, pink question, green done,
//     reset when idle and at exit. Muted, so the tab text stays readable.
//   - a question also asks for attention (OSC 1337 RequestAttention), which
//     bounces the dock icon when iTerm2 is in the background.
//
// Experimental and off by default (owner, 2026-10-27): only with
// --exp-iterm-tabcontrol, and then only in iTerm2: TERM_PROGRAM=iTerm.app,
// or LC_TERMINAL=iTerm2 (which survives ssh and tmux). DUN_BELL=off turns
// off the attention request with the bell. Inside tmux every escape
// is wrapped for passthrough, which needs `set -g allow-passthrough on`;
// without it tmux drops them and the @dun_icon path is what shows.

const (
	esc = "\x1b"
	bel = "\a"
)

// itermTab writes the escapes for one TUI's tab. nil outside iTerm2.
// Everything it returns goes out through Bubble Tea (tea.Raw) while the
// program runs; restore is written after the program has ended.
type itermTab struct {
	title  string // "dun · <workspace>"
	wrap   bool   // inside tmux: wrap each escape for passthrough
	pushed bool   // the title stack holds the title from before dun
}

// itermDetect reports whether the environment is iTerm2.
func itermDetect(getenv func(string) string) bool {
	return getenv("TERM_PROGRAM") == "iTerm.app" || getenv("LC_TERMINAL") == "iTerm2"
}

// newItermTab is nil unless on (--exp-iterm-tabcontrol) and in iTerm2.
func newItermTab(on bool, getenv func(string) string, workspace string) *itermTab {
	if !on || !itermDetect(getenv) {
		return nil
	}
	if abs, err := filepath.Abs(workspace); err == nil {
		workspace = abs
	}
	return &itermTab{
		title: "dun · " + filepath.Base(workspace),
		wrap:  getenv("TMUX") != "",
	}
}

// tmuxWrap wraps an escape for tmux passthrough: DCS tmux; with each ESC
// doubled, ended by ST.
func tmuxWrap(s string) string {
	return esc + "Ptmux;" + strings.ReplaceAll(s, esc, esc+esc) + esc + `\`
}

func (t *itermTab) out(s string) string {
	if t.wrap {
		return tmuxWrap(s)
	}
	return s
}

// itermColors are the tab colours per state, as r, g, b.
var itermColors = map[string][3]int{
	attnWorking:  {200, 150, 60},  // amber
	attnQuestion: {220, 110, 160}, // dun's pink accent, muted
	attnDone:     {90, 170, 100},  // green
}

// tabColor is OSC 6 for one state; idle resets it.
func tabColor(state string) string {
	c, ok := itermColors[state]
	if !ok {
		return esc + "]6;1;bg;*;default" + bel
	}
	var b strings.Builder
	for i, ch := range []string{"red", "green", "blue"} {
		fmt.Fprintf(&b, "%s]6;1;bg;%s;brightness;%d%s", esc, ch, c[i], bel)
	}
	return b.String()
}

// show is the escapes for a new state: push the title once, set title and
// colour, and on a question ask for attention when ring is set.
func (t *itermTab) show(state string, ring bool) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	if !t.pushed {
		t.pushed = true
		b.WriteString(t.out(esc + "[22;1t"))
	}
	title := t.title
	if icon := attnIcon(state); icon != "" {
		title = icon + " " + title
	}
	b.WriteString(t.out(esc + "]1;" + title + bel))
	b.WriteString(t.out(tabColor(state)))
	if state == attnQuestion && ring {
		b.WriteString(t.out(esc + "]1337;RequestAttention=yes" + bel))
	}
	return b.String()
}

// restore is the escapes for exit: colour reset and, when dun changed the
// title, the old title popped back.
func (t *itermTab) restore() string {
	if t == nil || !t.pushed {
		return ""
	}
	t.pushed = false
	return t.out(tabColor(attnIdle)) + t.out(esc+"[23;1t")
}
