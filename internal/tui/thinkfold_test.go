package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// thinkfold_test.go: a thought is one line (thinking.go), and ctrl+t shows all
// of it.

func TestThinkHeadline(t *testing.T) {
	for _, c := range []struct {
		name, text string
		live       bool
		want       string
	}{
		{"empty", "", true, ""},
		{"blank", " \n\n", false, ""},
		{"live: the latest step", "# Thinking: read the config\nmore\n# Thinking: find the bug\nbody", true, "find the bug"},
		{"live: a step still arriving", "# Thinking: read the config\n# Thinking: find the b", true, "find the b"},
		{"live: half a keyword is not a heading yet", "# Thinking: read the config\n# Thin", true, "read the config"},
		{"live: a bare marker is not a heading", "# Thinking: read the config\n# Thinking:", true, "read the config"},
		{"done: the last summary", "# Thinking: a\n# Thinking: b\n# Summary: found it\n", false, "found it"},
		{"done: the last of two summaries", "# Summary: first\n# Thinking: c\n# Summary: second", false, "second"},
		{"done, no summary: the last step", "# Thinking: a\nbody\n# Thinking: b\nbody", false, "b"},
		{"live, only a summary so far: the first line", "# Summary: done\n", true, "Summary: done"},
		{"no headings: the first line", "\n\n  Let me look at this.\nThen more.", false, "Let me look at this."},
		{"no headings, live", "Let me look", true, "Let me look"},
		{"case and spaces", "   #   THINKING :  Loud  \n#summary:quiet", false, "quiet"},
		{"case and spaces, live", "   #thinking:  Loud  ", true, "Loud"},
		{"more hashes", "## Thinking: deeper", true, "deeper"},
		{"a heading mid-line is not one", "see # Thinking: x\nfirst", true, "see # Thinking: x"},
		{"crlf", "# Thinking: a\r\n# Summary: b\r\n", false, "b"},
		{"inside a fence it still counts", "```sh\n# Thinking: in a script\n```\nafter", true, "in a script"},
		{"a markdown title as the fallback", "# Plan\nbody", false, "Plan"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := thinkHeadline(c.text, c.live); got != c.want {
				t.Errorf("thinkHeadline(%q, %v) = %q, want %q", c.text, c.live, got, c.want)
			}
		})
	}
}

// ctrlT is the shortcut as the terminal sends it.
func ctrlT(m tuiModel) tuiModel { return key(m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}) }

func (v *vtui) tick() {
	nm, _ := v.m.Update(renderTickMsg{})
	v.m = nm.(tuiModel)
}

func reason(v *vtui, text string) {
	v.event(map[string]any{"type": "reasoning", "text": text})
	v.tick()
}

// A live thought is one row, its heading swapped in place; finished, the
// summary, on the same row. ctrl+t shows the text and brings the line back.
func TestThinkFold_LiveFinishedAndToggle(t *testing.T) {
	for _, w := range []int{80, 30} {
		t.Run(fmt.Sprintf("w%d", w), func(t *testing.T) {
			v := newVtui(w, 24)
			v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
			v.send("go")
			reason(v, "# Thinking: read config\nthe body of step one\n")
			i := thinkIdx(v.m)
			if i < 0 {
				t.Fatalf("no thought block:\n%s", v.view())
			}
			scr := v.view()
			if !strings.Contains(scr, "✻ read config") || !strings.Contains(scr, "…") || strings.Contains(scr, "body of step one") {
				t.Fatalf("live, folded:\n%s", scr)
			}
			h := v.m.frame.blockH[i]
			if h != 1 {
				t.Errorf("a folded thought is %d rows, want 1", h)
			}
			reason(v, "# Thinking: find the bug\nstep two body\n")
			scr = v.view()
			if strings.Contains(scr, "read config") || !strings.Contains(scr, "✻ find the") {
				t.Errorf("the heading should swap in place:\n%s", scr)
			}
			if v.m.frame.blockH[i] != 1 {
				t.Errorf("height changed while streaming: %d", v.m.frame.blockH[i])
			}
			reason(v, "# Summary: found it\n")
			v.event(map[string]any{"type": "token", "text": "reply"})
			v.tick()
			scr = v.view()
			if !strings.Contains(scr, "✻ found it") || strings.Contains(scr, "find the bug") || strings.Contains(scr, "…)") {
				t.Errorf("finished, folded:\n%s", scr)
			}
			if v.m.frame.blockH[i] != 1 {
				t.Errorf("the end of the thought changed its height: %d", v.m.frame.blockH[i])
			}
			// The full text stays what search and the dump read.
			if c := stripANSI(v.m.convo[i].collapsed); !strings.Contains(c, "step two body") || !strings.Contains(c, "# Summary: found it") {
				t.Errorf("folded, the plain form lost the text: %q", c)
			}
			for _, row := range strings.Split(scr, "\n") {
				if n := len([]rune(stripANSI(row))); n > w {
					t.Errorf("row of %d columns on a %d-column screen: %q", n, w, row)
				}
			}

			v.m = ctrlT(v.m)
			scr = v.view()
			if !strings.Contains(scr, "step two body") || !strings.Contains(scr, "thought for") {
				t.Errorf("ctrl+t should show the thought whole:\n%s", scr)
			}
			v.m = ctrlT(v.m)
			scr = v.view()
			if strings.Contains(scr, "step two body") || !strings.Contains(scr, "✻ found it") {
				t.Errorf("ctrl+t again should fold it:\n%s", scr)
			}
		})
	}
}

// Thoughts on screen and in a tool run both follow the toggle, and the row
// the view had at its top stays there.
func TestThinkFold_ToggleCoversRunsAndKeepsTheView(t *testing.T) {
	v := newVtui(30, 12)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	for q := 0; q < 3; q++ {
		v.send(fmt.Sprintf("question %d", q))
		reason(v, fmt.Sprintf("# Thinking: step %d\nbody line a of %d\nbody line b of %d\n# Summary: sum %d\n", q, q, q, q))
		call(v, "echo hi", "hi\n")
		v.event(map[string]any{"type": "token", "text": "answer"})
		v.event(map[string]any{"type": "done"})
	}
	v.tick()
	for i, e := range v.m.convo {
		if e.run != nil {
			for _, it := range e.run.items {
				if it.think != nil && it.think.full {
					t.Fatalf("run %d: a thought starts unfolded", i)
				}
			}
		}
	}
	// Scroll up a little, read the top row, flip.
	v.setScrollPin(false)
	for i, l := range v.m.vp.lines { // a message's row: the same before and after
		if strings.Contains(stripANSI(l), "question 1") {
			v.setYOffset(i)
			break
		}
	}
	top := strings.TrimRight(stripANSI(v.m.vp.lines[v.m.vp.YOffset]), " ")
	v.m = ctrlT(v.m)
	if !v.m.thinkFull {
		t.Fatal("ctrl+t did not unfold")
	}
	full := 0
	for _, e := range v.m.convo {
		if e.run != nil {
			for _, it := range e.run.items {
				if it.think != nil && it.think.full {
					full++
				}
			}
		}
		if e.think != nil && e.think.full {
			full++
		}
	}
	if full != 3 {
		t.Errorf("%d thoughts unfolded, want 3", full)
	}
	if got := strings.TrimRight(stripANSI(v.m.vp.lines[v.m.vp.YOffset]), " "); got != top {
		t.Errorf("the view's top row changed: %q → %q", top, got)
	}
	v.m = ctrlT(v.m)
	if v.m.thinkFull {
		t.Error("ctrl+t again did not fold")
	}
	// A thought that arrives while unfolded is born unfolded.
	v.m = ctrlT(v.m)
	v.send("again")
	reason(v, "# Thinking: new\nnew body\n")
	if i := thinkIdx(v.m); i < 0 || !v.m.convo[i].think.full {
		t.Error("a new thought should follow the toggle")
	}
}

// Enter on a selected thought opens the pager, folded or not, with the text.
func TestThinkFold_EnterOpensThePager(t *testing.T) {
	for _, full := range []bool{false, true} {
		v := newVtui(30, 24)
		v.m.thinkFull = full
		v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
		v.send("go")
		reason(v, "# Thinking: a\nthe whole body\n# Summary: b\n")
		v.event(map[string]any{"type": "token", "text": "reply"})
		i := thinkIdx(v.m)
		v.m.sel, v.m.focus = i, focusConvo
		v.m = key(v.m, kEnter)
		if !v.m.paging || !strings.Contains(v.view(), "the whole body") {
			t.Errorf("full=%v: enter should open the pager on the thought:\n%s", full, v.view())
		}
	}
}

// History comes back folded, a thought with no text still reads sensibly, and
// so does a thought that never used a heading.
func TestThinkFold_ReplayAndPlainText(t *testing.T) {
	v := newVtui(30, 24)
	v.event(map[string]any{"type": "history", "items": []any{
		map[string]any{"kind": "user", "content": "hi"},
		map[string]any{"kind": "reasoning", "content": "# Thinking: a\nbody text\n# Summary: the old answer\n"},
		map[string]any{"kind": "assistant", "content": "hello"},
		map[string]any{"kind": "user", "content": "again"},
		map[string]any{"kind": "reasoning", "content": "Plain musing about the thing.\nSecond line."},
		map[string]any{"kind": "assistant", "content": "ok"},
	}})
	scr := v.view()
	if !strings.Contains(scr, "✻ the old answer") || !strings.Contains(scr, "✻ Plain musing") || strings.Contains(scr, "body text") || strings.Contains(scr, "Second line") {
		t.Errorf("history should come back folded:\n%s", scr)
	}

	w := newVtui(30, 24)
	w.send("go")
	w.event(map[string]any{"type": "reasoning", "text": ""})
	w.tick()
	if scr := w.view(); !strings.Contains(scr, "✻ thinking") || strings.Contains(scr, "0 words") {
		t.Errorf("an empty live thought:\n%s", scr)
	}
	w.event(map[string]any{"type": "token", "text": "reply"})
	w.tick()
	if scr := w.view(); !strings.Contains(scr, "✻ thought") || strings.Contains(scr, "0 words") {
		t.Errorf("an empty finished thought:\n%s", scr)
	}
	w.m = ctrlT(w.m)
	_ = w.view() // unfolded, empty: draws without panicking
}
