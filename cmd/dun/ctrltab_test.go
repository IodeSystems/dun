package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// ctrl+tab types a tab, plain tab moves between zones (USER 2026-10-23).

// The spellings a terminal that reports modifiers uses for ctrl+tab become
// ctrlTabRune; kitty's shift+tab becomes the legacy CSI Z.
func TestKeyFilter_RewritesCtrlTabAndKittyShiftTab(t *testing.T) {
	ct := string(ctrlTabRune)
	cases := []struct{ name, in, want string }{
		{"ctrl+tab CSI-u (kitty)", "a\x1b[9;5ub", "a" + ct + "b"},
		{"ctrl+tab modifyOtherKeys", "a\x1b[27;5;9~b", "a" + ct + "b"},
		{"shift+tab CSI-u (kitty)", "\x1b[9;2u", "\x1b[Z"},
		{"shift+tab modifyOtherKeys", "\x1b[27;2;9~", "\x1b[Z"},
		{"a legacy tab is left alone", "a\tb", "a\tb"},
	}
	for _, c := range cases {
		if got := readAll(t, newKeyFilter(strings.NewReader(c.in))); got != c.want {
			t.Errorf("%s: want %q, got %q", c.name, c.want, got)
		}
	}
}

// keyRec records the keys bubbletea's real parser produces, and quits on q.
type keyRec struct{ keys []tea.KeyMsg }

func (k *keyRec) Init() tea.Cmd { return nil }
func (k *keyRec) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if km, ok := msg.(tea.KeyMsg); ok {
		if km.String() == "ctrl+q" {
			return k, tea.Quit
		}
		k.keys = append(k.keys, km)
	}
	return k, nil
}
func (k *keyRec) View() string { return "" }

// Through bubbletea's own parser: what the filter emits arrives as keys the
// TUI can act on — ctrl+tab as a rune the composer turns into \t, kitty's
// shift+tab as shift+tab, and a bracketed paste with a tab in it with the tab.
func TestCtrlTab_ThroughTheRealParser(t *testing.T) {
	in := "a\x1b[9;5ub" + "\x1b[9;2u" + "\x1b[200~x\ty\x1b[201~" + "\x11" // ctrl+q ends it
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := &keyRec{}
	p := tea.NewProgram(rec, tea.WithInput(newKeyFilter(strings.NewReader(in))),
		tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler(), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	var got []string
	for _, k := range rec.keys {
		got = append(got, k.String())
	}
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.w = 80
	m.input.width = 78
	sawShiftTab := false
	for _, k := range rec.keys {
		if k.String() == "shift+tab" {
			sawShiftTab = true
			continue // a zone move; checked elsewhere
		}
		m = key(m, k)
	}
	if !sawShiftTab {
		t.Errorf("kitty's shift+tab did not arrive as shift+tab: %q", got)
	}
	if v := m.input.Value(); v != "a\tbx\ty" {
		t.Errorf("composer %q, want %q (keys %q)", v, "a\tbx\ty", got)
	}
}

// ctrl+tab in the composer types a tab and leaves the focus where it is;
// plain tab still moves to the conversation.
func TestCtrlTab_InsertsATabInTheComposer(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = typeStr(m, "a")
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ctrlTabRune}})
	m = typeStr(m, "b")
	if m.focus != focusInput {
		t.Fatalf("ctrl+tab moved the focus to %d", m.focus)
	}
	if v := m.input.Value(); v != "a\tb" {
		t.Fatalf("composer %q, want %q", v, "a\tb")
	}
	m = key(m, kTab)
	if m.focus != focusConvo || m.input.Value() != "a\tb" {
		t.Fatalf("plain tab should move to the conversation and type nothing: focus=%d value=%q", m.focus, m.input.Value())
	}
}

// A pasted tab is kept in the value and drawn as spaces to the next stop, so
// the row is as wide as what the terminal will show — never a 0- or 1-cell tab
// that lets the box run past its edge.
func TestPaste_KeepsTabsAndMeasuresThem(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	m = nm.(tuiModel)
	text := "col1\tcol2\tcol3\tcol4\tcol5\tcol6\tcol7\tcol8"
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(text)})
	if v := m.input.Value(); v != text {
		t.Fatalf("paste lost its tabs: %q", v)
	}
	view := m.input.View()
	if strings.Contains(view, "\t") {
		t.Fatalf("a raw tab reached the terminal: %q", view)
	}
	if !strings.Contains(view, "col1    col2") {
		t.Errorf("a tab after 4 columns should draw to the next stop (4 spaces):\n%s", view)
	}
	for i, row := range strings.Split(view, "\n") {
		if w := ansi.StringWidth(row); w > m.w {
			t.Errorf("row %d is %d cells, wider than the %d-cell screen: %q", i, w, m.w, row)
		}
	}
	if strings.Count(view, "\n") < 1 {
		t.Errorf("the tabbed line is wider than the box and should wrap:\n%s", view)
	}
}
