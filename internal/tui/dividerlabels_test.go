package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The divider is one dim centered line naming the panes; the focused name is
// bracketed so it reads without colour (owner, 2026-11-17).
func TestDividerLabels_EachFocus(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.w = 80
	for _, tc := range []struct {
		focus int
		want  string
	}{
		{focusInput, "─[input]─ messages ─"},
		{focusConvo, "─ input ─[messages]─"},
	} {
		m.focus = tc.focus
		d, _ := m.baseDivider()
		got := ansi.Strip(d)
		if !strings.HasPrefix(got, tc.want) || ansi.StringWidth(got) != 80 || strings.Contains(got, "jobs") {
			t.Errorf("focus %d: %q", tc.focus, got)
		}
		t.Logf("%s", got)
	}
	m.agents = []agentRow{{id: 1, state: "running"}}
	m.focus = focusActivity
	d, _ := m.baseDivider()
	if got := ansi.Strip(d); !strings.HasPrefix(got, "─ input ─ messages ─[jobs]─") || ansi.StringWidth(got) != 80 {
		t.Errorf("activity focus: %q", got)
	} else {
		t.Logf("%s", got)
	}
	// The focused name is accent and bold; the others are dim.
	if !strings.Contains(d, stEdge.Bold(true).Render("[jobs]")) || !strings.Contains(d, StDim.Render(" input ")) {
		t.Errorf("styles: %q", d)
	}
	m.focus, m.asking = focusConvo, true // a question has the keys
	d, _ = m.baseDivider()
	if got := ansi.Strip(d); !strings.HasPrefix(got, "─[input]─") {
		t.Errorf("asking: %q", got)
	}
}

func TestDividerLabels_NoJobsWhenStripEmpty(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.w = 80
	d, _ := m.baseDivider()
	if strings.Contains(ansi.Strip(d), "jobs") {
		t.Errorf("empty strip names jobs: %q", ansi.Strip(d))
	}
}

func TestDividerLabels_NarrowWidths(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.agents = []agentRow{{id: 1, state: "running"}}
	m.focus = focusConvo
	for _, tc := range []struct {
		w    int
		want string // prefix; "" = a plain line
	}{
		{80, "─ input ─[messages]─ jobs ─"},
		{30, "─ input ─[messages]─ jobs ─"},
		{25, "─ in ─[msg]─ jobs ─"},
		{21, "─ in ─[msg]─ jobs ─"},
		{20, "─ in ─[msg]─ jobs ─"},
		{19, ""},
		{15, ""},
		{2, ""},
	} {
		m.w = tc.w
		d, _ := m.baseDivider()
		got := ansi.Strip(d)
		if ansi.StringWidth(got) != max(tc.w, 2) || strings.Contains(got, "\n") {
			t.Errorf("w=%d: width %d: %q", tc.w, ansi.StringWidth(got), got)
		}
		if tc.want == "" && strings.Trim(got, "─") != "" {
			t.Errorf("w=%d: want a plain line: %q", tc.w, got)
		}
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("w=%d: %q, want prefix %q", tc.w, got, tc.want)
		}
	}
}

func TestDividerLabels_CueKeepsRoom(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.scrollPinned = false
	m.focus = focusConvo
	m.w = 80
	d, spans := m.baseDivider()
	got := ansi.Strip(d)
	if !strings.HasPrefix(got, "─ input ─[messages]─") || !strings.HasSuffix(got, " ↓ newest · ctrl+end ─") || ansi.StringWidth(got) != 80 {
		t.Errorf("%q", got)
	}
	t.Logf("%s", got)
	if id, ok := m.dividerHit(79); !ok || id != divCue || spans[len(spans)-1].id != divCue {
		t.Errorf("the cue is not clickable: %+v", spans)
	}
	// Narrower: the cue stays, the names give way, nothing overlaps.
	for w := 20; w < 100; w++ {
		m.w = w
		d, spans := m.baseDivider()
		got := ansi.Strip(d)
		if ansi.StringWidth(got) != w || !strings.Contains(got, "↓ newest") {
			t.Fatalf("w=%d: %q", w, got)
		}
		for i := 1; i < len(spans); i++ {
			if spans[i].x0 < spans[i-1].x1 {
				t.Fatalf("w=%d: overlap %+v", w, spans)
			}
		}
	}
}

// A click on a name focuses that pane; one on the cue still goes to the
// newest. A click in the conversation body still takes no focus.
func TestDividerLabels_ClickFocuses(t *testing.T) {
	v := tallConvo(t)
	m := v.m
	divY := m.convoTopRow() + m.convoHeight()
	_, spans := m.baseDivider()
	if m.focus != focusInput {
		t.Fatal("test needs input focus")
	}
	for _, sp := range spans {
		if sp.id == focusConvo {
			m = click(m, sp.x0+1, divY)
		}
	}
	if m.focus != focusConvo {
		t.Fatalf("clicking messages: focus %d", m.focus)
	}
	_, spans = m.baseDivider()
	for _, sp := range spans {
		if sp.id == focusInput {
			m = click(m, sp.x0, divY)
		}
	}
	if m.focus != focusInput {
		t.Fatalf("clicking input: focus %d", m.focus)
	}
	m = click(m, 10, divY-1)
	if m.focus != focusInput {
		t.Errorf("a click in the body moved focus to %d", m.focus)
	}
}

// A pushed screen names its panes on its divider and a click focuses one.
func TestDividerLabels_PaneScreen(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.w, m.h = 80, 24
	s := mcpScreen()
	if got := ansi.Strip(s.paneDivider(80)); !strings.HasPrefix(got, "─[servers]─ log ─") || ansi.StringWidth(got) != 80 {
		t.Errorf("%q", got)
	}
	s.focus = 2
	got := ansi.Strip(s.paneDivider(80))
	t.Logf("%s", got)
	if !strings.HasPrefix(got, "─ servers ─[log]─") {
		t.Errorf("%q", got)
	}
	s.valid = func(*tuiModel) bool { return true }
	m.scr = s
	hs := s.heights(&m, 80, m.screenRows())
	// head rows, then the divider below pane 0 and the one below pane 1.
	y := len(strings.Split(m.headView(), "\n")) + hs[0] + 1 + hs[1]
	_, spans := renderDivider(80, s.labels(), "")
	if !m.screenClick(tea.Mouse{Button: tea.MouseLeft, X: spans[0].x0, Y: y}) || s.focus != 1 {
		t.Errorf("clicking servers: focus %d", s.focus)
	}
	if m.screenClick(tea.Mouse{Button: tea.MouseLeft, X: 79, Y: y}) {
		t.Error("a click past the names took")
	}
}

// Names must not move when focus changes: each label's columns are the same
// in every focus state, at wide and narrow widths, with and without the cue.
func TestDividerLabels_NamesDoNotMove(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.agents = []agentRow{{id: 1, state: "running"}}
	for _, w := range []int{80, 30, 25} {
		for _, scrolled := range []bool{false, true} {
			if scrolled && w == 25 {
				continue // too narrow for every name beside the cue: only the focused one shows
			}
			m.w, m.scrollPinned = w, !scrolled
			var base []divSpan
			for _, f := range []int{focusInput, focusConvo, focusActivity} {
				m.focus = f
				_, spans := m.baseDivider()
				if base == nil {
					base = spans
				}
				if len(spans) != len(base) {
					t.Fatalf("w=%d focus %d: %d spans, want %d", w, f, len(spans), len(base))
				}
				for i := range spans {
					if spans[i] != base[i] {
						t.Errorf("w=%d scrolled=%v focus %d: span %d is %+v, was %+v", w, scrolled, f, i, spans[i], base[i])
					}
				}
			}
		}
	}
	// The pushed screen too.
	s := mcpScreen()
	_, a := renderDivider(80, s.labels(), "")
	s.focus = 2
	_, b := renderDivider(80, s.labels(), "")
	if len(a) != len(b) || a[0] != b[0] || a[1] != b[1] {
		t.Errorf("pane screen names moved: %+v then %+v", a, b)
	}
}

// divClick clicks the base divider's label for focus zone id.
func divClick(m tuiModel, id int) tuiModel {
	y := m.convoTopRow() + m.convoHeight() + m.dividerGap()
	_, spans := m.baseDivider()
	for _, sp := range spans {
		if sp.id == id {
			return click(m, sp.x0+1, y)
		}
	}
	return m
}

// A click on a name does nothing while something else has the keys: a
// confirm prompt, a form, the palette.
func TestDividerLabels_ClickIgnoredUnderModals(t *testing.T) {
	for name, set := range map[string]func(m *tuiModel){
		"confirm":   func(m *tuiModel) { m.confirm.q = "sure?" },
		"provider":  func(m *tuiModel) { m.providerForm = true },
		"login":     func(m *tuiModel) { m.loginPicking = true },
		"palette":   func(m *tuiModel) { m.input.SetValue("/") },
		"searching": func(m *tuiModel) { m.searching = true },
	} {
		v := tallConvo(t)
		m := v.m
		set(&m)
		before := m.focus
		m = divClick(m, focusConvo)
		if m.focus != before {
			t.Errorf("%s: focus moved %d -> %d", name, before, m.focus)
		}
	}
}

// Clicking the name that already has focus changes nothing: the selection
// and the activity scope stay.
func TestDividerLabels_ClickFocusedKeepsState(t *testing.T) {
	v := tallConvo(t)
	m := v.m
	m.setFocus(focusConvo)
	m.sel = 0
	m = divClick(m, focusConvo)
	if m.focus != focusConvo || m.sel != 0 {
		t.Errorf("sel %d focus %d: the click reset them", m.sel, m.focus)
	}
}

// The cue works at widths between 20 and 41 (the bare cue) and when the
// divider is one row lower (dividerGap).
func TestDividerLabels_CueClickNarrowAndGap(t *testing.T) {
	for _, tc := range []struct {
		w   int
		gap bool
	}{{30, false}, {20, false}, {80, true}, {30, true}} {
		v := tallConvo(t)
		v.resize(tc.w, 15)
		v.setScrollPin(false)
		v.setYOffset(2)
		v.m.refresh()
		if tc.gap {
			v.m.modelPicking, v.m.rolesPicking = true, true
			v.m.refresh()
			if v.m.dividerGap() != 1 {
				t.Fatal("test needs dividerGap 1")
			}
		}
		m := v.m
		y := m.convoTopRow() + m.convoHeight() + m.dividerGap()
		m = click(m, tc.w-3, y)
		if !m.scrollPinned {
			t.Errorf("w=%d gap=%v: the cue click did not go to the newest", tc.w, tc.gap)
		}
	}
}

// An emptied strip with focus still on it names a pane as focused.
func TestDividerLabels_EmptiedStripStillMarksFocus(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.focus = focusActivity
	for _, w := range []int{80, 25, 20} {
		m.w = w
		d, _ := m.baseDivider()
		if got := ansi.Strip(d); !strings.Contains(got, "[") {
			t.Errorf("w=%d: no focused name: %q", w, got)
		}
	}
}
