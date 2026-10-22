package main

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// The no-screen-jump rule: while a session runs, the conversation's total
// rendered height never goes down. Only the person may shrink it — /clear,
// closing something they opened, a resize (which re-wraps). Events alone —
// thinking, replies, tool runs, retries, errors, questions — only add.
//
// This drives long mixed event streams through the model and checks the
// height after EVERY event (and every render tick), over every block — the
// whole conversation, not just what is on screen.

// convoHeight is the rendered height of every conversation block and the
// streaming reply; the suggestion rows under them are an idle-only picker,
// not conversation.
func totalHeight(m tuiModel) int {
	n := len(m.convo)
	if replyShown(m.cur) {
		n++
	}
	h := 0
	for i := 0; i < n && i < len(m.frame.blockH); i++ {
		h += m.frame.blockH[i]
	}
	return h
}

type shrinkCheck struct {
	t    *testing.T
	v    *vtui
	last int
	step int
	log  []string
}

func (c *shrinkCheck) ev(ev map[string]any) {
	c.t.Helper()
	c.v.event(ev)
	nm, _ := c.v.m.Update(renderTickMsg{})
	c.v.m = nm.(tuiModel)
	c.step++
	c.log = append(c.log, fmt.Sprintf("%v %q", ev["type"], clip(oneLine(fmt.Sprint(ev["text"], ev["result"])), 30)))
	if len(c.log) > 12 {
		c.log = c.log[1:]
	}
	h := totalHeight(c.v.m)
	if h < c.last {
		c.t.Fatalf("step %d: the conversation shrank %d → %d rows after:\n  %s\n%s",
			c.step, c.last, h, strings.Join(c.log, "\n  "), c.v.view())
	}
	c.last = h
}

func newShrinkCheck(t *testing.T, w, h int) *shrinkCheck {
	v := newVtui(w, h)
	v.m.proc = &dunProc{stdin: discardWC{}}
	c := &shrinkCheck{t: t, v: v}
	c.ev(map[string]any{"type": "ready", "tools": []any{"exec", "eval", "ask_user"}})
	c.ev(map[string]any{"type": "user", "content": "start the work"})
	c.ev(map[string]any{"type": "turn"})
	return c
}

// The sequences that shrank under the old fold-and-replace: thinking, a call,
// thinking, a call — each new call used to replace the round before with a
// one-line aggregate.
func TestNoShrink_ToolRunsWithThinking(t *testing.T) {
	c := newShrinkCheck(t, 80, 24)
	for round := 0; round < 8; round++ {
		for k := 0; k < 15; k++ {
			c.ev(map[string]any{"type": "reasoning", "text": fmt.Sprintf("round %d line %d of the plan. ", round, k)})
			if k%4 == 3 {
				c.ev(map[string]any{"type": "reasoning", "text": "\n"})
			}
		}
		c.ev(map[string]any{"type": "tool_call", "tool": "exec", "args": map[string]any{"command": fmt.Sprintf("echo %d", round)}})
		res := fmt.Sprintf("%d\n", round)
		if round == 3 {
			res = "boom\nreal error here\n[exit: 2]"
		}
		c.ev(map[string]any{"type": "tool_result", "tool": "exec", "result": res})
	}
	c.ev(map[string]any{"type": "token", "text": "All done."})
	c.ev(map[string]any{"type": "done"})
}

// A long random mix of everything the engine sends.
func TestNoShrink_RandomEventStreams(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 4} {
		for _, size := range [][2]int{{80, 24}, {44, 18}} {
			t.Run(fmt.Sprintf("seed%d_%dx%d", seed, size[0], size[1]), func(t *testing.T) {
				r := rand.New(rand.NewSource(seed))
				c := newShrinkCheck(t, size[0], size[1])
				pending := false
				for i := 0; i < 300; i++ {
					switch k := r.Intn(20); {
					case k < 5:
						c.ev(map[string]any{"type": "reasoning", "text": randText(r)})
					case k < 9:
						c.ev(map[string]any{"type": "token", "text": randMarkdown(r)})
					case k < 11:
						tool := []string{"exec", "eval"}[r.Intn(2)]
						c.ev(map[string]any{"type": "tool_call", "tool": tool, "args": map[string]any{"command": randText(r), "code": "x"}})
						pending = true
						fallthrough
					case k < 12:
						if pending {
							c.ev(map[string]any{"type": "tool_result", "tool": "exec", "result": randResult(r)})
							pending = false
						}
					case k == 12:
						c.ev(map[string]any{"type": "done"})
						c.ev(map[string]any{"type": "user", "content": randText(r)})
						c.ev(map[string]any{"type": "turn"})
					case k == 13:
						c.ev(map[string]any{"type": "user", "content": randText(r), "queued": true})
						c.ev(map[string]any{"type": "queued", "count": 1.0})
					case k == 14:
						c.ev(map[string]any{"type": "retry", "scope": []string{"turn", "request"}[r.Intn(2)], "kind": "interrupted",
							"attempt": 1.0, "delay_ms": 10.0, "reason": "the turn was interrupted", "text": "interrupted"})
						if r.Intn(2) == 0 {
							c.ev(map[string]any{"type": "retry", "kind": "recovered", "text": "recovered"})
						}
					case k == 15:
						c.ev(map[string]any{"type": "notification", "text": randText(r)})
					case k == 16:
						c.ev(map[string]any{"type": "error", "error": "provider said no: " + randText(r)})
						c.ev(map[string]any{"type": "user", "content": "try again"})
						c.ev(map[string]any{"type": "turn"})
					case k == 17:
						c.ev(map[string]any{"type": "ask", "question": "Which way? " + randText(r), "options": []any{"A", "B"}})
						c.ev(map[string]any{"type": "answered", "value": "A"})
					case k == 18:
						c.ev(map[string]any{"type": "compaction", "text": "folded 12 entries"})
					default:
						c.ev(map[string]any{"type": "recap", "note": "recapped", "detail": randText(r), "entries": 3.0})
					}
				}
			})
		}
	}
}

func randText(r *rand.Rand) string {
	words := []string{"the", "config", "loads", "layers", "in", "order", "so", "overlay", "wins", "test", "passes"}
	n := 1 + r.Intn(25)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(words[r.Intn(len(words))])
		switch r.Intn(9) {
		case 0:
			b.WriteString(". ")
		case 1:
			b.WriteString("\n")
		default:
			b.WriteString(" ")
		}
	}
	return b.String()
}

// randMarkdown streams pieces whose partial renders differ from the final
// one: fences opened and not yet closed, list items, headings, blank runs.
func randMarkdown(r *rand.Rand) string {
	return []string{"\n\n", "```go\nfunc x() {}\n", "```\n", "- item one\n- item", " two\n", "# Heading\n", "**bold", "** text ", randText(r), "| a | b |\n|---|---|\n| 1 | 2 |\n"}[r.Intn(10)]
}

func randResult(r *rand.Rand) string {
	switch r.Intn(4) {
	case 0:
		return "ERROR: no such file"
	case 1:
		return randText(r) + "\n[exit: 1]"
	case 2:
		return strings.Repeat("output line\n", 1+r.Intn(30))
	}
	return randText(r)
}
