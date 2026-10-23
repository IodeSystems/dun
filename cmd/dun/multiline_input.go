package main

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/iodesystems/dun/proto"
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
// Columns are terminal CELLS, not runes (see layout): a CJK character or an
// emoji takes two, a combining mark joins the character before it, a tab
// draws as spaces to the next tabStop. Counting runes let such text run past
// the box and shift the frame.
//
// A big paste sits in the buffer as ONE rune — a paste token — and its text
// in a side table (see pastes). The box shows a one-line label for it; Value
// gives the full text back.
//
// Enter submits (the caller owns that). alt+enter / ctrl+j insert a newline.
// Readline motions and kills are bound below — a text box that a shell user
// cannot drive with ctrl+a/ctrl+e/ctrl+w is not finished.
type multilineInput struct {
	buf         []rune // flat text buffer; never written in place (layout caches by identity)
	cursor      int    // flat rune offset into buf, always at a unit start
	width       int    // columns available for text (set on WindowSizeMsg)
	maxLines    int    // most display rows the box may occupy
	scroll      int    // first visible display row
	placeholder string
	focused     bool
	blinkOn     bool      // is the caret visible right now?
	typedAt     time.Time // last key; the caret holds solid while typing

	// pastes holds the text of each paste token in buf. A token rune not in
	// the table is ordinary text.
	pastes  map[rune]string
	nextTok rune
	// images holds each image token in buf (imageinput.go); imgs is every
	// image label made, shared by the copies, so a label in recalled text
	// becomes its token again.
	images map[rune]proto.Image
	imgs   *imageRegistry
	// lay is the last layout. A pointer so the copies Bubble Tea makes of the
	// model (View runs on one) share it.
	lay *layout
}

// inputMaxLines caps the box. It GROWS to this, it does not start at it: an
// empty composer that reserves four rows is four rows of conversation nobody
// gets back.
const inputMaxLines = 4

// tabStop: a tab draws as spaces up to the next multiple of this, counted
// from the start of its row. The buffer keeps the \t, so the text sent is
// unchanged; only the drawing is spaces. The terminal's own tab expansion is
// not used because it counts from the screen edge (the prompt shifts it) and
// does not erase what was under it.
const tabStop = 4

// A paste of more lines than pasteMaxLines, or more runes than pasteMaxRunes,
// becomes a paste token instead of flooding the box.
const (
	pasteMaxLines = 10
	pasteMaxRunes = 2000
)

// Paste tokens are runes in Supplementary Private Use Area-B. Area-A is
// avoided: Nerd Fonts put icons there, and those get pasted.
const (
	tokFirst rune = 0x100000
	tokLast  rune = 0x10FFFD
)

// stCaret is the cursor cell. Reverse video rather than a "█" glyph, which
// painted over the character it was sitting on.
var stCaret = lipgloss.NewStyle().Reverse(true)

func newMultilineInput() multilineInput {
	return multilineInput{
		maxLines:    inputMaxLines,
		placeholder: "ask dun to do something…",
		focused:     true,
		blinkOn:     true,
		lay:         &layout{},
		imgs:        &imageRegistry{},
	}
}

// ── accessors (the subset of textinput.Model tuiModel used) ──

// Value is the text as written: paste tokens are replaced by their text,
// image tokens by their labels.
func (m *multilineInput) Value() string {
	if len(m.pastes) == 0 && len(m.images) == 0 {
		return string(m.buf)
	}
	var b strings.Builder
	for _, r := range m.buf {
		if p, ok := m.pastes[r]; ok {
			b.WriteString(p)
		} else if img, ok := m.images[r]; ok {
			b.WriteString(img.Label)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SetValue replaces the text. Text big enough to be a paste token (a recalled
// history entry, a restored draft) becomes one, so recalling it does not
// flood the box either; ctrl+o opens it. An image label this TUI knows
// becomes its image token again.
func (m *multilineInput) SetValue(s string) {
	m.pastes, m.images = nil, nil
	r := cleanInput([]rune(s))
	if bigPaste(r) {
		r = []rune{m.addPaste(r)}
	} else {
		r = m.tokenizeImages(r)
	}
	m.buf = r
	m.clampCursor()
}

// cleanInput is text fit for the composer: what a paste of styled text (or a
// stray control key) carries that would draw on the terminal instead of in
// the box goes. Escape sequences (colors, OSC links) are stripped; \r\n and
// a lone \r become \n (a raw \r sends the cursor to column 0 and the line
// overwrites itself); a tab stays; every other control character goes.
// ctrlTabRune — ctrl+tab, as the key filter spells it (keyfilter.go) — is a
// tab: it is how a tab is typed, since plain tab moves between zones.
func cleanInput(r []rune) []rune {
	s := string(r)
	if strings.ContainsRune(s, '\x1b') || strings.ContainsRune(s, '\u009b') {
		s = ansi.Strip(s)
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	out := make([]rune, 0, len(s))
	for _, c := range s {
		switch {
		case c == '\r':
			out = append(out, '\n')
		case c == '\n' || c == '\t':
			out = append(out, c)
		case c == ctrlTabRune:
			out = append(out, '\t')
		case c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0):
			// C0, DEL, C1 controls: never text
		case c == '\u200b' || c == '\ufeff':
			// zero-width space, byte-order mark: invisible, and they shift the
			// cursor against the text
		default:
			out = append(out, c)
		}
	}
	return out
}

func (m *multilineInput) Reset() {
	m.buf, m.cursor, m.scroll, m.pastes, m.images = nil, 0, 0, nil, nil
}

func (m *multilineInput) Focus() {
	m.focused = true
	m.blinkOn = true
}
func (m *multilineInput) Blur() { m.focused = false }

// caretBlink is the blink half-period. The caret stays solid while keys
// arrive and starts blinking once typing has paused for one period.
const caretBlink = 530 * time.Millisecond

// BlinkTick toggles the caret visibility and returns the next tick command.
// When the input is not focused the caret is always on (no blink while idle).
func (m *multilineInput) BlinkTick() tea.Cmd {
	if !m.focused {
		return nil
	}
	m.blinkOn = !m.blinkOn || time.Since(m.typedAt) < caretBlink
	return tea.Tick(caretBlink, func(time.Time) tea.Msg {
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

// ── paste tokens ──

// bigPaste: more than pasteMaxLines lines or pasteMaxRunes runes.
func bigPaste(r []rune) bool {
	if len(r) > pasteMaxRunes {
		return true
	}
	n := 0
	for _, c := range r {
		if c == '\n' {
			n++
		}
	}
	return pasteLines(r, n) > pasteMaxLines
}

// pasteLines counts lines the way an editor does: a trailing newline ends the
// last line, it does not start another.
func pasteLines(r []rune, newlines int) int {
	if len(r) > 0 && r[len(r)-1] != '\n' {
		return newlines + 1
	}
	return newlines
}

// addPaste files text in the side table and returns its token.
func (m *multilineInput) addPaste(text []rune) rune {
	if m.pastes == nil {
		m.pastes = map[rune]string{}
	}
	if m.nextTok < tokFirst || m.nextTok > tokLast {
		m.nextTok = tokFirst
	}
	tok := m.nextTok
	m.nextTok++
	m.pastes[tok] = string(text)
	return tok
}

func (m *multilineInput) isTok(r rune) bool {
	if r < tokFirst || r > tokLast {
		return false
	}
	if _, ok := m.pastes[r]; ok {
		return true
	}
	_, ok := m.images[r]
	return ok
}

// pasteLabel is what a token shows: [pasted 120 lines · 4,312 chars].
func (m *multilineInput) pasteLabel(tok rune) string {
	if img, ok := m.images[tok]; ok {
		return img.Label
	}
	p := []rune(m.pastes[tok])
	n := pasteLines(p, strings.Count(string(p), "\n"))
	unit := "lines"
	if n == 1 {
		unit = "line"
	}
	return commaf("[pasted %d %s · %d chars]", n, unit, len(p))
}

// expandPaste (ctrl+o) puts a token's text back into the buffer for editing:
// the token just before the cursor, else the one under it, else every token.
// An image token has no text to edit and stays.
func (m *multilineInput) expandPaste() {
	isPaste := func(r rune) bool { _, ok := m.pastes[r]; return ok }
	at := -1
	if m.cursor > 0 && isPaste(m.buf[m.cursor-1]) {
		at = m.cursor - 1
	} else if m.cursor < len(m.buf) && isPaste(m.buf[m.cursor]) {
		at = m.cursor
	}
	out := make([]rune, 0, len(m.buf))
	cur := m.cursor
	for i, r := range m.buf {
		if isPaste(r) && (at < 0 || i == at) {
			p := []rune(m.pastes[r])
			out = append(out, p...)
			if i < m.cursor {
				cur += len(p) - 1
			}
			continue
		}
		out = append(out, r)
	}
	m.buf, m.cursor = out, cur
	m.clampCursor()
}

// ── layout ──

// wrapSeg is one display row: the buffer range it shows, plus where the next
// row starts. next > end exactly when wrapping consumed a character (the space
// it broke on, or a newline), which is the fact that makes the offset mapping
// reversible. w is the row's width in cells.
type wrapSeg struct {
	start, end int
	next       int
	w          int
}

// layout is the buffer wrapped at one width. The text is cut into UNITS — a
// grapheme cluster, a tab, a paste token — that never split across rows and
// that the cursor moves over whole. Per rune: x is the column of the unit it
// belongs to, uw the unit's width in cells on the rune that starts it and 0
// on the rest (every unit is at least one cell wide, so uw > 0 marks a start).
type layout struct {
	buf   []rune // the buffer this was computed for (compared by identity)
	width int
	segs  []wrapSeg
	x     []int32
	uw    []int32
}

func (m *multilineInput) textWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 80
}

// wrapWidth is the cells a row's text may take: one less than the box, so the
// caret has a cell after a full row instead of pushing the row one past the
// edge.
func (m *multilineInput) wrapWidth() int {
	return max(1, m.textWidth()-1)
}

func sameBuf(a, b []rune) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// layout returns the layout for the current buffer and width. Cached: a
// keystroke asks for it a dozen times and a 50,000-rune line is not free to
// wrap. The key is the buffer's identity — every edit builds a fresh slice.
func (m *multilineInput) layout() *layout {
	w := m.wrapWidth()
	if m.lay == nil {
		m.lay = &layout{}
	}
	if l := m.lay; l.segs != nil && l.width == w && sameBuf(l.buf, m.buf) {
		return l
	}
	*m.lay = m.computeLayout(w)
	return m.lay
}

func (m *multilineInput) segments() []wrapSeg { return m.layout().segs }

// computeLayout wraps the buffer into display rows. Always at least one row,
// so an empty buffer still has somewhere to put the cursor.
func (m *multilineInput) computeLayout(w int) layout {
	l := layout{buf: m.buf, width: w, x: make([]int32, len(m.buf)+1), uw: make([]int32, len(m.buf)+1)}
	lineStart := 0
	var units []unit
	for i := 0; i <= len(m.buf); i++ {
		if i < len(m.buf) && m.buf[i] != '\n' {
			continue
		}
		units = m.lineUnits(units[:0], lineStart, i, w)
		l.segs = wrapUnits(l.segs, units, lineStart, i, w, l.x, l.uw)
		// The newline itself belongs to no row; it is what `next` skips.
		if i < len(m.buf) {
			n := len(l.segs) - 1
			l.segs[n].next = i + 1
			l.x[i] = int32(l.segs[n].w)
		}
		lineStart = i + 1
	}
	if len(l.segs) == 0 {
		l.segs = []wrapSeg{{}}
	}
	return l
}

type unit struct {
	a, b  int // rune range
	w     int // cells; 0 for a tab (its width depends on its column)
	tab   bool
	space bool
}

// lineUnits cuts buf[start:end] (no newlines) into units. Clusters and their
// widths come from ansi.FirstGraphemeCluster — what ansi.StringWidth and
// lipgloss.Width count with. A cluster of width 0 (a lone combining mark)
// joins the unit before it; at the start of a line it counts as one cell.
func (m *multilineInput) lineUnits(units []unit, start, end, w int) []unit {
	addZero := func(a, b int) {
		if n := len(units); n > 0 {
			units[n-1].b = b
			return
		}
		units = append(units, unit{a: a, b: b, w: 1})
	}
	run := func(a, b int) {
		s := string(m.buf[a:b])
		for s != "" {
			cl, cw := ansi.FirstGraphemeCluster(s, ansi.GraphemeWidth)
			if cl == "" { // cannot happen on valid UTF-8; never loop forever
				cl, cw = s[:1], 1
			}
			n := utf8.RuneCountInString(cl)
			if cw <= 0 {
				addZero(a, a+n)
			} else {
				units = append(units, unit{a: a, b: a + n, w: cw, space: cl == " "})
			}
			a += n
			s = s[len(cl):]
		}
	}
	runStart := start
	for i := start; i < end; i++ {
		r := m.buf[i]
		if r != '\t' && !m.isTok(r) {
			continue
		}
		if runStart < i {
			run(runStart, i)
		}
		if r == '\t' {
			units = append(units, unit{a: i, b: i + 1, tab: true})
		} else {
			units = append(units, unit{a: i, b: i + 1, w: min(w, ansi.StringWidth(m.pasteLabel(r)))})
		}
		runStart = i + 1
	}
	if runStart < end {
		run(runStart, end)
	}
	return units
}

// wrapUnits greedily wraps one logical line's units into rows of at most w
// cells, breaking on the last space that fits and consuming it. A unit that
// does not fit moves to the next row whole; a tab that does not fit shrinks
// to the end of the row, as a terminal's does at the margin.
func wrapUnits(segs []wrapSeg, units []unit, start, end, w int, x, uw []int32) []wrapSeg {
	rowStart, rowFirst, col := start, 0, 0
	lastSpace, lastSpaceCol := -1, 0
	newRow := func(k int) {
		rowFirst, col, lastSpace = k, 0, -1
		if k < len(units) {
			rowStart = units[k].a
		} else {
			rowStart = end
		}
	}
	for k := 0; k < len(units); {
		u := units[k]
		cw := u.w
		if u.tab {
			cw = min(tabStop-col%tabStop, w-col)
			if cw <= 0 {
				cw = tabStop // does not fit at all: wrap it like anything else
			}
		}
		if col+cw > w && k > rowFirst {
			switch {
			case u.space: // break here, consuming this space
				segs = append(segs, wrapSeg{start: rowStart, end: u.a, next: u.b, w: col})
				x[u.a], uw[u.a] = int32(col), 1
				newRow(k + 1)
			case lastSpace > rowFirst: // back up to the last space
				s := units[lastSpace]
				segs = append(segs, wrapSeg{start: rowStart, end: s.a, next: s.b, w: lastSpaceCol})
				newRow(lastSpace + 1)
			default: // no space: break at the edge — a long token has to go somewhere
				segs = append(segs, wrapSeg{start: rowStart, end: u.a, next: u.a, w: col})
				newRow(k)
			}
			k = rowFirst
			continue
		}
		for i := u.a; i < u.b; i++ {
			x[i], uw[i] = int32(col), 0
		}
		uw[u.a] = int32(cw)
		if u.space && k > rowFirst {
			lastSpace, lastSpaceCol = k, col
		}
		col += cw
		k++
	}
	return append(segs, wrapSeg{start: rowStart, end: end, next: end, w: col})
}

// wrapLine wraps plain text buf[start:end] (no newlines, no paste tokens) into
// rows of at most w cells.
func wrapLine(buf []rune, start, end, w int) []wrapSeg {
	m := multilineInput{buf: buf}
	x, uw := make([]int32, len(buf)+1), make([]int32, len(buf)+1)
	return wrapUnits(nil, m.lineUnits(nil, start, end, w), start, end, w, x, uw)
}

// unitStart: may the cursor sit at offset i?
func (m *multilineInput) unitStart(i int) bool {
	if i <= 0 || i >= len(m.buf) || m.buf[i] == '\n' {
		return true
	}
	return m.layout().uw[i] > 0
}

func (m *multilineInput) prevUnit(i int) int {
	if i > 0 {
		i--
	}
	for i > 0 && !m.unitStart(i) {
		i--
	}
	return i
}

func (m *multilineInput) nextUnit(i int) int {
	if i < len(m.buf) {
		i++
	}
	for i < len(m.buf) && !m.unitStart(i) {
		i++
	}
	return i
}

// cursorDisplayPos maps the flat cursor to a (row, cell column).
func (m *multilineInput) cursorDisplayPos() (int, int) {
	l := m.layout()
	segs := l.segs
	i := sort.Search(len(segs), func(i int) bool { return m.cursor < segs[i].next })
	if i == len(segs) {
		i = len(segs) - 1
	}
	s := segs[i]
	if m.cursor >= s.end {
		return i, s.w
	}
	if m.cursor <= s.start {
		return i, 0
	}
	return i, int(l.x[m.cursor])
}

// displayToOffset is the inverse: the unit under cell `col` of `row`, clamped
// to the row's own text. The second cell of a wide character is that
// character.
func (m *multilineInput) displayToOffset(row, col int) int {
	l := m.layout()
	row = max(0, min(row, len(l.segs)-1))
	s := l.segs[row]
	if col >= s.w {
		return s.end
	}
	for i := s.start; i < s.end; i++ {
		if l.uw[i] > 0 && int(l.x[i]+l.uw[i]) > col {
			return i
		}
	}
	return s.end
}

// clickAt puts the cursor under a click on visible row `row` of the box, `col`
// cells from its left edge (the two-cell prompt included).
func (m *multilineInput) clickAt(row, col int) {
	m.cursor = m.displayToOffset(m.scroll+row, col-2)
	m.clampCursor()
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

// clampCursor keeps the cursor inside the buffer and on a unit start (never
// inside a cluster), and its row on screen.
func (m *multilineInput) clampCursor() {
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor > len(m.buf) {
		m.cursor = len(m.buf)
	}
	for m.cursor > 0 && !m.unitStart(m.cursor) {
		m.cursor--
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
	m.typedAt, m.blinkOn = time.Now(), true

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
		case "ctrl+o": // open a paste token for editing
			m.expandPaste()
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
		r := cleanInput(msg.Runes)
		if msg.Type == tea.KeySpace {
			r = []rune{' '}
		}
		// A paste (bracketed, or several runes at once from a terminal
		// without it) that names an image file is that image.
		if msg.Paste || len(r) > 1 {
			if tok, ok := m.pastedImage(r); ok {
				m.insert([]rune{tok})
				return *m
			}
		}
		if msg.Paste && bigPaste(r) {
			r = []rune{m.addPaste(r)}
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
		m.cursor = m.prevUnit(m.cursor)
		m.clampCursor()
	}
}

func (m *multilineInput) cursorRight() {
	if m.cursor < len(m.buf) {
		m.cursor = m.nextUnit(m.cursor)
		m.clampCursor()
	}
}

// backspace and delete take a whole unit: a character with its combining
// marks, an emoji sequence, a paste token.
func (m *multilineInput) backspace() {
	if m.cursor > 0 {
		m.deleteTo(m.prevUnit(m.cursor))
	}
}

func (m *multilineInput) delete() {
	if m.cursor < len(m.buf) {
		m.deleteTo(m.nextUnit(m.cursor))
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
	row, _ := m.cursorDisplayPos()

	lines := make([]string, 0, m.maxLines)
	for i := 0; i < m.height(); i++ {
		idx := m.scroll + i
		prompt := "  "
		if idx == 0 {
			prompt = "› "
		}
		var text string
		if idx < len(segs) {
			caret := -1
			if m.focused && m.blinkOn && idx == row {
				caret = m.cursor
			}
			text = m.rowView(segs[idx], caret)
		}
		lines = append(lines, m.pad(prompt+text))
	}
	return strings.Join(lines, "\n")
}

// rowView draws one row unit by unit: a tab as spaces, a paste token as its
// label, the unit at `caret` (an offset; -1 for none) in reverse video, and a
// caret cell past the end when the cursor sits there.
func (m *multilineInput) rowView(s wrapSeg, caret int) string {
	l := m.layout()
	var b strings.Builder
	for i := s.start; i < s.end; {
		j := i + 1
		for j < s.end && l.uw[j] == 0 {
			j++
		}
		w := int(l.uw[i])
		var cell, rest string
		switch r := m.buf[i]; {
		case r == '\t':
			cell, rest = " ", strings.Repeat(" ", w-1)+string(m.buf[i+1:j])
		case m.isTok(r):
			label := ansi.Truncate(m.pasteLabel(r), w, "…")
			if i == caret {
				cell = label
			} else {
				rest = stDim.Render(label)
			}
		default:
			cell = string(m.buf[i:j])
		}
		if i == caret {
			b.WriteString(stCaret.Render(cell))
		} else {
			b.WriteString(cell)
		}
		b.WriteString(rest)
		i = j
	}
	if caret >= s.end {
		b.WriteString(stCaret.Render(" "))
	}
	return b.String()
}

// text is the row's text as stored (tabs as tabs, tokens as their runes).
func (m *multilineInput) text(s wrapSeg) string { return string(m.buf[s.start:s.end]) }

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
	if m.focused && ghost != "" {
		// The caret on its first cell; the rest stays ghost.
		r := []rune(ghost)
		text = stCaret.Render(string(r[0])) + stDim.Render(string(r[1:]))
	}
	return m.pad("› " + text)
}

// suggestStyle is a predicted next message: dim, and the highlighted one a
// step towards the text colour — lighter on a dark terminal, darker on a
// light one (mdStyle, resolved without querying the terminal).
func suggestStyle(selected bool) lipgloss.Style {
	if !selected {
		return stDim
	}
	if mdStyle == "light" {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
}

// withCaret reverse-videos the rune at index col of a plain one-line string,
// adding a cell past the end when that is where the cursor sits.
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
