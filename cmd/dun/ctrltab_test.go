package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// ctrl+tab types a tab, plain tab moves between zones (USER 2026-10-23).

// keyRec records the messages Bubble Tea's real input decoder produces, and
// quits on ctrl+q.
type keyRec struct{ msgs []tea.Msg }

func (k *keyRec) Init() tea.Cmd { return nil }
func (k *keyRec) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+q" {
			return k, tea.Quit
		}
		k.msgs = append(k.msgs, msg)
	case tea.PasteMsg:
		k.msgs = append(k.msgs, msg)
	}
	return k, nil
}
func (k *keyRec) View() tea.View { return tea.NewView("") }

// decodeKeys runs raw terminal bytes through Bubble Tea's own decoder.
func decodeKeys(t *testing.T, in string) []tea.Msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := &keyRec{}
	p := tea.NewProgram(rec, tea.WithInput(strings.NewReader(in+"\x11")), // ctrl+q ends it
		tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler(), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	return rec.msgs
}

// The spellings terminals use for ctrl+tab, shift+tab and shift+enter, through
// Bubble Tea's real decoder: they arrive as keys the TUI acts on — ctrl+tab
// and a bracketed paste's tab as tabs in the composer, kitty's shift+tab as
// shift+tab, and every shift+enter as a newline rather than a send.
func TestKeys_ThroughTheRealDecoder(t *testing.T) {
	in := "a\x1b[9;5ub" + // ctrl+tab, kitty
		"\x1b[27;5;9~c" + // ctrl+tab, xterm modifyOtherKeys
		"\x1b[9;2u" + "\x1b[Z" + // shift+tab, kitty and legacy
		"\x1b[200~x\ty\x1b[201~" + // a bracketed paste with a tab in it
		"\x1bOM" + "\x1b[13;2u" + "\x1b[27;2;13~" + "d" // shift+enter: SS3 M, kitty, modifyOtherKeys
	msgs := decodeKeys(t, in)
	var got []string
	for _, k := range msgs {
		got = append(got, fmt.Sprintf("%T %q", k, k))
	}
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.w = 80
	m.input.width = 78
	shiftTabs := 0
	for _, k := range msgs {
		if k, ok := k.(tea.KeyPressMsg); ok && k.String() == "shift+tab" {
			shiftTabs++
			continue // a zone move; checked elsewhere
		}
		m = key(m, k)
	}
	if shiftTabs != 2 {
		t.Errorf("want 2 shift+tabs (kitty and legacy), got %d: %q", shiftTabs, got)
	}
	if want := "a\tb\tcx\ty\n\n\nd"; m.input.Value() != want {
		t.Errorf("composer %q, want %q (msgs %q)", m.input.Value(), want, got)
	}
	if len(m.convo) != 0 {
		t.Errorf("a shift+enter sent the message: %d convo entries", len(m.convo))
	}
}

// ctrl+tab in the composer types a tab and leaves the focus where it is;
// plain tab still moves to the conversation.
func TestCtrlTab_InsertsATabInTheComposer(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = typeStr(m, "a")
	m = key(m, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModCtrl})
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
	m = key(m, tea.PasteMsg{Content: string([]rune(text))})
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
