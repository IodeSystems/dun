package tui

import (
	"strings"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// The editing keys every text field shares (USER 2026-11-04: "list out the
// other good shell-isms and ensure that we support them … a generic keyboard
// mode … that worked everywhere"). The composer (multilineInput) and every
// one-line field (textinput, through editField) run a key through editKey
// first; what it does not take, the field does itself (motions, typing).
//
//	ctrl+w                                 kill the blank-separated word before the cursor
//	alt+backspace, ctrl+backspace          kill the word before the cursor
//	alt+d, alt+delete, ctrl+delete         kill the word after it
//	ctrl+k                                 kill to the end of the line
//	ctrl+u                                 kill to the start of the line
//	ctrl+y                                 yank: put the last kill back
//	alt+y                                  right after a yank: the kill before
//	ctrl+t                                 swap the two characters at the cursor
//	alt+t                                  swap the two words at the cursor
//	alt+u, alt+l, alt+c                    upper, lower, capitalize the next word
//	ctrl+h                                 delete the character before the cursor
//	ctrl+_, ctrl+/                         undo
//
// A word is a run of letters and digits, as in readline: alt+f stops at each
// / and . of a path. ctrl+w alone takes blank-separated words
// (unix-word-rubout), so it still removes a whole path (USER 2026-11-05).
//
// Kills in a row make one ring entry, as in readline: a kill forward adds to
// its end, a kill back to its start. The ring is one for all fields, so a
// key cut from a form can be yanked into the composer.

// editRing is how many kills are kept.
const editRing = 16

// editState is the shared ring and each field's undo history.
type editState struct {
	kills    []string
	last     string // what the previous key did: "kill", "yank", "type" or ""
	lastID   string // the field it did it in
	yankIdx  int    // the ring entry the last yank put in
	yankFrom int    // where it starts; it ends at the cursor
	undo     map[string][]editSnap
}

type editSnap struct {
	buf []rune
	cur int
}

// edits is the one editState the TUI has: a terminal has one keyboard.
var edits = &editState{}

// editUndoDepth is how many states a field can go back.
const editUndoDepth = 64

// editBuf is a field's text as editKey sees it.
type editBuf struct {
	buf    []rune
	cur    int
	lo, hi int // the cursor's line: [lo, hi); a one-line field is all of it
	// Optional: a field with tokens (the composer's pastes and images) says
	// what runes stand for, and turns yanked text back into runes. Its
	// characters can be clusters: prev and next step over one.
	written    func([]rune) string
	runes      func(string) []rune
	prev, next func(int) int
}

func (e *editBuf) text(rs []rune) string {
	if e.written != nil {
		return e.written(rs)
	}
	return string(rs)
}

func (e *editBuf) prevAt(i int) int {
	if e.prev != nil {
		return e.prev(i)
	}
	return i - 1
}

func (e *editBuf) nextAt(i int) int {
	if e.next != nil {
		return e.next(i)
	}
	return i + 1
}

// editKey applies key to e, for the field named id. It reports whether the
// key was an editing key; e is changed in place.
func editKey(id, key string, e *editBuf) bool {
	st := edits
	prev := st.last
	if st.lastID != id {
		prev = "" // a kill or yank in another field does not continue here
	}
	before := editSnap{buf: e.buf, cur: e.cur}
	st.last, st.lastID = "", id
	switch key {
	case "ctrl+w":
		st.kill(e, blankStart(e.buf, e.cur), prev)
	case "alt+backspace", "ctrl+backspace":
		st.kill(e, wordStart(e.buf, e.cur), prev)
	case "alt+d", "alt+delete", "ctrl+delete":
		st.kill(e, wordEnd(e.buf, e.cur), prev)
	case "ctrl+k":
		// At the line's end the kill takes the newline: the next line joins.
		if e.cur < e.hi {
			st.kill(e, e.hi, prev)
		} else {
			st.kill(e, e.cur+1, prev)
		}
	case "ctrl+u":
		if e.cur > e.lo {
			st.kill(e, e.lo, prev)
		} else {
			st.kill(e, e.cur-1, prev)
		}
	case "ctrl+y":
		st.yank(e, len(st.kills)-1)
	case "alt+y":
		if prev != "yank" || len(st.kills) < 2 {
			st.last = prev
			return true
		}
		e.buf = splice(e.buf, st.yankFrom, e.cur, nil)
		e.cur = st.yankFrom
		st.yank(e, (st.yankIdx+len(st.kills)-1)%len(st.kills))
	case "ctrl+t":
		transposeChars(e)
	case "alt+t":
		transposeWords(e)
	case "alt+u", "alt+l", "alt+c":
		caseWord(e, key)
	case "ctrl+h":
		if e.cur > 0 {
			p := max(0, e.prevAt(e.cur))
			e.buf, e.cur = splice(e.buf, p, e.cur, nil), p
		}
	case "ctrl+_", "ctrl+/":
		st.popUndo(id, e)
		return true
	default:
		// Not an editing key: it ends a run of kills or a yank. Typing keeps
		// its run (noteTyped), so a word typed is one undo step.
		if prev == "type" {
			st.last = "type"
		}
		return false
	}
	if !equalRunes(before.buf, e.buf) {
		st.pushUndo(id, before)
	}
	return true
}

// kill deletes from the cursor to to and keeps the text in the ring.
func (st *editState) kill(e *editBuf, to int, prev string) {
	lo, hi := max(0, min(e.cur, to)), min(len(e.buf), max(e.cur, to))
	if lo >= hi {
		st.last = prev // nothing to kill: a next kill still joins
		return
	}
	text := e.text(e.buf[lo:hi])
	switch {
	case prev == "kill" && len(st.kills) > 0 && to < e.cur:
		st.kills[len(st.kills)-1] = text + st.kills[len(st.kills)-1]
	case prev == "kill" && len(st.kills) > 0:
		st.kills[len(st.kills)-1] += text
	default:
		st.kills = append(st.kills, text)
		if len(st.kills) > editRing {
			st.kills = st.kills[len(st.kills)-editRing:]
		}
	}
	e.buf, e.cur = splice(e.buf, lo, hi, nil), lo
	st.last = "kill"
}

// yank puts ring entry i in at the cursor.
func (st *editState) yank(e *editBuf, i int) {
	if i < 0 || i >= len(st.kills) {
		return
	}
	r := []rune(st.kills[i])
	if e.runes != nil {
		r = e.runes(st.kills[i])
	}
	st.yankFrom, st.yankIdx = e.cur, i
	e.buf = splice(e.buf, e.cur, e.cur, r)
	e.cur += len(r)
	st.last = "yank"
}

// noteTyped records a change the field made itself (typing, backspace, a
// paste), so undo can take it back. Characters typed in a row are one step.
func (st *editState) noteTyped(id string, before editSnap, after []rune) {
	if equalRunes(before.buf, after) {
		return
	}
	if st.last == "type" && st.lastID == id {
		return
	}
	st.pushUndo(id, before)
	st.last, st.lastID = "type", id
}

func (st *editState) pushUndo(id string, s editSnap) {
	if st.undo == nil {
		st.undo = map[string][]editSnap{}
	}
	h := append(st.undo[id], editSnap{buf: append([]rune(nil), s.buf...), cur: s.cur})
	if len(h) > editUndoDepth {
		h = h[len(h)-editUndoDepth:]
	}
	st.undo[id] = h
}

func (st *editState) popUndo(id string, e *editBuf) {
	h := st.undo[id]
	if len(h) == 0 {
		return
	}
	s := h[len(h)-1]
	st.undo[id] = h[:len(h)-1]
	e.buf, e.cur = s.buf, min(s.cur, len(s.buf))
}

// forget drops a field's undo history: its text was replaced (a message
// sent, a form opened afresh).
func (st *editState) forget(id string) { delete(st.undo, id) }

// transposeChars swaps the character before the cursor with the one under
// it and moves past both; at the end of a line it swaps the last two.
func transposeChars(e *editBuf) {
	if e.cur == e.lo || e.hi-e.lo < 2 {
		return
	}
	at := e.cur
	if at >= e.hi {
		at = e.prevAt(e.hi)
	}
	a := e.prevAt(at)
	b := e.nextAt(at)
	if a < e.lo || b > e.hi {
		return
	}
	left, right := append([]rune(nil), e.buf[a:at]...), append([]rune(nil), e.buf[at:b]...)
	e.buf = splice(e.buf, a, b, append(right, left...))
	e.cur = b
}

// transposeWords swaps the word before the cursor with the one after it
// (or, at the end, the last two), leaving the cursor after both.
func transposeWords(e *editBuf) {
	s2, e2 := wordStart(e.buf, wordEnd(e.buf, e.cur)), wordEnd(e.buf, e.cur)
	if s2 >= e2 { // no word after: the last two before the cursor
		e2 = wordEnd(e.buf, wordStart(e.buf, e.cur))
		s2 = wordStart(e.buf, e2)
	}
	e1 := s2
	for e1 > 0 && !isWordRune(e.buf[e1-1]) {
		e1--
	}
	s1 := wordStart(e.buf, e1)
	if s1 >= e1 || s2 >= e2 || s1 < e.lo || e2 > e.hi {
		return
	}
	w1, gap, w2 := e.buf[s1:e1], e.buf[e1:s2], e.buf[s2:e2]
	out := append(append(append([]rune(nil), w2...), gap...), w1...)
	e.buf = splice(e.buf, s1, e2, out)
	e.cur = e2
}

// caseWord changes the next word's case and moves past it: alt+u upper,
// alt+l lower, alt+c capital first letter, rest lower.
func caseWord(e *editBuf, key string) {
	end := min(wordEnd(e.buf, e.cur), e.hi)
	if end <= e.cur {
		return
	}
	w := append([]rune(nil), e.buf[e.cur:end]...)
	first := true
	for i, r := range w {
		switch key {
		case "alt+u":
			w[i] = unicode.ToUpper(r)
		case "alt+l":
			w[i] = unicode.ToLower(r)
		case "alt+c":
			if !isWordRune(r) {
				continue
			}
			if first {
				w[i], first = unicode.ToUpper(r), false
			} else {
				w[i] = unicode.ToLower(r)
			}
		}
	}
	e.buf = splice(e.buf, e.cur, end, w)
	e.cur = end
}

// wordStart and wordEnd are readline's words: runs of letters and digits.
func wordStart(buf []rune, i int) int {
	for i > 0 && !isWordRune(buf[i-1]) {
		i--
	}
	for i > 0 && isWordRune(buf[i-1]) {
		i--
	}
	return i
}

func wordEnd(buf []rune, i int) int {
	for i < len(buf) && !isWordRune(buf[i]) {
		i++
	}
	for i < len(buf) && isWordRune(buf[i]) {
		i++
	}
	return i
}

// blankStart is the start of the shell word before i: a run of anything but
// blanks (ctrl+w).
func blankStart(buf []rune, i int) int {
	for i > 0 && isWordSep(buf[i-1]) {
		i--
	}
	for i > 0 && !isWordSep(buf[i-1]) {
		i--
	}
	return i
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func isWordSep(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }

// splice is buf with [lo, hi) replaced by r, in a new slice: the composer's
// layout caches by the buffer's identity, so a buffer is never written in
// place.
func splice(buf []rune, lo, hi int, r []rune) []rune {
	out := make([]rune, 0, len(buf)-(hi-lo)+len(r))
	out = append(out, buf[:lo]...)
	out = append(out, r...)
	return append(out, buf[hi:]...)
}

func equalRunes(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// editField is a one-line field's Update with the shared editing keys in
// front: every textinput in the TUI goes through it, so ctrl+y in the MCP
// form is ctrl+y in the composer. id names the field's undo history.
func editField(ti *textinput.Model, id string, msg tea.Msg) tea.Cmd {
	before := editSnap{buf: []rune(ti.Value()), cur: ti.Position()}
	if k, ok := msg.(tea.KeyPressMsg); ok {
		e := editBuf{buf: before.buf, cur: before.cur, lo: 0, hi: len(before.buf)}
		// textinput's own word motions stop only at blanks.
		switch k.String() {
		case "alt+left", "ctrl+left", "alt+b":
			ti.SetCursor(wordStart(e.buf, e.cur))
			edits.last = ""
			return nil
		case "alt+right", "ctrl+right", "alt+f":
			ti.SetCursor(wordEnd(e.buf, e.cur))
			edits.last = ""
			return nil
		}
		if editKey(id, k.String(), &e) {
			if s := string(e.buf); s != ti.Value() {
				ti.SetValue(strings.ReplaceAll(s, "\n", " "))
			}
			ti.SetCursor(e.cur)
			return nil
		}
	}
	var cmd tea.Cmd
	*ti, cmd = ti.Update(msg)
	edits.noteTyped(id, before, []rune(ti.Value()))
	return cmd
}
