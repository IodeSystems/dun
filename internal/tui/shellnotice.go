package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/iodesystems/dun/internal/textfmt"
)

// A command in a notice opens as highlighted shell with its lines as written,
// through the same glamour renderer as a ```sh fence in a reply. The model's text
// is not touched: this is display only.

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
		rows = append(rows, StDim.Render(fmt.Sprintf("  … %d more lines", more)))
	}
	return rows
}

// jobNoticeRe reads a background job notice's head: everything up to the
// opening backtick of the command, and the command to the backtick that is
// followed by " (" (the duration or "still running").
var jobNoticeRe = regexp.MustCompile("(?s)^(background job #\\d+ [^`]*?— )`(.*?)`( \\(.*)$")

// noticeEntry is a notification for the conversation: one line, whatever came
// in, because a finished job's notice carries its output's tail and that used to
// flatten into a wall. Enter on it shows the rest: the command as shell rows, and
// the output with its lines as written.
func noticeEntry(text string, w int) convoEntry {
	if m := jobNoticeRe.FindStringSubmatch(text); m != nil {
		head := strings.TrimSuffix(strings.TrimSpace(m[1]), "—")
		took, rest, _ := strings.Cut(strings.TrimPrefix(m[3], " "), "\n")
		cmd := strings.TrimSpace(m[2])
		first, _, several := strings.Cut(cmd, "\n")
		line := head + took + " · $ " + first
		if several {
			line += fmt.Sprintf(" (+%d lines)", strings.Count(cmd, "\n"))
		}
		out := []string{stNote.Render("🔔 " + strings.TrimSpace(head) + " " + took)}
		out = append(out, shellBlock(m[2], w)...)
		if rest = strings.TrimRight(rest, "\n"); rest != "" {
			for _, l := range strings.Split(rest, "\n") {
				out = append(out, StDim.Render(l))
			}
		}
		return convoEntry{collapsed: noticeRow(line, w), full: strings.Join(out, "\n")}
	}
	first, _, several := strings.Cut(strings.TrimSpace(text), "\n")
	e := convoEntry{collapsed: noticeRow(first, w)}
	if several || (w > 0 && ansi.StringWidth("🔔 "+first) > w) {
		e.full = stNote.Render("🔔 " + strings.TrimSpace(text))
	}
	return e
}

// noticeRow is a notice's one row, cut to w columns (w <= 0: not cut).
func noticeRow(text string, w int) string {
	line := "🔔 " + textfmt.OneLine(text)
	if w > 0 {
		line = ansi.Truncate(line, w, "…")
	}
	return stNote.Render(line)
}

// noticeW is the width a notice's command rows are cut to.
func (m *tuiModel) noticeW() int {
	if m.w <= 0 {
		return 0
	}
	return max(1, m.w-1)
}
