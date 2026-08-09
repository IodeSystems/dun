package main

import (
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Rendering helpers for the TUI: markdown (glamour) for assistant replies, and
// diff colorization for tool output that looks like a unified diff.

// mdStyle is the resolved light/dark style, decided ONCE.
//
// glamour.WithAutoStyle() asks the terminal for its background colour with an
// OSC escape and waits up to FIVE SECONDS for a reply. Plenty of terminals
// never answer — tmux without passthrough among them — and dun rebuilt the
// renderer on every tea.WindowSizeMsg, inside Update. Measured: a single
// WindowSizeMsg taking 5.0052s while every keystroke queued behind it. That is
// the "spinning so hard it missed input while tmux was fine" bug: not a spin at
// all, a blocking terminal query on the input goroutine, once per resize.
//
// Resolved once, off the message loop (initMarkdownStyle, before the program
// starts), and overridable so the query can be skipped entirely.
var (
	mdStyleOnce sync.Once
	mdStyle     string
)

// initMarkdownStyle resolves the style. Call it BEFORE the Bubble Tea loop
// starts: the query writes to the tty and reads the reply, so doing it while
// the key reader is running would race for the same bytes — and doing it inside
// Update blocks input for as long as the terminal stays silent.
func initMarkdownStyle() {
	mdStyleOnce.Do(func() {
		switch strings.ToLower(strings.TrimSpace(os.Getenv("DUN_MD_STYLE"))) {
		case "dark":
			mdStyle = "dark"
			return
		case "light":
			mdStyle = "light"
			return
		case "notty", "none":
			mdStyle = "notty"
			return
		}
		// COLORFGBG is free when the terminal sets it — no round trip.
		if fg, bg, ok := parseColorFGBG(os.Getenv("COLORFGBG")); ok {
			_ = fg
			mdStyle = "dark"
			if bg >= 7 {
				mdStyle = "light"
			}
			return
		}
		// The OSC background query is OPT-IN, because when the terminal does not
		// answer it costs a FIVE SECOND stall and there is no way to know in
		// advance which terminals answer. Measured: 10.6s to reach `ready`
		// under a silent terminal versus 5.6s with the query skipped, and it
		// was still 5s of a replay harness that has no business asking anything.
		//
		// It only ever helps light-terminal users who do not export COLORFGBG,
		// and the cost of guessing wrong for them is a slightly-off palette —
		// against five seconds of everyone else's startup. DUN_MD_STYLE=light
		// fixes it permanently for them; DUN_MD_QUERY=1 asks the terminal.
		if os.Getenv("DUN_MD_QUERY") == "1" && termenv.HasDarkBackground() {
			mdStyle = "dark"
			return
		}
		if os.Getenv("DUN_MD_QUERY") == "1" {
			mdStyle = "light"
			return
		}
		mdStyle = "dark"
	})
}

// newMarkdown builds a glamour renderer word-wrapped to width.
// Returns nil on failure — callers fall back to raw text.
func newMarkdown(width int) *glamour.TermRenderer {
	if width < 20 {
		width = 20
	}
	initMarkdownStyle() // no-op after the first call
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(mdStyle),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return nil
	}
	return r
}

// renderMarkdown renders md text through r; falls back to the raw text.
func renderMarkdown(r *glamour.TermRenderer, text string) string {
	if r == nil {
		return text
	}
	out, err := r.Render(text)
	if err != nil {
		return text
	}
	return strings.Trim(out, "\n")
}

var (
	diffAdd = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	diffDel = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	diffHun = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
)

// isDiff reports whether text looks like a unified diff (git or plain).
func isDiff(text string) bool {
	return strings.Contains(text, "@@ ") ||
		strings.Contains(text, "diff --git ") ||
		strings.HasPrefix(strings.TrimSpace(text), "--- ")
}

// colorizeDiff colors +/-/@@ lines of a unified diff.
func colorizeDiff(text string) string {
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "---"):
			// file headers — leave neutral
		case strings.HasPrefix(ln, "+"):
			lines[i] = diffAdd.Render(ln)
		case strings.HasPrefix(ln, "-"):
			lines[i] = diffDel.Render(ln)
		case strings.HasPrefix(ln, "@@"):
			lines[i] = diffHun.Render(ln)
		}
	}
	return strings.Join(lines, "\n")
}

// parseColorFGBG reads the "fg;bg" form many terminals export. The background
// index decides light vs dark: 0-6 and 8 are dark, 7 and 9-15 light-ish.
func parseColorFGBG(v string) (fg, bg int, ok bool) {
	parts := strings.Split(strings.TrimSpace(v), ";")
	if len(parts) < 2 {
		return 0, 0, false
	}
	f, err1 := strconv.Atoi(parts[0])
	b, err2 := strconv.Atoi(parts[len(parts)-1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return f, b, true
}

// maxLineWidth is the widest display line in s, ANSI-aware. refresh() uses it
// to decide whether a block needs wrapping at all: on a resume-sized
// conversation, measuring every block costs 14ms where wrapping every block
// costs 73ms, and a block that already fits does not need to be touched.
func maxLineWidth(s string) int {
	widest, start := 0, 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if w := lipgloss.Width(s[start:i]); w > widest {
				widest = w
			}
			start = i + 1
		}
	}
	return widest
}
