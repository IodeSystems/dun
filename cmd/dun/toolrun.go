package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/cellbuf"
)

// Tool runs: the tool calls of a turn, and the thinking between them, are ONE
// bounded box (box.go) that only ever grows.
//
//	⚙ 5 steps — exec ×4, notes · project notes · 1 failed · thought 12s    ↑ 40 more
//	│ ✻ thought for 2s · 40 words
//	│ │ the round's reasoning, dimmed
//	│ ✓ $ go test ./...  → ✓ 12 lines · ok
//	│ ✻ thinking… 1s · 12 words
//	│ │ the newest lines, followed while live
//
// Every new thought or call is APPENDED inside the box; nothing above it is
// removed, folded or shrunk, because the conversation's height never goes
// down while a session runs (the no-screen-jump rule — tested as a property
// over mixed event streams, see noshrink_test.go). The header carries the
// summary and changes in place.
//
// A run is OPEN while it is the last block and the round's reply has not
// begun: thoughts and calls go into it. A reply, a user message, a
// notification — anything appended after it — ends it, and the next call
// starts a new run. The first call of a run takes over the thinking box just
// above it, if there is one: the thought becomes the run's first item, in
// the same place, so the box only grows.
//
// Enter opens the run in the pager as a tree, a node per thought and per
// call; "i" on a call node opens it in the inspector.

// runItem is one thing in a run: a call, or a round's thinking.
type runItem struct {
	tool   string
	args   map[string]any
	result string
	done   bool   // the result arrived
	failed bool   // it was a failure (toolFailed)
	head   string // the call and its one-line preview, styled
	// preview is the result's one-line preview alone ("  → …"), for a
	// narrow pane, where it takes rows of its own (callPreviewRows).
	preview string
	output  string // the renderer's styled output, for the pager
	think   *thinkBlock
	wrap    boxState // a thought's wrapped rows (box.go's line-at-a-time cache)
}

type toolRun struct {
	items []*runItem
	open  bool // still taking calls and thoughts
	// interrupted: the user stopped the turn while this run was open.
	interrupted bool
	ver         int // bumped on every change, the key of the rows cache

	rows    []string // the box body as of rowsVer at rowsW
	rowsVer int
	rowsW   int
}

// toolFailed reads whether a result is a failure: exec's own exit marker, or
// the ERROR the dispatcher returns for a call that never ran.
func toolFailed(tool, result string) bool {
	if tool == "exec" {
		if _, failed := execExitMarker(result); failed {
			return true
		}
	}
	r := strings.TrimSpace(result)
	return strings.HasPrefix(r, "ERROR") || strings.HasPrefix(r, "error:")
}

// summary is the run's header: count, tools by name with counts (most used
// first), failures, and how long it spent thinking.
func (r *toolRun) summary() string {
	counts := map[string]int{}
	failed := 0
	var thought time.Duration
	for _, it := range r.items {
		if it.think != nil {
			switch {
			case it.think.live:
				thought += time.Since(it.think.start)
			case !it.think.end.IsZero():
				thought += it.think.end.Sub(it.think.start)
			}
			continue
		}
		counts[it.tool]++
		if it.failed {
			failed++
		}
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = toolLabel(n)
		if counts[n] > 1 {
			parts[i] += fmt.Sprintf(" ×%d", counts[n])
		}
	}
	// Neutral: the green is for ✓ alone, so a run's rows do not outrank the
	// answer under them (run 4).
	s := fmt.Sprintf("⚙ %s — %s", plural(len(r.items), "step"), strings.Join(parts, ", "))
	if thought > 0 {
		s += stDim.Render(" · thought " + thinkDur(thought))
	}
	if failed > 0 {
		s += stErr.Render(fmt.Sprintf(" · %d failed", failed))
	}
	if r.interrupted {
		s += stDim.Render(" · ⏹ interrupted")
	}
	return s
}

// line is an item as a single line, for the tree's heads and the plain text.
func (it *runItem) line(width int) string {
	if it.think != nil {
		return thinkHeader(it.think)
	}
	mark := "✓ "
	switch {
	case !it.done:
		mark = "… "
	case it.failed:
		mark = "✗ "
	}
	return mark + callText(it.tool, it.args, width)
}

// boxRows is the run's box body at width w, a step per line:
//
//	✻ the overlay is read before the project file   (2s, 60 words)
//	✗ $ go test ./internal/...  → ✗ 2 failed
//	    config_test.go:41: want 3 layers, got 2
//	✓ read ×4: a.go, b.go, c.go, d.go
//	▾ ✻ thinking… 1s · 12 words
//	  the live thought's text, all of it; the box shows its tail
//
// Finished steps are one line (a failure keeps up to two lines of its
// error); only the ACTIVE step — the thought still streaming — is open. The
// box windows the tail of this, so the active step gets the rows left under
// the cap, and when it finishes and folds to one line the rows it gives back
// show earlier steps instead: the box never shrinks (renderBox).
func (r *toolRun) boxRows(w int) []string {
	if r.rows != nil && r.rowsVer == r.ver && r.rowsW == w && !r.live() {
		return r.rows
	}
	rows := r.rows[:0]
	lw := max(1, w-2)
	for k := 0; k < len(r.items); k++ {
		it := r.items[k]
		switch {
		case it.think != nil && it.think.live:
			rows = append(rows, "  "+stDim.Render("▾ "+thinkHeader(it.think)))
			it.wrap.wrapBody(strings.TrimSpace(it.think.text), w, true, "    ")
			rows = append(rows, it.wrap.rows...)
		case it.think != nil:
			rows = append(rows, "  "+thoughtLine(it.think, lw))
		default:
			if n := groupLen(r.items[k:]); n > 1 {
				rows = append(rows, "  "+groupLine(r.items[k:k+n], lw))
				k += n - 1
				continue
			}
			rows = append(rows, "  "+fitRow(callLine(it), lw))
			if it.done && it.failed {
				for _, ln := range failLines(it) {
					rows = append(rows, "      "+fitRow(stErr.Render(ln), max(1, w-6)))
				}
			}
		}
	}
	r.rows, r.rowsVer, r.rowsW = rows, r.ver, w
	return rows
}

// live says the run's last step is a thought still streaming.
func (r *toolRun) live() bool {
	n := len(r.items)
	return n > 0 && r.items[n-1].think != nil && r.items[n-1].think.live
}

// thoughtLine is a finished thought as one line: its LAST sentence — where it
// landed — then how long and how much.
func thoughtLine(t *thinkBlock, w int) string {
	meta := fmt.Sprintf("  (%s, %d words)", thoughtTime(t), len(strings.Fields(t.text)))
	last, _ := fitPlain(lastSentence(t.text), max(1, w-2-len([]rune(meta))))
	return stSecondary.Render("✻ " + last + meta)
}

func thoughtTime(t *thinkBlock) string {
	if t.end.IsZero() || t.start.IsZero() {
		return "–"
	}
	return thinkDur(t.end.Sub(t.start))
}

// lastSentence is the final sentence of a thought, on one line.
func lastSentence(s string) string {
	s = strings.TrimSpace(oneLine(s))
	body := strings.TrimRight(s, ".!?…")
	cut := -1
	for i := len(body) - 1; i > 0; i-- {
		if (body[i-1] == '.' || body[i-1] == '!' || body[i-1] == '?') && body[i] == ' ' {
			cut = i
			break
		}
	}
	if cut < 0 {
		return s
	}
	return strings.TrimSpace(s[cut:])
}

// callLine is a call: its mark, short form and outcome preview.
func callLine(it *runItem) string {
	mark := stDim.Render("… ")
	switch {
	case it.done && it.failed:
		mark = stErr.Render("✗ ")
	case it.done:
		mark = stTool.Render("✓ ")
	}
	if it.head != "" {
		return mark + it.head
	}
	return mark + callText(it.tool, it.args, 80)
}

// narrowCall is the widest pane on which a one-call run puts its result's
// preview on rows of its own, under the call: on a phone the call and the
// preview on one row left the preview three words ("→ Notes: keep the…").
const narrowCall = 60

// callPreviewRows is the preview on up to two rows of its own, from column
// 2, wrapped at a word; the second ends in an ellipsis when there is more.
func callPreviewRows(preview string, w int) []string {
	plain := ansi.Strip(preview)
	at := strings.Index(plain, "→ ")
	if at < 0 || strings.TrimSpace(plain[:at]) != "" {
		return nil
	}
	start := ansi.StringWidth(plain[:at]) + 2
	body := ansi.Cut(preview, start, ansi.StringWidth(preview))
	if strings.TrimSpace(ansi.Strip(body)) == "" {
		return nil
	}
	lines := strings.Split(cellbuf.Wrap(body, max(1, w-4), ""), "\n")
	if len(lines) > 2 {
		lines[1] = fitText(lines[1]+" "+lines[2], max(1, w-4))
		lines = lines[:2]
	}
	rows := []string{stDim.Render("  → ") + lines[0]}
	for _, l := range lines[1:] {
		rows = append(rows, "    "+l)
	}
	return rows
}

// failLines are up to two lines of a failed call's error.
func failLines(it *runItem) []string {
	if it.tool == "eval" {
		return evalFailLines(it.result)
	}
	lines := meaningfulLines(it.result)
	if n := len(lines); n > 0 && strings.HasPrefix(lines[n-1], "[exit:") {
		lines = lines[:n-1]
	}
	if len(lines) > 2 {
		lines = lines[len(lines)-2:]
	}
	return lines
}

// callTarget is what a call was about, short: exec's command, or a tool's
// only argument. "" when it is not short enough to group.
func callTarget(it *runItem) string {
	var v string
	switch {
	case it.tool == "exec":
		v, _ = it.args["command"].(string)
	case len(it.args) == 1:
		for _, a := range it.args {
			v = fmt.Sprint(a)
		}
	default:
		return ""
	}
	v = oneLine(v)
	if v == "" || len(v) > 30 {
		return ""
	}
	return v
}

// groupLen is how many calls at the head of items group into one line:
// consecutive finished, successful calls of one tool with short targets.
func groupLen(items []*runItem) int {
	n := 0
	for _, it := range items {
		if it.think != nil || !it.done || it.failed || it.tool != items[0].tool || callTarget(it) == "" {
			break
		}
		n++
	}
	return n
}

// groupLine is a group of calls as one line: `✓ read ×4: a.go, b.go, …`.
func groupLine(items []*runItem, w int) string {
	label := toolLabel(items[0].tool)
	switch {
	case items[0].tool == "exec":
		label = "$"
	case isFileTool(items[0].tool):
		label = strings.TrimPrefix(items[0].tool, filePrefix) // read ×4: a.go, …
	}
	label += fmt.Sprintf(" ×%d: ", len(items))
	ts := make([]string, len(items))
	for i, it := range items {
		ts[i] = callTarget(it)
	}
	list, _ := fitPlain(strings.Join(ts, ", "), max(1, w-2-len([]rune(label))))
	return stTool.Render("✓ ") + label + stSecondary.Render(list)
}

// fitRow fits a styled row to w columns by the one rule (fitText): it ends
// at a word with an ellipsis, never mid-word at the edge — a tool call's
// result cut at 48 columns read "→ Notes: keep the greet".
func fitRow(s string, w int) string { return fitText(s, max(1, w)) }

// single is the run's one call when that is all it has: it is drawn as that
// call's line alone, with no summary over it (one call said once). A second
// step turns the line into the summary, the call one row below it.
func (r *toolRun) single() *runItem {
	if len(r.items) == 1 && r.items[0].think == nil {
		return r.items[0]
	}
	return nil
}

// singleHead and singleRows are a one-call run as a box: the call's line as
// the head, and a failure's lines under it.
func (r *toolRun) singleHead(it *runItem) string {
	s := callLine(it)
	if r.interrupted {
		s += stDim.Render(" · ⏹ interrupted")
	}
	return s
}

func singleRows(it *runItem, w int) []string {
	if !it.done || !it.failed {
		return nil
	}
	var rows []string
	for _, ln := range failLines(it) {
		rows = append(rows, "    "+fitRow(stErr.Render(ln), max(1, w-4)))
	}
	return rows
}

// plain is the run as text, for search and the screen dump.
func (r *toolRun) plain() string {
	if it := r.single(); it != nil {
		return it.line(80)
	}
	lines := []string{stripANSI(r.summary())}
	for _, it := range r.items {
		lines = append(lines, it.line(80))
		if it.think != nil {
			lines = append(lines, strings.TrimSpace(it.think.text))
		}
	}
	return strings.Join(lines, "\n")
}

// runPage is the run as a tree for the pager.
func (r *toolRun) page() pageDoc {
	doc := pageDoc{title: stripANSI(r.summary())}
	for _, it := range r.items {
		n := pageNode{head: it.line(200), err: it.failed}
		if it.think != nil {
			n.body = strings.TrimSpace(it.think.text)
		} else {
			// The renderer's full body (a table, laid-out values, a marked
			// error) where it has one; the raw result otherwise.
			res := strings.TrimRight(it.output, "\n")
			if strings.TrimSpace(stripANSI(res)) == "" {
				res = strings.TrimRight(it.result, "\n")
			}
			n.body = callFull(it.tool, it.args) + "\n\n" + res
			n.insp = &inspSource{name: it.tool, input: argFull(it.args), output: it.result}
		}
		doc.nodes = append(doc.nodes, n)
	}
	return doc
}

// ── the model's side ───────────────────────────────────────────────

// openRun is the index of the run still taking items, or -1: the last block,
// if it is a run that has not been ended.
func (m *tuiModel) openRun() int {
	if n := len(m.convo); n > 0 && m.convo[n-1].run != nil && m.convo[n-1].run.open {
		return n - 1
	}
	return -1
}

// touchRun marks run entry i changed.
func (m *tuiModel) touchRun(i int) {
	e := &m.convo[i]
	e.run.ver++
	e.collapsed = e.run.plain()
	e.invalidateWrap()
}

// runEntry makes a run block.
func runEntry(items ...*runItem) convoEntry {
	e := convoEntry{run: &toolRun{items: items, open: true}, box: &boxState{follow: true}}
	e.collapsed = e.run.plain()
	return e
}

// runAddCall appends a call to the open run, or starts one — taking over the
// finished thinking box just above, if there is one, in place. It returns the
// run's index.
func (m *tuiModel) runAddCall(tool string, args map[string]any) int {
	it := &runItem{tool: tool, args: args}
	if i := m.openRun(); i >= 0 {
		m.convo[i].run.items = append(m.convo[i].run.items, it)
		m.touchRun(i)
		return i
	}
	if n := len(m.convo); n > 0 && m.convo[n-1].think != nil && !m.convo[n-1].think.live {
		e := &m.convo[n-1]
		t := e.think
		th := &runItem{think: t}
		r := &toolRun{items: []*runItem{th, it}, open: true}
		if m.paging && m.pg.id == any(t) {
			m.pg.id = r // the pager follows the block it was reading
		}
		e.think, e.run = nil, r
		m.touchRun(n - 1)
		return n - 1
	}
	m.convo = append(m.convo, runEntry(it))
	return len(m.convo) - 1
}

// runSetResult gives the run's pending call its result.
func (m *tuiModel) runSetResult(i int, tool, result string) {
	if i < 0 || i >= len(m.convo) || m.convo[i].run == nil {
		return
	}
	r := m.convo[i].run
	var it *runItem
	for k := len(r.items) - 1; k >= 0; k-- {
		if r.items[k].think == nil && !r.items[k].done {
			it = r.items[k]
			break
		}
	}
	if it == nil { // a result with no call: show it on its own
		it = &runItem{tool: tool}
		r.items = append(r.items, it)
	}
	preview, body := renderToolResult(renderCtx{tool: it.tool, args: it.args, result: result, width: m.vp.Width})
	it.result, it.done, it.output = result, true, body
	it.failed = toolFailed(it.tool, result)
	it.preview = strings.ReplaceAll(preview, "\n", " ")
	it.head = callText(it.tool, it.args, 80) + it.preview
	m.touchRun(i)
}

// endRun ends the open run, if any: the round's reply began, or the turn did.
func (m *tuiModel) endRun() {
	if i := m.openRun(); i >= 0 {
		m.convo[i].run.open = false
		m.touchRun(i)
	}
}
