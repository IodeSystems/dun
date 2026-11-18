package textfmt

import "strings"

// Tool-call markup in text: a model that wrote its tool call into the reply
// instead of the API's tool_calls (DeepSeek's DSML with U+FF5C bars, USER
// 2026-11-18). Nothing in text runs, so the harness tells the model and the
// TUI shows the markup raw. Markup inside fenced or inline code is prose
// about the tags, not a call, and is ignored.

// toolMarkers are the markers that alone say "a tool call in text", with the
// name ToolMarkup gives for each.
var toolMarkers = []struct{ mark, name string }{
	{"<｜DSML｜", "DSML markup"},
	{"<|DSML|", "DSML markup"},
	{"<function_calls>", "<function_calls>"},
	{"<tool_call>", "<tool_call>"},
	{"<|tool▁calls▁begin|>", "<|tool▁calls▁begin|>"},
	{"<｜tool▁calls▁begin｜>", "<|tool▁calls▁begin|>"},
}

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

func markupIn(s string) string {
	for _, m := range toolMarkers {
		if strings.Contains(s, m.mark) {
			return m.name
		}
	}
	if strings.Contains(s, "<invoke") && strings.Contains(s, "<parameter") {
		return "<invoke>/<parameter>"
	}
	return ""
}

// FenceToolMarkup puts the tool-call markup in s (outside code) in a code
// fence, so a Markdown renderer shows it raw instead of dropping the tags as
// HTML. The fence runs from the first line with markup to the last; s is
// unchanged when ToolMarkup finds none.
func FenceToolMarkup(s string) string {
	if ToolMarkup(s) == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	free := codeFree(lines)
	first, last := -1, -1
	for i, l := range free {
		for _, t := range toolSpanTokens {
			if strings.Contains(l, t) {
				if first < 0 {
					first = i
				}
				last = i
				break
			}
		}
	}
	if first < 0 {
		return s
	}
	span := strings.Join(lines[first:last+1], "\n")
	fence := strings.Repeat("`", max(3, longestRun(span, '`')+1))
	out := make([]string, 0, len(lines)+2)
	out = append(out, lines[:first]...)
	out = append(out, fence, span, fence)
	out = append(out, lines[last+1:]...)
	return strings.Join(out, "\n")
}

// codeFree is lines with code taken out: a fenced block's lines (and its
// fences) empty, inline code spans removed. One entry per line.
func codeFree(lines []string) []string {
	out := make([]string, len(lines))
	fence := ""
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = t[:3]
			continue
		}
		out[i] = stripInlineCode(ln)
	}
	return out
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
