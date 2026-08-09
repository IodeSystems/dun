package main

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// convoPane is the window onto the conversation: pre-split display rows plus a
// scroll offset. It stands in for bubbles' viewport on this one pane — the
// inspector still uses the real thing, where the content is small.
//
// The reason is SetContent. Every call there runs ReplaceAll over the whole
// string, splits it, and then measures the ANSI display width of EVERY line to
// keep longestLineWidth up to date for horizontal scrolling — which dun does
// not use. On a resume-sized conversation that scan alone was 11ms, paid on
// every refresh, which is every streamed token. Taking the rows already split
// costs nothing and skips all three.
//
// Field names match the viewport's so the call sites read the same.
type convoPane struct {
	lines   []string
	Width   int
	Height  int
	YOffset int
}

func newConvoPane(w, h int) convoPane {
	return convoPane{Width: max(1, w), Height: max(1, h)}
}

// SetLines replaces the content. It takes ownership of the slice — refresh()
// builds a fresh one each time, and nothing else holds a reference.
func (p *convoPane) SetLines(lines []string) {
	p.lines = lines
	if p.YOffset > len(p.lines)-1 {
		p.GotoBottom() // content shrank out from under the offset
	}
}

// maxYOffset is the furthest down the content can scroll: the last full window.
func (p convoPane) maxYOffset() int { return max(0, len(p.lines)-p.Height) }

func (p *convoPane) SetYOffset(n int) {
	p.YOffset = min(max(n, 0), p.maxYOffset())
}

func (p convoPane) AtBottom() bool { return p.YOffset >= p.maxYOffset() }

func (p *convoPane) GotoBottom() { p.SetYOffset(p.maxYOffset()) }

// visible is the slice of rows the pane is currently showing.
func (p convoPane) visible() []string {
	if len(p.lines) == 0 {
		return nil
	}
	top := min(max(p.YOffset, 0), len(p.lines))
	return p.lines[top:min(top+p.Height, len(p.lines))]
}

// View renders the window, padded to the pane's full size and truncated to it.
// Padding to the width is not cosmetic: Bubble Tea's renderer diffs against the
// previous frame line by line, so a row that got shorter has to be written out
// at full width or the tail of the old row stays on screen.
func (p convoPane) View() string {
	return lipgloss.NewStyle().
		Width(p.Width).MaxWidth(p.Width).
		Height(p.Height).MaxHeight(p.Height).
		Render(strings.Join(p.visible(), "\n"))
}

// wheelDelta matches the viewport's default, which is what the trace recordings
// of real sessions were made against.
const wheelDelta = 3

// Update handles the wheel and the page keys — the only input the model hands
// straight to the pane. Everything else (↑/↓, enter, tab) it keeps, because
// scrolling the conversation is entangled with the selection.
func (p convoPane) Update(msg tea.Msg) (convoPane, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.MouseMsg:
		if msg.Action != tea.MouseActionPress {
			return p, nil
		}
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			p.SetYOffset(p.YOffset - wheelDelta)
		case tea.MouseButtonWheelDown:
			p.SetYOffset(p.YOffset + wheelDelta)
		}
	case tea.KeyMsg:
		switch msg.String() {
		case "pgup":
			p.SetYOffset(p.YOffset - p.Height)
		case "pgdown":
			p.SetYOffset(p.YOffset + p.Height)
		}
	}
	return p, nil
}
