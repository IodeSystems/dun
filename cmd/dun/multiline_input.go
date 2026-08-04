package main

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// multilineInput is the message composer: a word-wrapped text box that grows
// from one line to inputMaxLines and scrolls after that.
//
// The buffer is a flat rune slice and the cursor is a flat offset into it.
// Wrapping is computed at render time into SEGMENTS that carry their own
// [start,end) offsets — see wrapSeg. That is the whole design: the first
// version derived cursor offsets by re-measuring the wrapped STRINGS, which
// cannot recover the characters wrapping consumed (the space it broke on, the
// newline it split on), so the cursor drifted and text went missing on the
// screen. A segment knows where it came from, so display↔offset is exact in
// both directions.
//
// Enter submits (the caller owns that). alt+enter / ctrl+j insert a newline.
// Readline motions and kills are bound below — a text box that a shell user
// cannot drive with ctrl+a/ctrl+e/ctrl+w is not finished.
type multilineInput struct {
	buf         []rune // flat text buffer
	cursor      int    // flat rune offset into buf
	width       int    // columns available for text (set on WindowSizeMsg)
	maxLines    int    // most display rows the box may occupy
	scroll      int    // first visible display row
	placeholder string
	focused     bool
	blinkOn     bool   // is the caret visible right now?
}

// inputMaxLines caps the box. It GROWS to this, it does not start at it: an
// empty composer that reserves four rows is four rows of conversation nobody
// gets back.
const inputMaxLines = 4

// stCaret is the cursor cell. Reverse video rather than a "█" glyph, which
// painted over the character it was sitting on.
var stCaret = lipgloss.NewStyle().Reverse(true)

func newMultilineInput() multilineInput {
	return multilineInput{
		maxLines:    inputMaxLines,
		placeholder: "ask dun to do something…",
		focused:     true,
		blinkOn:     true,
	}
}

// ── accessors (the subset of textinput.Model tuiModel used) ──

func (m *multilineInput) Value() string { return string(m.buf) }

func (m *multilineInput) SetValue(s string) {
	m.buf = []rune(s)
	m.clampCursor()
}

func (m *multilineInput) Reset() {
	m.buf, m.cursor, m.scroll = nil, 0, 0
}

func (m *multilineInput) Focus() {
	m.focused = true
	m.blinkOn = true
}
func (m *multilineInput) Blur() { m.focused = false }

// BlinkTick toggles the caret visibility and returns the next tick command.
// When the input is not focused the caret is always on (no blink while idle).
func (m *multilineInput) BlinkTick() tea.Cmd {
	if !m.focused {
		return nil
	}
	m.blinkOn = !m.blinkOn
	return tea.Tick(530*time.Millisecond, func(time.Time) tea.Msg {
		return blinkTickMsg{}
	})
}

type blinkTickMsg struct{}

func (m *multilineInput) CursorEnd() {
	m.cursor = len(m.buf)
	m.clampScroll()
}

// Position is 0 only with the cursor at the very front of the buffer — which is
// what the "← at the front hops to the conversation" check asks about.
func (m *multilineInput) Position() int {
	if m.cursor == 0 {
		return 0
	}
	return 1
}



// ── wrapping ──

// wrapSeg is one display row: the buffer range it shows, plus where the next
// row starts. next > end exactly when wrapping consumed a character (the space
// it broke on, or a newline), which is the fact that makes the offset mapping
// reversible.
type wrapSeg struct {
	start, end int
	next       int
}

func (m *multilineInput) textWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 80
}

// segments wraps the buffer into display rows. Always at least one row, so an
// empty buffer still has somewhere to put the cursor.
func (m *multilineInput) segments() []wrapSeg {
	w := m.textWidth()
	var segs []wrapSeg
	lineStart := 0
	for i := 0; i <= len(m.buf); i++ {
		if i < len(m.buf) && m.buf[i] != '\n' {
			continue
		}
		segs = append(segs, wrapLine(m.buf, lineStart, i, w)...)
		// The newline itself belongs to no row; it is what `next` skips.
		if n := len(segs) - 1; n >= 0 && i < len(m.buf) {
			segs[n].next = i + 1
		}
		lineStart = i + 1
	}
	if len(segs) == 0 {
		segs = []wrapSeg{{}}
	}
	return segs
}

// wrapLine greedily wraps buf[start:end] into rows of at most w columns,
// breaking on the last space that fits and consuming it.
func wrapLine(buf []rune, start, end, w int) []wrapSeg {
	if start >= end {
		return []wrapSeg{{start: start, end: start, next: start}}
	}
	var segs []wrapSeg
	for start < end {
		if end-start <= w {
			segs = append(segs, wrapSeg{start: start, end: end, next: end})
			break
		}
		// The break point is the last space inside the window. Breaking AT the
		// window edge when there is no space is deliberate: an unbroken 200-char
		// token has to go somewhere, and refusing to split it would overflow.
		brk := -1
		for i := start + w; i > start; i-- {
			if buf[i-1] == ' ' {
				brk = i - 1
				break
			}
		}
		if brk <= start {
			segs = append(segs, wrapSeg{start: start, end: start + w, next: start + w})
			start += w
			continue
		}
		segs = append(segs, wrapSeg{start: start, end: brk, next: brk + 1})
		start = brk + 1
	}
	return segs
}

func (m *multilineInput) text(s wrapSeg) string { return string(m.buf[s.start:s.end]) }

// cursorDisplayPos maps the flat cursor to a (row, column).
func (m *multilineInput) cursorDisplayPos() (int, int) {
	segs := m.segments()
	for i, s := range segs {
		if m.cursor < s.next || (i == len(segs)-1 && m.cursor <= s.end) {
			col := m.cursor - s.start
			if col < 0 {
				col = 0
			}
			if col > s.end-s.start {
				col = s.end - s.start
			}
			return i, col
		}
	}
	last := segs[len(segs)-1]
	return len(segs) - 1, last.end - last.start
}

// displayToOffset is the inverse, clamped to the row's own text.
func (m *multilineInput) displayToOffset(row, col int) int {
	segs := m.segments()
	if row < 0 {
		row = 0
	}
	if row >= len(segs) {
		row = len(segs) - 1
	}
	s := segs[row]
	if col > s.end-s.start {
		col = s.end - s.start
	}
	if col < 0 {
		col = 0
	}
	return s.start + col
}

// height is how many rows the box occupies right now: one until the text needs
// more, never more than maxLines.
func (m *multilineInput) height() int {
	n := len(m.segments())
	if n < 1 {
		n = 1
	}
	if n > m.maxLines {
		n = m.maxLines
	}
	return n
}

// AtFirstLine / AtLastLine let the caller decide when ↑/↓ stop moving the
// cursor and start recalling history — the shell behaviour, where the edge of
// the buffer is what hands the key on.
func (m *multilineInput) AtFirstLine() bool {
	row, _ := m.cursorDisplayPos()
	return row == 0
}

func (m *multilineInput) AtLastLine() bool {
	row, _ := m.cursorDisplayPos()
	return row == len(m.segments())-1
}

func (m *multilineInput) clampCursor() {
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor > len(m.buf) {
		m.cursor = len(m.buf)
	}
	m.clampScroll()
}

// clampScroll keeps the cursor's row inside the visible window.
func (m *multilineInput) clampScroll() {
	row, _ := m.cursorDisplayPos()
	total := len(m.segments())
	if total <= m.maxLines {
		m.scroll = 0
		return
	}
	if row < m.scroll {
		m.scroll = row
	}
	if row >= m.scroll+m.maxLines {
		m.scroll = row - m.maxLines + 1
	}
	if max := total - m.maxLines; m.scroll > max {
		m.scroll = max
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

// ── key handling ──

// HandleKey processes one key. Motions and kills follow readline, because that
// is what every other text field on the machine does.
func (m *multilineInput) HandleKey(msg tea.KeyMsg) multilineInput {
	if !m.focused {
		return *m
	}

	// Named-key bindings are matched on String(), but ONLY for keys that are not
	// plain runes: a bracketed paste arrives as KeyRunes, and pasting the word
	// "home" would otherwise be read as the Home key.
	if msg.Type != tea.KeyRunes || msg.Alt {
		switch msg.String() {
		// A newline has to be typeable: enter submits, so the composer needs its own
		// key. alt+enter is the common one; ctrl+j is what the terminal actually
		// sends for it on setups where alt is not sent as a modifier.
		case "alt+enter", "ctrl+j":
			m.insert([]rune{'\n'})
			return *m
		case "ctrl+a", "home":
			m.cursor = m.lineStart()
			m.clampCursor()
			return *m
		case "ctrl+e", "end":
			m.cursor = m.lineEnd()
			m.clampCursor()
			return *m
		case "ctrl+b":
			m.cursorLeft()
			return *m
		case "ctrl+f":
			m.cursorRight()
			return *m
		case "alt+left", "ctrl+left", "alt+b":
			m.cursor = m.wordLeft()
			m.clampCursor()
			return *m
		case "alt+right", "ctrl+right", "alt+f":
			m.cursor = m.wordRight()
			m.clampCursor()
			return *m
		case "ctrl+w", "alt+backspace", "ctrl+backspace":
			m.deleteTo(m.wordLeft())
			return *m
		case "alt+d":
			m.deleteTo(m.wordRight())
			return *m
		case "ctrl+k":
			m.deleteTo(m.lineEnd())
			return *m
		case "ctrl+u":
			m.deleteTo(m.lineStart())
			return *m
		case "ctrl+d":
			m.delete()
			return *m
		}
	}

	switch msg.Type {
	case tea.KeyEnter:
		// Submit — the caller's business.
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
		m.pageBy(-m.maxLines)
	case tea.KeyPgDown:
		m.pageBy(m.maxLines)
	case tea.KeyRunes, tea.KeySpace:
		r := msg.Runes
		if msg.Type == tea.KeySpace {
			r = []rune{' '}
		}
		m.insert(r)
	}
	return *m
}

// insert puts runes at the cursor. Built through a fresh slice on purpose: the
// obvious append(buf[:c], append(runes, buf[c:]...)...) writes through the
// caller's slice and can alias the buffer it is copying from.
func (m *multilineInput) insert(r []rune) {
	if len(r) == 0 {
		return
	}
	out := make([]rune, 0, len(m.buf)+len(r))
	out = append(out, m.buf[:m.cursor]...)
	out = append(out, r...)
	out = append(out, m.buf[m.cursor:]...)
	m.buf = out
	m.cursor += len(r)
	m.clampCursor()
}

// deleteTo removes everything between the cursor and to, in either direction.
func (m *multilineInput) deleteTo(to int) {
	lo, hi := m.cursor, to
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo < 0 {
		lo = 0
	}
	if hi > len(m.buf) {
		hi = len(m.buf)
	}
	if lo == hi {
		return
	}
	out := make([]rune, 0, len(m.buf)-(hi-lo))
	out = append(out, m.buf[:lo]...)
	out = append(out, m.buf[hi:]...)
	m.buf, m.cursor = out, lo
	m.clampCursor()
}

// lineStart / lineEnd are the LOGICAL line's bounds — what ctrl+a and ctrl+k
// mean everywhere else. A wrapped row is a rendering artefact, not a line.
func (m *multilineInput) lineStart() int {
	for i := m.cursor - 1; i >= 0; i-- {
		if m.buf[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

func (m *multilineInput) lineEnd() int {
	for i := m.cursor; i < len(m.buf); i++ {
		if m.buf[i] == '\n' {
			return i
		}
	}
	return len(m.buf)
}

func (m *multilineInput) wordLeft() int {
	i := m.cursor
	for i > 0 && isWordSep(m.buf[i-1]) {
		i--
	}
	for i > 0 && !isWordSep(m.buf[i-1]) {
		i--
	}
	return i
}

func (m *multilineInput) wordRight() int {
	i := m.cursor
	for i < len(m.buf) && isWordSep(m.buf[i]) {
		i++
	}
	for i < len(m.buf) && !isWordSep(m.buf[i]) {
		i++
	}
	return i
}

func isWordSep(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }

// ── cursor movement ──

func (m *multilineInput) cursorUp()   { m.moveRow(-1) }
func (m *multilineInput) cursorDown() { m.moveRow(1) }

func (m *multilineInput) moveRow(d int) {
	row, col := m.cursorDisplayPos()
	target := row + d
	if target < 0 || target >= len(m.segments()) {
		return
	}
	m.cursor = m.displayToOffset(target, col)
	m.clampCursor()
}

func (m *multilineInput) pageBy(d int) {
	row, col := m.cursorDisplayPos()
	target := row + d
	if target < 0 {
		target = 0
	}
	if n := len(m.segments()); target >= n {
		target = n - 1
	}
	m.cursor = m.displayToOffset(target, col)
	m.clampCursor()
}

func (m *multilineInput) cursorLeft() {
	if m.cursor > 0 {
		m.cursor--
		m.clampCursor()
	}
}

func (m *multilineInput) cursorRight() {
	if m.cursor < len(m.buf) {
		m.cursor++
		m.clampCursor()
	}
}

func (m *multilineInput) backspace() {
	if m.cursor > 0 {
		m.deleteTo(m.cursor - 1)
	}
}

func (m *multilineInput) delete() {
	if m.cursor < len(m.buf) {
		out := make([]rune, 0, len(m.buf)-1)
		out = append(out, m.buf[:m.cursor]...)
		out = append(out, m.buf[m.cursor+1:]...)
		m.buf = out
		m.clampCursor()
	}
}

// ── rendering ──

// View renders the visible rows, prompt on the first one, cursor in reverse
// video. Height is height() — the box grows with the text.
func (m *multilineInput) View() string {
	m.clampScroll()
	if len(m.buf) == 0 {
		return m.placeholderView()
	}
	segs := m.segments()
	row, col := m.cursorDisplayPos()

	lines := make([]string, 0, m.maxLines)
	for i := 0; i < m.height(); i++ {
		idx := m.scroll + i
		prompt := "  "
		if idx == 0 {
			prompt = "› "
		}
		var text string
		if idx < len(segs) {
			text = m.text(segs[idx])
		}
		if m.focused && m.blinkOn && idx == row {
			text = withCaret(text, col)
		}
		lines = append(lines, m.pad(prompt+text))
	}
	return strings.Join(lines, "\n")
}

// placeholderView is the empty state. The placeholder shows whether or not the
// box has focus — a hint that disappears when you click into the field is the
// wrong way round, and it is where the caret has to live anyway.
func (m *multilineInput) placeholderView() string {
	text := stDim.Render(m.placeholder)
	if m.focused {
		text = withCaret(m.placeholder, 0)
	}
	return m.pad("› " + text)
}

// ghostView renders the empty input with ghost text (a suggested completion)
// shown as dimmed text after the prompt. The caret sits at the beginning of
// the ghost text — right arrow accepts it, up/down cycle suggestions.
func (m *multilineInput) ghostView(ghost string) string {
	text := stDim.Render(ghost)
	if m.focused {
		text = withCaret(ghost, 0)
	}
	return m.pad("› " + text)
}

// withCaret reverse-videos the cell at col, adding one past the end of the text
// when that is where the cursor sits.
func withCaret(s string, col int) string {
	r := []rune(s)
	if col >= len(r) {
		return s + stCaret.Render(" ")
	}
	return string(r[:col]) + stCaret.Render(string(r[col])) + string(r[col+1:])
}

// pad fills the row to the full width so a shortened line cannot leave the
// previous frame's characters behind it.
func (m *multilineInput) pad(s string) string {
	if n := m.textWidth() + 2 - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
