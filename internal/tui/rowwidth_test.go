package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Width rules: prose fills the pane (up to the reading cap), collapsed rows
// run to the edge and are cut once, a URL is not split when it fits a row.

const rwURL = "https://github.com/example/some-repo/blob/main/internal/workflow/some_long_file_name.go"

func rwScreen(t *testing.T, w int, build func(m tuiModel) tuiModel) []string {
	t.Helper()
	var mm tea.Model = newTUIModel(&dunProc{}, "/ws")
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: w, Height: 60})
	m := build(mm.(tuiModel))
	m.refresh()
	var out []string
	for _, l := range strings.Split(ansi.Strip(m.View().Content), "\n") {
		out = append(out, strings.TrimRight(l, " "))
	}
	return out
}

func rwMax(rows []string, contains string) int {
	n := 0
	for _, r := range rows {
		if strings.Contains(r, contains) {
			n = max(n, ansi.StringWidth(r))
		}
	}
	return n
}

func TestRowWidth_UserProseUsesPane(t *testing.T) {
	old := readingCap
	readingCap = readingCapFrom("off")
	defer func() { readingCap = old }()
	text := strings.Repeat("alpha bravo charlie delta ", 20) + rwURL + " tail words after the link"
	for _, w := range []int{80, 132, 200} {
		rows := rwScreen(t, w, func(m tuiModel) tuiModel { m.appendUser(text); return m })
		want := w - 4
		if got := rwMax(rows, "alpha"); got < want-12 {
			t.Errorf("w=%d: widest user row %d, want near %d\n%s", w, got, want, strings.Join(rows, "\n"))
		}
		for _, r := range rows {
			if ansi.StringWidth(r) > w {
				t.Errorf("w=%d: row %d wide", w, ansi.StringWidth(r))
			}
		}
		joined := strings.Join(rows, "\n")
		if !strings.Contains(joined, rwURL) && w >= 132 {
			t.Errorf("w=%d: URL split:\n%s", w, joined)
		}
	}
}

// The reading cap stays the default: 100 columns, DUN_READING_WIDTH sets it.
func TestRowWidth_ReadingCapDefault(t *testing.T) {
	if readingCap != bandCap {
		t.Fatalf("default cap %d, want %d", readingCap, bandCap)
	}
	for in, want := range map[string]int{"": bandCap, "120": 120, "5": 20, "off": 1 << 20, "0": 1 << 20} {
		if got := readingCapFrom(in); got != want {
			t.Errorf("DUN_READING_WIDTH=%q: %d, want %d", in, got, want)
		}
	}
	text := strings.Repeat("alpha bravo charlie delta ", 20)
	rows := rwScreen(t, 200, func(m tuiModel) tuiModel { m.appendUser(text); return m })
	// The cap is for assistant prose; a typed message wraps at the composer's
	// width (typedrow_test.go), so its rows pass the cap.
	if got := rwMax(rows, "alpha"); got <= bandCap {
		t.Errorf("user row %d wide at 200 columns, want past the cap %d", got, bandCap)
	}
}

func TestRowWidth_CollapsedRowsReachEdge(t *testing.T) {
	long := "dund restarted: the shared MCP servers this session used through dund are stopped. A call to one fails until this session is next idle, when dun re-attaches them, and then everything works again as before."
	cmd := "cd ~/src/proto-REE-97264 && gh pr view 2196 -R Example/proto --json title,body,comments,reviews --jq '.comments[].body' | head -200"
	think := strings.Repeat("Now I understand the review comments and how callers keep working with it, in detail and at length. ", 3)
	for _, w := range []int{80, 132, 200} {
		rows := rwScreen(t, w, func(m tuiModel) tuiModel {
			m = m.feed(evMsg{"type": "notification", "kind": "info", "text": long})
			m = m.feed(evMsg{"type": "reasoning", "text": think})
			m = m.feed(evMsg{"type": "tool_call", "tool": "exec", "args": map[string]any{"command": cmd}})
			m = m.feed(evMsg{"type": "tool_result", "tool": "exec", "result": "=== someone COMMENTED on this pull request with a long first line that goes on and on and on and on and on and on\nsecond\n"})
			m = m.feed(evMsg{"type": "reasoning", "text": think})
			return m
		})
		joined := strings.Join(rows, "\n")
		if n := strings.Count(joined, "dund restarted"); n != 1 {
			t.Errorf("w=%d: notice wrapped or cut early:\n%s", w, joined)
		}
		if got := rwMax(rows, "dund restarted"); got < w-6 {
			t.Errorf("w=%d: notice row %d, want near %d", w, got, w)
		}
		if got := rwMax(rows, "gh pr view"); got < w-6 {
			t.Errorf("w=%d: tool row %d, want near %d\n%s", w, got, w, joined)
		}
		if got := rwMax(rows, "✻ Now"); got < w-6 {
			t.Errorf("w=%d: thinking row %d, want near %d\n%s", w, got, w, joined)
		}
	}
}

// A notice made at one width is cut again at the next: once, at the edge.
func TestRowWidth_NoticeRecutOnResize(t *testing.T) {
	long := "dund restarted: " + strings.Repeat("the shared MCP servers this session used are stopped. ", 6)
	var mm tea.Model = newTUIModel(&dunProc{}, "/ws")
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	m := mm.(tuiModel).feed(evMsg{"type": "notification", "kind": "info", "text": long})
	for _, w := range []int{132, 80, 200} {
		mm, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: 60})
		m = mm.(tuiModel)
		m.refresh()
		rows := strings.Split(ansi.Strip(m.View().Content), "\n")
		n := 0
		for _, r := range rows {
			if strings.Contains(r, "…") && strings.Contains(r, "stopped") {
				n++
				if got := ansi.StringWidth(strings.TrimRight(r, " ")); got < w-3 {
					t.Errorf("w=%d: notice row %d wide", w, got)
				}
			}
		}
		if n != 1 || strings.Count(strings.Join(rows, "\n"), "dund restarted") != 1 {
			t.Errorf("w=%d: want one cut row:\n%s", w, strings.Join(rows, "\n"))
		}
	}
}

// A URL that wraps is one OSC 8 link on every row (same id, whole URL),
// widths ignore the escapes, and a drag across the wrap copies it unbroken.
func TestRowWidth_LinkAcrossWrap(t *testing.T) {
	old := hyperlinksOn
	hyperlinksOn = true
	defer func() { hyperlinksOn = old }()
	long := "https://example.com/" + strings.Repeat("segment/", 12) + "end.go"
	for _, w := range []int{40, 80} {
		var mm tea.Model = newTUIModel(&dunProc{}, "/ws")
		mm, _ = mm.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		m := mm.(tuiModel)
		m.appendUser("see " + long + " now")
		m.refresh()
		var ids []string
		var urls []string
		nrows := 0
		for _, l := range m.vp.lines {
			if !strings.Contains(l, "\x1b]8;id=") {
				continue
			}
			nrows++
			for _, mt := range osc8Re.FindAllStringSubmatch(l, -1) {
				if mt[2] != "" {
					ids, urls = append(ids, mt[1]), append(urls, mt[2])
				}
			}
			if sw := ansi.StringWidth(l); sw > w {
				t.Errorf("w=%d: row %d wide with escapes", w, sw)
			}
		}
		if len(long) > w-4 && nrows < 2 {
			t.Fatalf("w=%d: url on %d rows, want it wrapped", w, nrows)
		}
		for i := range ids {
			if ids[i] != ids[0] || urls[i] != long {
				t.Errorf("w=%d: segment %d id %q url %q", w, i, ids[i], urls[i])
			}
		}
		last := len(m.vp.lines) - 1
		text := selText(m.vp.lines, cellPos{line: 0, col: 0}, cellPos{line: last, col: 500}, m.gutterW())
		if !strings.Contains(text, long) {
			t.Errorf("w=%d: drag copy split the url:\n%s", w, text)
		}
		if strings.Contains(stripLinks(m.View().Content), "\x1b]8") {
			t.Error("stripLinks left an escape")
		}
	}
	hyperlinksOn = false
	var mm tea.Model = newTUIModel(&dunProc{}, "/ws")
	mm, _ = mm.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m := mm.(tuiModel)
	m.appendUser("see " + long)
	if strings.Contains(m.View().Content, "\x1b]8") {
		t.Error("escapes reach the terminal with hyperlinks off")
	}
}

func TestHyperlinkDetect(t *testing.T) {
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	for _, c := range []struct {
		env  func(string) string
		want bool
	}{
		{env("TERM_PROGRAM", "iTerm.app"), true},
		{env("TERM_PROGRAM", "Apple_Terminal"), false},
		{env("TERM_PROGRAM", "iTerm.app", "TMUX", "/x"), false},
		{env("TERM_PROGRAM", "Apple_Terminal", "DUN_HYPERLINKS", "on"), true},
		{env("TERM_PROGRAM", "WezTerm", "DUN_HYPERLINKS", "off"), false},
		{env("TERM", "xterm-256color"), false},
		{env("KITTY_WINDOW_ID", "3"), true},
	} {
		if got := hyperlinkDetect(c.env); got != c.want {
			t.Errorf("%v: %v", c.env("TERM_PROGRAM"), got)
		}
	}
}
