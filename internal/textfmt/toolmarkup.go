package textfmt

import (
	"regexp"
	"strings"
)

// Tool-call markup in text: a model that wrote its tool call into the reply
// instead of the API's tool_calls (DeepSeek's DSML with U+FF5C bars, USER
// 2026-11-18). Nothing in text runs, so the harness tells the model and the
// TUI shows the markup raw. Only a call's structure counts, not a tag named
// in prose ("Qwen wraps calls in <tool_call> tags"), and markup in code
// (fenced, indented or inline) is prose about the tags, not a call.

// toolMarkers alone say "a tool call in text", with the name ToolMarkup
// gives each.
var toolMarkers = []struct{ mark, name string }{
	{"<｜DSML｜", "DSML markup"},
	{"<|DSML|", "DSML markup"},
	{"<|tool▁calls▁begin|>", "<|tool▁calls▁begin|>"},
	{"<｜tool▁calls▁begin｜>", "<|tool▁calls▁begin|>"},
}

// toolCallJSON is Hermes/Qwen's form: <tool_call> then a JSON object with a
// name.
var toolCallJSON = regexp.MustCompile(`<tool_call>\s*\{\s*"name"`)

// toolSpanTokens mark a line as part of a tool-call span (openers, closers,
// parameters), for FenceToolMarkup.
var toolSpanTokens = []string{
	"｜DSML｜", "|DSML|", "<invoke", "</invoke", "<parameter", "</parameter",
	"function_calls>", "tool_call>", "tool▁call",
}

// ToolMarkup names the tool-call markup in s outside code, "" with none.
func ToolMarkup(s string) string {
	if !strings.Contains(s, "<") {
		return ""
	}
	return markupIn(strings.Join(codeFree(strings.Split(s, "\n")), "\n"))
}

// markupIn is ToolMarkup on text with the code taken out.
func markupIn(s string) string {
	for _, m := range toolMarkers {
		if strings.Contains(s, m.mark) {
			return m.name
		}
	}
	switch {
	case strings.Contains(s, "<invoke name=") && strings.Contains(s, "<parameter name="):
		return "<invoke>/<parameter>"
	case strings.Contains(s, "<function_calls>") && strings.Contains(s, "<invoke"):
		return "<function_calls>"
	case toolCallJSON.MatchString(s):
		return "<tool_call>"
	}
	return ""
}

// FenceToolMarkup puts each tool-call span in s (outside code) in a code
// fence, so a Markdown renderer shows it raw instead of dropping the tags as
// HTML. A span is the lines from a markup line to the last one after it in
// the same paragraph, or up to a parameter's close. In a blockquote or an
// indented block the fence lines carry the line's prefix; a span that opens
// on a list item's marker line is left as it is. s is unchanged when
// ToolMarkup finds none.
func FenceToolMarkup(s string) string {
	if ToolMarkup(s) == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	free := codeFree(lines)
	tok := func(i int) bool {
		for _, t := range toolSpanTokens {
			if strings.Contains(free[i], t) {
				return true
			}
		}
		return false
	}
	out := make([]string, 0, len(lines)+4)
	for i := 0; i < len(lines); {
		if !tok(i) {
			out = append(out, lines[i])
			i++
			continue
		}
		start, end, open := i, i, paramOpen(free[i])
		for j := i + 1; j < len(lines); j++ {
			if tok(j) {
				end, open = j, open+paramOpen(free[j])
				continue
			}
			// A blank line or code ends the paragraph, unless a parameter's
			// value is still open.
			if open <= 0 && strings.TrimSpace(free[j]) == "" {
				break
			}
		}
		span := lines[start : end+1]
		prefix, ok := spanPrefix(span)
		if !ok || markupIn(strings.Join(free[start:end+1], "\n")) == "" {
			out = append(out, span...)
			i = end + 1
			continue
		}
		fence := prefix + strings.Repeat("`", max(3, longestRun(strings.Join(span, "\n"), '`')+1))
		out = append(out, fence)
		out = append(out, span...)
		out = append(out, fence)
		i = end + 1
	}
	return strings.Join(out, "\n")
}

// paramOpen is the parameters a line opens less the ones it closes.
func paramOpen(l string) int {
	closed := strings.Count(l, "</parameter>") + strings.Count(l, "</｜DSML｜parameter>") + strings.Count(l, "</|DSML|parameter>")
	return strings.Count(l, "parameter name=") - closed
}

// containerRe is a line's blockquote markers and indent.
var containerRe = regexp.MustCompile(`^[ \t>]*`)

// spanPrefix is the prefix the fence lines take: the first line's
// blockquote markers and indent, which every line must carry (a quote's
// blank line may drop its trailing space). false: leave the span alone (a
// list marker line, or lines that disagree).
func spanPrefix(span []string) (string, bool) {
	prefix := containerRe.FindString(span[0])
	if listRe.MatchString(span[0][len(prefix):]) {
		return "", false
	}
	for _, l := range span[1:] {
		if !strings.HasPrefix(l, prefix) && l != strings.TrimRight(prefix, " \t") {
			return "", false
		}
	}
	return prefix, true
}

// quoteRe is a line's blockquote markers; listRe a list item's marker.
var (
	quoteRe = regexp.MustCompile(`^(?:[ ]{0,3}>[ ]?)*`)
	listRe  = regexp.MustCompile(`^[ ]{0,3}(?:[-*+]|\d{1,9}[.)])(?:[ \t]+|$)`)
)

// codeFree is lines with code taken out: a fenced block's lines (and its
// fences) and an indented block's lines empty, inline code spans removed.
// One entry per line. Fences count inside blockquotes and list items; a
// fence closes only with its own character, at least as many as opened it.
// An indented block (4 columns) starts after a blank line, outside a list.
func codeFree(lines []string) []string {
	out := make([]string, len(lines))
	var fc byte // the open fence's character
	fn := 0     // and its length; 0: none open
	inList := false
	prevBlank, prevIndent := true, false
	for i, ln := range lines {
		body := ln[len(quoteRe.FindString(ln)):]
		marker := listRe.FindString(body)
		rest := strings.TrimSpace(body[len(marker):])
		if fn > 0 {
			if c, n := fenceRun(rest); c == fc && n >= fn && strings.TrimSpace(rest[n:]) == "" {
				fn = 0
			}
			continue
		}
		if c, n := fenceRun(rest); n >= 3 && (c == '~' || !strings.Contains(rest[n:], "`")) {
			fc, fn = c, n
			prevBlank, prevIndent = false, false
			continue
		}
		if rest == "" {
			prevBlank = true
			continue
		}
		ind := indentOf(body)
		switch {
		case marker != "":
			inList = true
		case ind == 0:
			inList = false
		}
		if marker == "" && !inList && ind >= 4 && (prevBlank || prevIndent) {
			prevBlank, prevIndent = false, true
			continue
		}
		prevBlank, prevIndent = false, false
		out[i] = stripInlineCode(ln)
	}
	return out
}

// fenceRun is the fence character s opens with and the run's length; 0
// when s does not open with ` or ~.
func fenceRun(s string) (byte, int) {
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return 0, 0
	}
	return s[0], runAt(s, 0)
}

// indentOf is s's leading indent in columns (a tab is 4).
func indentOf(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// stripInlineCode removes `code` spans: a run of n backticks to the next run
// of exactly n. An unclosed run stays as text.
func stripInlineCode(s string) string {
	if !strings.Contains(s, "`") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '`' {
			b.WriteByte(s[i])
			i++
			continue
		}
		n := runAt(s, i)
		end := -1
		for j := i + n; j < len(s); {
			if s[j] != '`' {
				j++
				continue
			}
			m := runAt(s, j)
			if m == n {
				end = j + m
				break
			}
			j += m
		}
		if end < 0 {
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		i = end
	}
	return b.String()
}

// runAt is the length of the run of s[i] starting at i.
func runAt(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == s[i] {
		n++
	}
	return n
}

// longestRun is the longest run of c in s.
func longestRun(s string, c byte) int {
	best := 0
	for i := 0; i < len(s); {
		if s[i] != c {
			i++
			continue
		}
		n := runAt(s, i)
		best = max(best, n)
		i += n
	}
	return best
}
