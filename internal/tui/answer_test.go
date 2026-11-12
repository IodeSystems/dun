package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/iodesystems/bubbleteandmd"
	"github.com/iodesystems/bubbleteandmd/prompt"
)

// streamAnswer streams src into a model the way tokens arrive (step bytes a
// frame) and finishes it, at a terminal w columns wide; it returns the
// finished entry and the pane width it rendered at.
func streamAnswer(t testing.TB, src string, w, step int) (convoEntry, int) {
	t.Helper()
	m := newTUIModel(&dunProc{}, "/ws")
	m.mdWidth = w
	avail := w - 1
	for i := step; i < len(src); i += step {
		m.cur = src[:i]
		m.curAnswer(avail)
	}
	m.cur = src
	m.curAnswer(avail)
	m.vp.Width = w
	e := m.finishAnswer(src)
	return e, avail
}

// The streamed reply ends as a whole render of its source would draw it:
// Final through the Stream that took every frame equals bubbleteandmd's
// Render of the sanitized source, at the same prose column and pane.
func TestAnswer_StreamFinalIsRender(t *testing.T) {
	src := wideAnswer + "\nThen:\n\n" + prompt.Examples()["steps"] + "\n\nAnd **done** with `code`.\n"
	for _, w := range []int{60, 100, 180} {
		e, avail := streamAnswer(t, src, w, 7)
		m := tuiModel{mdWidth: w}
		r, err := bubbleteandmd.NewRenderer(mdStyleConfig(), m.proseW())
		if err != nil {
			t.Fatal(err)
		}
		r.WideBlocks(avail)
		want, err := r.Render(bubbleteandmd.Sanitize(demoteStepHeads(src)))
		if err != nil {
			t.Fatal(err)
		}
		if got := e.collapsed; got != strings.Trim(want, "\n") {
			t.Errorf("w=%d: Final differs from Render:\n got %q\nwant %q", w, got, want)
		}
	}
}

// A reply in the forms prompt.Format teaches renders as forms: no directive
// marker reaches the screen.
func TestAnswer_FormsNotMarkers(t *testing.T) {
	for name, ex := range prompt.Examples() {
		e, _ := streamAnswer(t, "Intro.\n\n"+ex+"\n", 100, 5)
		plain := ansi.Strip(e.collapsed)
		if strings.Contains(plain, "[!") || strings.Contains(plain, "####") {
			t.Errorf("%s: a raw marker on screen:\n%s", name, plain)
		}
	}
}

// answer20k is a ~20k-character reply: prose, lists, code and tables.
func answer20k() string {
	var b strings.Builder
	for i := 0; b.Len() < 20000; i++ {
		fmt.Fprintf(&b, "## Part %d\n\nThe **%d**th part says what `f%d` does, at some length so the prose wraps over a few rows of the pane, and links [a page](https://example.com/%d).\n\n", i, i, i, i)
		fmt.Fprintf(&b, "- one item\n- two items, with `code`\n- three\n\n")
		fmt.Fprintf(&b, "```go\nfunc f%d(x int) int {\n\treturn x * %d // %s\n}\n```\n\n", i, i, strings.Repeat("y", 20*(i%8)))
		if i%3 == 0 {
			fmt.Fprintf(&b, "| name | value | note |\n|---|---|---|\n")
			for j := 0; j < 10; j++ {
				fmt.Fprintf(&b, "| n%d | %d | %s |\n", j, i*j, strings.Repeat("z", 10*(j%6)))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// BenchmarkAnswerStream20k is one ~20k-character reply streamed 20 bytes a
// frame through the Stream and finished, at a 100-column terminal.
func BenchmarkAnswerStream20k(b *testing.B) {
	src := answer20k()
	keep := answerCache
	defer func() { answerCache = keep }()
	for b.Loop() {
		answerCache = bubbleteandmd.NewCache(512) // a reply not seen before
		streamAnswer(b, src, 100, 20)
	}
}
