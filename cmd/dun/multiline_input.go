package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	tea "github.com/charmbracelet/bubbletea"
)

// multilineInput is a 4-line word-wrapped text box for composing messages.
//
// It replaces the single-line bubbles textinput for the main user input pane.
// The buffer is stored as a flat string (rune slice).  Word wrapping is
// computed at render time from the buffer + available width, producing a
// virtual display grid.  The cursor is a flat rune offset into the buffer;
// display coordinates (line, col) are derived from the offset via the wrapped
// grid.  This avoids the fragility of editing on a wrapped grid and then
// trying to reconstruct logical lines.
//
// Enter submits (handled by the caller).
// Down/Right at the end of a wrapped line auto-inserts a newline.
// Up/Down navigate within the wrapped display.  Ctrl+Up/Ctrl+Down navigate
// history (handled by the caller).
type multilineInput struct {
	buf         []rune // flat text buffer
	cursor      int    // flat rune offset into buf
	width       int    // available columns for wrapping (set on WindowSizeMsg)
	maxLines    int    // display height (always 4)
	scroll      int    // vertical scroll offset — which display line is at the top
	placeholder string
	focused     bool
}

const inputMaxLines = 4

func newMultilineInput() multilineInput {
	return multilineInput{
		buf:         []rune{},
		cursor:      0,
		maxLines:    inputMaxLines,
		placeholder: "ask dun to do something…",
		focused:     true,
	}
}

// ── accessors (match the textinput.Model interface used by tuiModel) ──

func (m *multilineInput) Value() string {
	return string(m.buf)
}

func (m *multilineInput) SetValue(s string) {
	m.buf = []rune(s)
	m.clampCursor()
}

func (m *multilineInput) Reset() {
	m.buf = nil
	m.cursor = 0
	m.scroll = 0
}

func (m *multilineInput) Focus() {
	m.focused = true
}

func (m *multilineInput) Blur() {
	m.focused = false
}

func (m *multilineInput) CursorEnd() {
	m.cursor = len(m.buf)
	m.clampScroll()
}

// Position returns 0 when cursor is at the very beginning of the buffer,
// non-zero otherwise.  Used by the "left-at-front-hops-to-convo" check.
func (m *multilineInput) Position() int {
	if m.cursor == 0 {
		return 0
	}
	return 1
}

// isEmpty reports whether the input has no visible content.
func (m *multilineInput) isEmpty() bool {
	return strings.TrimSpace(string(m.buf)) == ""
}

// ── word wrapping ──

// wrappedLines computes the display grid from the flat buffer.
// Each element is one display row (a string of runes).
func (m *multilineInput) wrappedLines() []string {
	if m.width <= 0 {
		m.width = 80
	}
	if len(m.buf) == 0 {
		return []string{""}
	}
	// Split on \n first to get logical lines, then word-wrap each.
	logical := strings.Split(string(m.buf), "\n")
	var result []string
	for _, line := range logical {
		result = append(result, wordWrap(line, m.width)...)
	}
	if len(result) == 0 {
		result = []string{""}
	}
	return result
}

// wordWrap wraps a single logical line into display rows of at most w columns.
func wordWrap(s string, w int) []string {
	if w <= 0 {
		w = 80
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return []string{""}
	}

	var lines []string
	var current []rune

	for len(runes) > 0 {
		if len(runes) <= w {
			current = append(current, runes...)
			runes = nil
			break
		}

		chunk := runes[:w]
		rest := runes[w:]

		// Try to find a space to break on within the chunk.
		spacePos := -1
		for i := w - 1; i >= 0; i-- {
			if chunk[i] == ' ' {
				spacePos = i
				break
			}
		}

		if spacePos >= 1 {
			current = append(current, chunk[:spacePos]...)
			lines = append(lines, string(current))
			current = nil
			runes = rest
		} else {
			current = append(current, chunk...)
			lines = append(lines, string(current))
			current = nil
			runes = rest
		}
	}

	if len(current) > 0 || len(lines) == 0 {
		lines = append(lines, string(current))
	}

	return lines
}

// cursorDisplayPos returns the display (line, col) for the current cursor offset.
func (m *multilineInput) cursorDisplayPos() (line, col int) {
	wrapped := m.wrappedLines()
	off := 0
	for i, wl := range wrapped {
		wlLen := len([]rune(wl))
		if m.cursor < off+wlLen {
			return i, m.cursor - off
		}
		// Account for the \n that was consumed to create the next logical line.
		// After the last display row of a logical line, there's an implicit \n
		// (unless it's the very end of the buffer).
		if i+1 < len(wrapped) {
			// Check if the next wrapped line starts a new logical line.
			// We detect this by checking if the current line is shorter than width
			// (meaning it ended naturally, not by wrapping).
			if wlLen < m.width {
				off++ // skip the \n
			}
		}
		off += wlLen
	}
	// Cursor is at the very end.
	return len(wrapped) - 1, len([]rune(wrapped[len(wrapped)-1]))
}

// cursorAtEndOfWrappedLine reports whether the cursor sits at a wrap break
// point: the previous display row was exactly width-long (split by wrapping,
// not by an explicit \n) and there is more buffer after the cursor.
func (m *multilineInput) cursorAtEndOfWrappedLine() bool {
	if m.cursor >= len(m.buf) {
		return false
	}
	line, col := m.cursorDisplayPos()
	wrapped := m.wrappedLines()
	if line >= len(wrapped) {
		return false
	}
	// Case 1: cursor is at the start of a display line (col == 0) and the
	// previous display line was full-width → we're at a wrap break.
	if line > 0 && col == 0 && len([]rune(wrapped[line-1])) == m.width {
		return true
	}
	// Case 2: cursor is at the end of a display line that is full-width,
	// and the next char in the buffer is not \n (i.e., it's a wrap, not a real newline).
	if col == len([]rune(wrapped[line])) && len([]rune(wrapped[line])) == m.width {
		// Check that the next buffer char is not \n (would be a real line break).
		if m.cursor < len(m.buf) && m.buf[m.cursor] != '\n' {
			return true
		}
	}
	return false
}

// clampCursor ensures the cursor offset is valid.
func (m *multilineInput) clampCursor() {
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor > len(m.buf) {
		m.cursor = len(m.buf)
	}
	m.clampScroll()
}

// clampScroll ensures the cursor's display line is visible.
func (m *multilineInput) clampScroll() {
	line, _ := m.cursorDisplayPos()
	wrapped := m.wrappedLines()
	totalLines := len(wrapped)
	if totalLines <= m.maxLines {
		m.scroll = 0
		return
	}
	if line < m.scroll {
		m.scroll = line
	}
	if line >= m.scroll+m.maxLines {
		m.scroll = line - m.maxLines + 1
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
	if m.scroll+m.maxLines > totalLines {
		m.scroll = totalLines - m.maxLines
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

// ── key handling ──

// HandleKey processes a single key event and returns the updated input.
func (m *multilineInput) HandleKey(msg tea.KeyMsg) multilineInput {
	if !m.focused {
		return *m
	}

	switch msg.Type {
	case tea.KeyEnter:
		// Enter is handled by the caller (submit).
	case tea.KeyUp:
		m.cursorUp()
	case tea.KeyDown:
		m.cursorDown()
	case tea.KeyLeft:
		m.cursorLeft()
	case tea.KeyRight:
		m.cursorRight()
	case tea.KeyBackspace:
		m.backspace()
	case tea.KeyDelete:
		m.delete()
	case tea.KeyPgUp:
		m.pageUp()
	case tea.KeyPgDown:
		m.pageDown()
	case tea.KeyHome:
		m.cursor = 0
		m.clampCursor()
	case tea.KeyEnd:
		m.cursor = len(m.buf)
		m.clampCursor()
	case tea.KeyRunes:
		m.buf = append(m.buf[:m.cursor], append(msg.Runes, m.buf[m.cursor:]...)...)
		m.cursor += len(msg.Runes)
		m.clampCursor()
	}

	return *m
}

// ── cursor movement ──

func (m *multilineInput) cursorUp() {
	line, col := m.cursorDisplayPos()
	if line <= 0 {
		return
	}
	// Find the cursor offset for (line-1, col).
	m.cursor = m.displayToOffset(line-1, col)
	m.clampCursor()
}

func (m *multilineInput) cursorDown() {
	// If at the end of a wrapped line (but not end of buffer), insert newline.
	if m.cursor < len(m.buf) && m.cursorAtEndOfWrappedLine() {
		m.buf = append(m.buf[:m.cursor], append([]rune{'\n'}, m.buf[m.cursor:]...)...)
		m.cursor++
		m.clampCursor()
		return
	}
	line, col := m.cursorDisplayPos()
	wrapped := m.wrappedLines()
	if line >= len(wrapped)-1 {
		return
	}
	m.cursor = m.displayToOffset(line+1, col)
	m.clampCursor()
}

func (m *multilineInput) cursorLeft() {
	if m.cursor > 0 {
		m.cursor--
		m.clampCursor()
	}
}

func (m *multilineInput) cursorRight() {
	if m.cursor < len(m.buf) && m.cursorAtEndOfWrappedLine() {
		// At end of a wrapped line → insert newline.
		m.buf = append(m.buf[:m.cursor], append([]rune{'\n'}, m.buf[m.cursor:]...)...)
		m.cursor++
		m.clampCursor()
		return
	}
	if m.cursor < len(m.buf) {
		m.cursor++
		m.clampCursor()
	}
}

func (m *multilineInput) pageUp() {
	line, col := m.cursorDisplayPos()
	target := line - m.maxLines
	if target < 0 {
		target = 0
	}
	m.cursor = m.displayToOffset(target, col)
	m.clampCursor()
}

func (m *multilineInput) pageDown() {
	line, col := m.cursorDisplayPos()
	wrapped := m.wrappedLines()
	target := line + m.maxLines
	if target >= len(wrapped) {
		target = len(wrapped) - 1
	}
	m.cursor = m.displayToOffset(target, col)
	m.clampCursor()
}

// displayToOffset converts a display (line, col) to a flat buffer offset.
func (m *multilineInput) displayToOffset(line, col int) int {
	wrapped := m.wrappedLines()
	if line >= len(wrapped) {
		line = len(wrapped) - 1
	}
	if line < 0 {
		line = 0
	}

	off := 0
	for i, wl := range wrapped {
		wlRunes := []rune(wl)
		wlLen := len(wlRunes)
		if i == line {
			if col > wlLen {
				col = wlLen
			}
			return off + col
		}
		// After this display line, check if there's a \n (logical line boundary).
		if i+1 < len(wrapped) && wlLen < m.width {
			// This line ended naturally (not by wrapping) → there's a \n.
			off++
		}
		off += wlLen
	}
	return off
}

// ── editing ──

func (m *multilineInput) backspace() {
	if m.cursor > 0 {
		m.buf = append(m.buf[:m.cursor-1], m.buf[m.cursor:]...)
		m.cursor--
		m.clampCursor()
	}
}

func (m *multilineInput) delete() {
	if m.cursor < len(m.buf) {
		m.buf = append(m.buf[:m.cursor], m.buf[m.cursor+1:]...)
		m.clampCursor()
	}
}

// ── rendering ──

// View renders the 4-line input box with cursor.
func (m *multilineInput) View() string {
	wrapped := m.wrappedLines()
	m.clampScroll()

	cursorLine, cursorCol := m.cursorDisplayPos()

	var sb strings.Builder
	for i := 0; i < m.maxLines; i++ {
		displayIdx := m.scroll + i
		var lineStr string
		if displayIdx < len(wrapped) {
			lineStr = wrapped[displayIdx]
		}

		// Prompt: "› " on first line, "  " on subsequent lines.
		prompt := "› "
		if i > 0 {
			prompt = "  "
		}

		// Cursor on this line?
		isCursorLine := (displayIdx == cursorLine) && m.focused

		if isCursorLine {
			lineRunes := []rune(lineStr)
			before := ""
			after := lineStr
			cursorCh := "█"
			if cursorCol < len(lineRunes) {
				before = string(lineRunes[:cursorCol])
				cursorCh = string(lineRunes[cursorCol])
				after = string(lineRunes[cursorCol+1:])
			} else {
				before = lineStr
			}
			cursorStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
			sb.WriteString(prompt)
			sb.WriteString(before)
			sb.WriteString(cursorStyle.Render(cursorCh))
			sb.WriteString(after)
		} else {
			sb.WriteString(prompt)
			sb.WriteString(lineStr)
		}

		// Pad to width.
		displayLen := lipgloss.Width(prompt + lineStr)
		for displayLen < m.width+2 {
			sb.WriteString(" ")
			displayLen++
		}

		if i < m.maxLines-1 {
			sb.WriteString("\n")
		}
	}

	// If no content and not focused, show placeholder.
	if m.isEmpty() && !m.focused {
		prompt := "› "
		sb.Reset()
		sb.WriteString(prompt)
		sb.WriteString(stDim.Render(m.placeholder))
		displayLen := lipgloss.Width(prompt + m.placeholder)
		for displayLen < m.width+2 {
			sb.WriteString(" ")
			displayLen++
		}
		for i := 1; i < m.maxLines; i++ {
			sb.WriteString("\n  ")
			for j := 0; j < m.width; j++ {
				sb.WriteString(" ")
			}
		}
		return sb.String()
	}

	return sb.String()
}
