package main

import (
	"fmt"
	"sort"
	"strings"
)

// Tool runs: a turn that makes many tool calls in a row shows the LATEST one
// as it always did, and folds every earlier one into a single row above it.
//
//	▸ ⚙ 6 tool calls — exec×4, eval×2 · 1 failed
//	▸ ✻ thought for 2s · 40 words — …        (the latest round's thinking)
//	⚙ exec(go test ./...)                     (the latest call)
//
// A run is the stretch of tool calls, and the thinking between them, with
// nothing else in it: a reply, a user message or a notification ends it.
// The fold happens when a NEW call arrives — the previous call and its
// thinking move into the aggregate, and the new call takes the bottom.
//
// The latest call's space never shrinks while the run goes on: each call is
// held at the tallest height the run has shown (holdH), so a short call after
// a long one does not pull the log up under the reader and push it down
// again on the next. The hold is released when the run ends (settleRun) —
// at that moment a reply or the end of the turn is changing the screen
// anyway, so the one shrink lands with it instead of on its own.
//
// What it does not do: fold under the person. A fold that would take in the
// selected block, renumber the conversation while a search is walking it, or
// change rows a reader who scrolled up has on screen, waits for the next call. → on the aggregate lists its calls in place; enter
// opens it in the pager as a tree, each call a node that opens to its result.

// runItem is one thing a run folded: a call, or a round's thinking.
type runItem struct {
	tool   string
	args   map[string]any
	result string
	failed bool
	think  string // set for a thinking item; tool is then ""
}

type toolRun struct {
	items []runItem
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

func (r *toolRun) calls() int {
	n := 0
	for _, it := range r.items {
		if it.tool != "" {
			n++
		}
	}
	return n
}

// summary is the aggregate's one line: count, tools by name with counts
// (most used first), failures.
func (r *toolRun) summary() string {
	counts := map[string]int{}
	failed := 0
	for _, it := range r.items {
		if it.tool == "" {
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
		parts[i] = fmt.Sprintf("%s×%d", n, counts[n])
	}
	s := fmt.Sprintf("⚙ %s — %s", plural(r.calls(), "tool call"), strings.Join(parts, ", "))
	if failed > 0 {
		return stTool.Render(s) + stErr.Render(fmt.Sprintf(" · %d failed", failed))
	}
	return stTool.Render(s)
}

// itemLine is one item as a single line, for the in-place list and the tree.
func (it runItem) line(width int) string {
	if it.tool == "" {
		words := len(strings.Fields(it.think))
		first, _ := fitPlain(oneLine(firstLine(it.think)), 40)
		return fmt.Sprintf("✻ thought · %d words — %s", words, first)
	}
	mark := "✓ "
	if it.failed {
		mark = "✗ "
	}
	return mark + callText(it.tool, it.args, width)
}

// renderRun writes the aggregate's collapsed and expanded forms.
func (e *convoEntry) renderRun() {
	r := e.run
	e.collapsed = stDim.Render("▸ ") + r.summary()
	rows := []string{stDim.Render("▾ ") + r.summary()}
	for _, it := range r.items {
		st := stDim
		if it.failed {
			st = stErr
		}
		rows = append(rows, "  "+st.Render(it.line(80)))
	}
	e.full = strings.Join(rows, "\n")
	e.invalidateWrap()
}

// runPage is the aggregate as a tree for the pager.
func (r *toolRun) page() pageDoc {
	doc := pageDoc{title: stripANSI(r.summary())}
	for _, it := range r.items {
		n := pageNode{head: it.line(200), err: it.failed}
		if it.tool == "" {
			n.body = strings.TrimSpace(it.think)
		} else {
			n.body = callFull(it.tool, it.args) + "\n\n" + strings.TrimRight(it.result, "\n")
		}
		doc.nodes = append(doc.nodes, n)
	}
	return doc
}

// runMember says whether a block can be folded into a run.
func runMember(e convoEntry) bool {
	return e.run != nil || e.tool != nil || (e.think != nil && !e.think.live)
}

// foldRun runs as a new tool call is about to be appended: everything since
// the run began, up to the current round's thinking, folds into one
// aggregate. It returns the height the new call should be held at. live is
// false for history, which folds whatever the screen is doing.
func (m *tuiModel) foldRun(live bool) int {
	// The current round's thinking stays with the call it led to.
	k := len(m.convo)
	for k > 0 && m.convo[k-1].think != nil {
		k--
	}
	start := k
	for start > 0 && runMember(m.convo[start-1]) {
		start--
	}
	prev := -1
	for i := start; i < k; i++ {
		if m.convo[i].tool != nil {
			prev = i
		}
	}
	if prev < 0 {
		return 0 // the first call of a run: nothing to fold yet
	}
	hold := m.convo[prev].holdH
	if prev < len(m.frame.blockH) {
		hold = max(hold, m.frame.blockH[prev])
	}
	// Not under the person: a selected block in the span, a search that holds
	// indices into the conversation, or a reader who scrolled up to where the
	// span is on screen, defers the fold to the next call.
	if live && ((m.focus == focusConvo && m.sel >= start && m.sel < k) || m.searching || m.searchActive) {
		return hold
	}
	if live && !m.scrollPinned && start < len(m.frame.rowOffset) && m.frame.rowOffset[start] < m.vp.YOffset+m.vp.Height {
		return hold
	}

	// The span's items in order. An aggregate already in it is kept — its
	// identity is what an open pager follows, and its state is the person's.
	var agg *convoEntry
	var items []runItem
	for i := start; i < k; i++ {
		e := m.convo[i]
		switch {
		case e.run != nil:
			if agg == nil {
				agg = &e
			}
			items = append(items, e.run.items...)
		case e.tool != nil:
			items = append(items, runItem{tool: e.tool.name, args: e.tool.args,
				result: e.tool.raw, failed: toolFailed(e.tool.name, e.tool.raw)})
		case e.think != nil:
			items = append(items, runItem{think: e.think.text})
		}
	}
	if agg == nil {
		agg = &convoEntry{run: &toolRun{}}
	}
	agg.run.items = items
	agg.holdH = 0
	agg.renderRun()

	shift := k - start - 1
	m.convo = append(append(m.convo[:start:start], *agg), m.convo[k:]...)
	if m.sel >= k {
		m.sel -= shift
	}
	if m.pendingTool >= k {
		m.pendingTool -= shift
	}
	return hold
}

// settleRun releases the height held for a run's latest call: the run is over.
func (m *tuiModel) settleRun() {
	if !m.holding {
		return
	}
	for i := range m.convo {
		m.convo[i].holdH = 0
	}
	m.holding = false
}
