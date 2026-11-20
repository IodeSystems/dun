package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

// A step opened in the pager (toolrun.go's page), other than exec
// (execstep.go). USER 2026-11-19: a file_edit step showed its arguments as a
// Go map dump ("edits: [map[new:… old:…]]") where the edit's diff belonged.
//
//	▾ ✗ edit Makefile
//	    Makefile
//	    @@ edit 1/2
//	    -build-docker: ## Build the co-located image
//	    +build-docker: ## Build the production image
//	    …
//
//	    ERROR: old text not found in Makefile
//
// The head is the step's one line; the body is the call once, in its own
// form, and the result as the tool's renderer draws it (renderers.go,
// filerender.go). A file tool's path is a dim line; an edit's old and new are
// a diff; any other argument is a key: value line, nested values indented
// under their key, a multi-line string a block. A failure's result is red.

// stepLine is one line of a call's arguments before it is wrapped: indent
// columns, then "- " (an array item), then key (dim), then text in style.
type stepLine struct {
	indent int
	dash   bool
	key    string // "" for a line with no key (a block's line, an array item)
	text   string
	style  lipgloss.Style
	pre    bool // text is already styled: drawn as it is, wrapped by its SGR
}

// lead is the line's indent and its dash, as plain text.
func (l stepLine) lead() string {
	s := strings.Repeat(" ", l.indent)
	if l.dash {
		s += "- "
	}
	return s
}

// render is the line unwrapped.
func (l stepLine) render() string {
	s := l.lead()
	if l.key != "" {
		k := l.key + ":"
		if l.text != "" {
			k += " "
		}
		s += StDim.Render(k)
	}
	if l.pre {
		return s + l.text
	}
	return s + l.style.Render(l.text)
}

// rows is the line at w columns: a row too wide breaks at a word, its
// continuation indented two past the line's own start.
func (l stepLine) rows(w int) []string {
	if l.pre {
		lead := l.lead()
		var out []string
		for i, r := range wrapOutputLine(l.text, max(1, w-len(lead))) {
			if i == 0 {
				out = append(out, lead+r)
			} else {
				out = append(out, strings.Repeat(" ", len(lead))+r)
			}
		}
		return out
	}
	lead := l.lead()
	prefix := ""
	if l.key != "" {
		prefix = l.key + ":"
		if l.text != "" {
			prefix += " "
		}
	}
	hang := strings.Repeat(" ", len(lead)+2)
	wrapped := strings.Split(xansi.Wrap(prefix+l.text, max(1, w-len(hang)), ""), "\n")
	out := make([]string, len(wrapped))
	for i, r := range wrapped {
		if i > 0 {
			out[i] = hang + l.style.Render(r)
			continue
		}
		if prefix != "" && strings.HasPrefix(r, prefix) {
			out[i] = lead + StDim.Render(prefix) + l.style.Render(r[len(prefix):])
		} else {
			out[i] = lead + l.style.Render(r)
		}
	}
	return out
}

// detab turns tabs into four spaces: a Makefile's recipe lines, measured.
func detab(s string) string { return strings.ReplaceAll(s, "\t", "    ") }

// argNorm is v as JSON would decode it: a Go-built []string or
// map[string]string reads like the wire's []any and map[string]any.
func argNorm(v any) any {
	switch v.(type) {
	case nil, string, bool, float64, map[string]any, []any:
		return v
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return string(b)
	}
	return out
}

// argScalar is a scalar as text: a number without a float's ".000000", nil
// as null.
func argScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// argInline is a value on one line: a scalar as text, an object or array as
// compact JSON — never Go's "map[k:v]".
func argInline(v any) string {
	v = argNorm(v)
	switch v.(type) {
	case map[string]any, []any:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if enc.Encode(v) == nil {
			return strings.TrimRight(b.String(), "\n")
		}
	}
	return argScalar(v)
}

// argLines is args as key: value lines from indent, keys sorted.
func argLines(args map[string]any, indent int) []stepLine {
	var out []stepLine
	for _, k := range sortedKeys(args) {
		out = append(out, valueLines(k, args[k], indent)...)
	}
	return out
}

// valueLines is one value under key ("" for an array item) at indent.
func valueLines(key string, v any, indent int) []stepLine {
	v = argNorm(v)
	plain := stSecondary
	switch x := v.(type) {
	case string:
		if !strings.Contains(x, "\n") {
			return []stepLine{{indent: indent, key: key, text: detab(x), style: plain}}
		}
		out := []stepLine{{indent: indent, key: key, text: "|", style: StDim}}
		for _, ln := range strings.Split(strings.TrimRight(x, "\n"), "\n") {
			out = append(out, stepLine{indent: indent + 2, text: detab(ln), style: plain})
		}
		return out
	case map[string]any:
		if len(x) == 0 {
			return []stepLine{{indent: indent, key: key, text: "{}", style: StDim}}
		}
		return append([]stepLine{{indent: indent, key: key}}, argLines(x, indent+2)...)
	case []any:
		if len(x) == 0 {
			return []stepLine{{indent: indent, key: key, text: "[]", style: StDim}}
		}
		out := []stepLine{{indent: indent, key: key}}
		for _, item := range x {
			out = append(out, itemLines(item, indent+2)...)
		}
		return out
	}
	return []stepLine{{indent: indent, key: key, text: argScalar(v), style: plain}}
}

// itemLines is an array item at indent: "- " and the item, an object's
// first key on the dash's line and the rest under it (YAML's form).
func itemLines(item any, indent int) []stepLine {
	item = argNorm(item)
	var sub []stepLine
	switch x := item.(type) {
	case map[string]any:
		if len(x) > 0 {
			sub = argLines(x, indent+2)
		}
	case []any:
		if len(x) > 0 {
			sub = []stepLine{{indent: indent, dash: true}}
			for _, it := range x {
				sub = append(sub, itemLines(it, indent+2)...)
			}
			return sub
		}
	case string:
		if strings.Contains(x, "\n") {
			sub = valueLines("", x, indent+2)
			sub[0].indent = indent
			sub[0].dash = true
			return sub
		}
	}
	if sub == nil {
		l := valueLines("", item, indent)[0]
		l.dash = true
		return []stepLine{l}
	}
	sub[0].indent, sub[0].dash = indent, true
	return sub
}

// without is args less keys.
func without(args map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// editPairs is a file_edit call's replacements: edits [{old, new}], or the
// old/new at the top level the tool still accepts. ok=false when the args
// are not that shape.
func editPairs(args map[string]any) (pairs [][2]string, ok bool) {
	if list, isList := argNorm(args["edits"]).([]any); isList {
		for _, e := range list {
			m, isMap := e.(map[string]any)
			if !isMap {
				return nil, false
			}
			o, _ := m["old"].(string)
			n, _ := m["new"].(string)
			pairs = append(pairs, [2]string{o, n})
		}
		return pairs, len(pairs) > 0
	}
	o, okO := args["old"].(string)
	n, okN := args["new"].(string)
	if okO || okN {
		return [][2]string{{o, n}}, true
	}
	return nil, false
}

// editDiffLines is the replacements as a diff: per edit a hunk line when
// there are several, old's lines as −, new's as +.
func editDiffLines(pairs [][2]string) []stepLine {
	var out []stepLine
	for i, p := range pairs {
		if len(pairs) > 1 {
			out = append(out, stepLine{text: fmt.Sprintf("@@ edit %d/%d", i+1, len(pairs)), style: diffHun})
		}
		for _, side := range []struct {
			s    string
			mark string
			st   lipgloss.Style
		}{{p[0], "-", diffDel}, {p[1], "+", diffAdd}} {
			if side.s == "" {
				continue
			}
			for _, ln := range strings.Split(strings.TrimRight(side.s, "\n"), "\n") {
				out = append(out, stepLine{text: side.mark + detab(ln), style: side.st})
			}
		}
	}
	return out
}

// stepCallLines is the call's part of a step's body: what the head does not
// already say. Where the renderer's body draws part of the call (a
// successful edit's diff, an ask's question) that part is left to it.
func stepCallLines(tool string, args map[string]any, result string, failed bool) []stepLine {
	switch tool {
	case "eval":
		if code := evalCode(args); strings.Contains(code, "\n") {
			var out []stepLine
			for _, ln := range strings.Split(highlightShell(code), "\n") {
				out = append(out, stepLine{text: ln, pre: true})
			}
			return out
		}
		return argLines(without(args, "code"), 0)
	case "ask_user", "ask_parent":
		if !failed { // askRender's body has the question
			return argLines(without(args, "question"), 0)
		}
	case "dun_msg":
		if !failed { // dunMsgRender's body has the text
			return argLines(without(args, "text"), 0)
		}
	}
	if !isFileTool(tool) {
		return argLines(args, 0)
	}
	path := argStr(args, "path")
	var out []stepLine
	if path != "" {
		out = append(out, stepLine{text: path, style: StDim})
	}
	rest := without(args, "path")
	if tool == filePrefix+"edit" {
		rest = without(rest, "edits", "old", "new")
		// A successful edit's result is the diff, with its context lines;
		// otherwise (refused, or not applied) the call's own old and new.
		if failed || !isDiff(result) {
			if pairs, ok := editPairs(args); ok {
				out = append(out, editDiffLines(pairs)...)
			} else {
				rest = without(args, "path")
			}
		}
	}
	return append(out, argLines(rest, 0)...)
}

// stepResultLines is the result's part: the renderer's body, or for a
// failure the result in red.
func stepResultLines(result, output string, failed bool) []string {
	if failed {
		r := strings.TrimRight(stripANSI(result), "\n")
		if strings.TrimSpace(r) == "" {
			return nil
		}
		var out []string
		for _, ln := range strings.Split(r, "\n") {
			out = append(out, StErr.Render(detab(ln)))
		}
		return out
	}
	res := strings.TrimRight(output, "\n")
	if strings.TrimSpace(stripANSI(res)) == "" {
		res = stSecondary.Render(strings.TrimRight(result, "\n"))
	}
	if strings.TrimSpace(stripANSI(res)) == "" {
		return nil
	}
	return strings.Split(detab(res), "\n")
}

// stepRows is a step's body at w columns.
func stepRows(tool string, args map[string]any, result, output string, failed bool, w int) []string {
	var rows []string
	for _, l := range stepCallLines(tool, args, result, failed) {
		rows = append(rows, l.rows(w)...)
	}
	res := stepResultLines(result, output, failed)
	if len(rows) > 0 && len(res) > 0 {
		rows = append(rows, "")
	}
	for _, ln := range res {
		rows = append(rows, wrapOutputLine(ln, w)...)
	}
	if len(rows) == 0 {
		rows = []string{StDim.Render("(no output)")}
	}
	return rows
}

// stepText is a step's body unwrapped, for a reader of the node's body text.
func stepText(tool string, args map[string]any, result, output string, failed bool) string {
	var lines []string
	for _, l := range stepCallLines(tool, args, result, failed) {
		lines = append(lines, l.render())
	}
	res := stepResultLines(result, output, failed)
	if len(lines) > 0 && len(res) > 0 {
		lines = append(lines, "")
	}
	return strings.Join(append(lines, res...), "\n")
}
