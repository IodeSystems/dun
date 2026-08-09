package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func paneWith(n, w, h int) convoPane {
	p := newConvoPane(w, h)
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	p.SetLines(lines)
	return p
}

// The frame layout depends on this exactly: convoHeight budgets the pane a
// number of rows, and View has to draw that many whatever the content is.
func TestConvoPane_ViewIsAlwaysHeightRows(t *testing.T) {
	for _, n := range []int{0, 1, 5, 100} {
		for _, h := range []int{1, 10, 40} {
			p := paneWith(n, 80, h)
			if got := lipgloss.Height(p.View()); got != h {
				t.Errorf("%d lines in a %d-row pane: View drew %d rows", n, h, got)
			}
		}
	}
}

func TestConvoPane_ScrollBounds(t *testing.T) {
	p := paneWith(100, 80, 10)
	if p.maxYOffset() != 90 {
		t.Fatalf("maxYOffset = %d, want 90", p.maxYOffset())
	}
	p.SetYOffset(-5)
	if p.YOffset != 0 {
		t.Errorf("negative offset not clamped: %d", p.YOffset)
	}
	p.SetYOffset(1000)
	if p.YOffset != 90 {
		t.Errorf("overshoot not clamped: %d", p.YOffset)
	}
	if !p.AtBottom() {
		t.Error("at maxYOffset but AtBottom is false")
	}
	p.SetYOffset(89)
	if p.AtBottom() {
		t.Error("one row short of the bottom but AtBottom is true")
	}

	// Content shorter than the pane never scrolls.
	short := paneWith(3, 80, 10)
	if short.maxYOffset() != 0 || !short.AtBottom() {
		t.Errorf("content shorter than the pane should be pinned at 0/bottom, got max=%d atBottom=%v",
			short.maxYOffset(), short.AtBottom())
	}
}

// Content shrinking out from under the offset must not leave it past the end.
func TestConvoPane_SetLinesRescuesOffset(t *testing.T) {
	p := paneWith(100, 80, 10)
	p.GotoBottom()
	p.SetLines([]string{"only", "four", "lines", "now"})
	if p.YOffset != 0 {
		t.Errorf("offset %d after the content shrank to 4 lines", p.YOffset)
	}
	if v := lipgloss.Height(p.View()); v != 10 {
		t.Errorf("View drew %d rows, want 10", v)
	}
}

func TestConvoPane_WheelAndPageKeys(t *testing.T) {
	p := paneWith(100, 80, 10)
	press := func(b tea.MouseButton) {
		p, _ = p.Update(tea.MouseMsg{Button: b, Action: tea.MouseActionPress})
	}
	press(tea.MouseButtonWheelDown)
	if p.YOffset != wheelDelta {
		t.Errorf("one wheel click down = %d rows, want %d", p.YOffset, wheelDelta)
	}
	press(tea.MouseButtonWheelUp)
	if p.YOffset != 0 {
		t.Errorf("wheel back up left the offset at %d", p.YOffset)
	}
	// A release is not a scroll.
	p, _ = p.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionRelease})
	if p.YOffset != 0 {
		t.Errorf("a wheel RELEASE scrolled to %d", p.YOffset)
	}

	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if p.YOffset != 10 {
		t.Errorf("pgdown = %d rows, want one page (10)", p.YOffset)
	}
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if p.YOffset != 0 {
		t.Errorf("pgup left the offset at %d", p.YOffset)
	}
}

// The window shows the rows the offset points at, and truncates rows wider
// than the pane rather than letting them wrap and break the row budget.
func TestConvoPane_WindowContent(t *testing.T) {
	p := paneWith(100, 80, 3)
	p.SetYOffset(42)
	got := strings.Split(p.View(), "\n")
	for i, want := range []string{"line 42", "line 43", "line 44"} {
		if strings.TrimRight(got[i], " ") != want {
			t.Errorf("row %d = %q, want %q", i, strings.TrimRight(got[i], " "), want)
		}
	}

	wide := newConvoPane(10, 2)
	wide.SetLines([]string{strings.Repeat("x", 40), "short"})
	for i, row := range strings.Split(wide.View(), "\n") {
		if w := lipgloss.Width(row); w > 10 {
			t.Errorf("row %d is %d cells wide in a 10-cell pane", i, w)
		}
	}
}
