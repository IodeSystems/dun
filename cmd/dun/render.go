package main

import (
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// Rendering helpers for the TUI: markdown (glamour) for assistant replies, and
// diff colorization for tool output that looks like a unified diff.

// newMarkdown builds a glamour renderer word-wrapped to width (auto light/dark).
// Returns nil on failure — callers fall back to raw text.
func newMarkdown(width int) *glamour.TermRenderer {
	if width < 20 {
		width = 20
	}
	r, err := glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(width))
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
