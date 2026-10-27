package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// A multi-line command (a heredoc, a script) is shown as highlighted shell
// with its lines as written, through the same glamour renderer as a ```sh
// fence in a reply. A one-line command stays on its line. The model's text is
// not touched: this is display only.

// shellMaxLines caps a command shown in a notice, so one cannot flood the
// conversation.
const shellMaxLines = 12

// shellBlock renders cmd as highlighted shell rows, at most shellMaxLines,
// each cut to w columns (w <= 0: not cut). Blank edge rows are dropped.
func shellBlock(cmd string, w int) []string {
	lines := strings.Split(strings.Trim(strings.ReplaceAll(cmd, "\r\n", "\n"), "\n"), "\n")
	more := 0
	if len(lines) > shellMaxLines {
		more = len(lines) - shellMaxLines
		lines = lines[:shellMaxLines]
	}
	widest := 0
	for _, l := range lines {
		widest = max(widest, ansi.StringWidth(l))
	}
	src := "```sh\n" + strings.Join(lines, "\n") + "\n```"
	rows := strings.Split(renderMarkdown(wideRenderer((widest+codeMargin+8+19)/20*20), src), "\n")
	for len(rows) > 0 && strings.TrimSpace(ansi.Strip(rows[0])) == "" {
		rows = rows[1:]
	}
	for len(rows) > 0 && strings.TrimSpace(ansi.Strip(rows[len(rows)-1])) == "" {
		rows = rows[:len(rows)-1]
	}
	for i, r := range rows {
		r = trimRightCells(r)
		if w > 0 && ansi.StringWidth(r) > w {
			r = ansi.Truncate(r, w, "…")
		}
		rows[i] = r
	}
	if more > 0 {
		rows = append(rows, stDim.Render(fmt.Sprintf("  … %d more lines", more)))
	}
	return rows
}

// jobNoticeRe reads a background job notice's head: everything up to the
// opening backtick of the command, and the command to the backtick that is
// followed by " (" (the duration or "still running").
var jobNoticeRe = regexp.MustCompile("(?s)^(background job #\\d+ [^`]*?— )`(.*?)`( \\(.*)$")

// noticeLine renders a notification for the conversation: one flattened line
// as always, unless it is a job notice whose command has several lines; then
// the head, the command as shell rows, and the rest of the notice.
func noticeLine(text string, w int) string {
	if m := jobNoticeRe.FindStringSubmatch(text); m != nil && strings.Contains(strings.TrimSpace(m[2]), "\n") {
		out := []string{stNote.Render("🔔 " + oneLine(m[1]))}
		for _, r := range shellBlock(m[2], w) {
			out = append(out, r)
		}
		return strings.Join(append(out, stNote.Render("  "+oneLine(strings.TrimPrefix(m[3], " ")))), "\n")
	}
	return stNote.Render("🔔 " + oneLine(text))
}

// noticeW is the width a notice's command rows are cut to.
func (m *tuiModel) noticeW() int {
	if m.w <= 0 {
		return 0
	}
	return max(1, m.w-1)
}
