package tui

import (
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

func ctrlK(c rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: c, Mod: tea.ModCtrl} }
func altK(c rune) tea.KeyPressMsg  { return tea.KeyPressMsg{Code: c, Mod: tea.ModAlt} }

// ctrl+k twice kills two lines into one entry and ctrl+y puts both back;
// alt+y after a yank swaps in the kill before (readline's kill ring).
func TestEdit_KillRing(t *testing.T) {
	edits = &editState{}
	in := newMultilineInput()
	in.SetValue("one\ntwo")
	in.cursor = 0
	in = in.HandleKey(ctrlK('k')) // "one"
	in = in.HandleKey(ctrlK('k')) // the newline
	if in.Value() != "two" || len(edits.kills) != 1 || edits.kills[0] != "one\n" {
		t.Fatalf("after two kills: %q, ring %q", in.Value(), edits.kills)
	}
	in.cursor = len(in.buf)
	in = in.HandleKey(ctrlK('y'))
	if in.Value() != "twoone\n" {
		t.Fatalf("yank: %q", in.Value())
	}

	// Words killed back join at the front; a key between starts a new entry.
	in.SetValue("alpha beta gamma")
	in.cursor = len(in.buf)
	in = in.HandleKey(ctrlK('w'))
	in = in.HandleKey(ctrlK('w'))
	if got := edits.kills[len(edits.kills)-1]; got != "beta gamma" {
		t.Fatalf("two words back: %q", got)
	}
	in = in.HandleKey(ctrlK('y'))
	in = in.HandleKey(altK('y'))
	if in.Value() != "alpha one\n" {
		t.Fatalf("alt+y swaps in the kill before: %q", in.Value())
	}
	in = in.HandleKey(ctrlK('a'))
	in = in.HandleKey(altK('y')) // not right after a yank: nothing
	if in.Value() != "alpha one\n" {
		t.Errorf("alt+y after a motion changed the text: %q", in.Value())
	}
}

// The ring is one for every field: a word cut from a form yanks into the
// composer, and the form's own keys are the composer's.
func TestEdit_OneRingForEveryField(t *testing.T) {
	edits = &editState{}
	f := textinput.New()
	f.Focus()
	f.SetValue("https://x.example sk-secret")
	f.CursorEnd()
	editField(&f, "form", ctrlK('w'))
	if f.Value() != "https://x.example " {
		t.Fatalf("ctrl+w in a field: %q", f.Value())
	}
	in := newMultilineInput()
	in = in.HandleKey(ctrlK('y'))
	if in.Value() != "sk-secret" {
		t.Errorf("yank in the composer: %q", in.Value())
	}
	editField(&f, "form", ctrlK('_'))
	if f.Value() != "https://x.example sk-secret" {
		t.Errorf("undo in a field: %q", f.Value())
	}
}

// ctrl+t, alt+t, alt+u/l/c and undo, as a shell has them.
func TestEdit_TransposeCaseUndo(t *testing.T) {
	edits = &editState{}
	cases := []struct {
		text string
		cur  int
		key  tea.KeyPressMsg
		want string
		at   int
	}{
		{"teh", 2, ctrlK('t'), "the", 3}, // swap before and under, move past
		{"teh", 3, ctrlK('t'), "the", 3}, // at the end: the last two
		{"b a", 3, altK('t'), "a b", 3},  // the last two words
		{"one two", 0, altK('u'), "ONE two", 3},
		{"ONE two", 0, altK('l'), "one two", 3},
		{"oNE two", 0, altK('c'), "One two", 3},
		{"abc", 3, ctrlK('h'), "ab", 2},
	}
	for _, c := range cases {
		in := newMultilineInput()
		in.SetValue(c.text)
		in.cursor = c.cur
		in = in.HandleKey(c.key)
		if in.Value() != c.want || in.cursor != c.at {
			t.Errorf("%q at %d, %s: %q at %d; want %q at %d", c.text, c.cur, c.key, in.Value(), in.cursor, c.want, c.at)
		}
	}
	in := newMultilineInput()
	for _, r := range "hello world" {
		in = in.HandleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	in = in.HandleKey(ctrlK('w'))
	in = in.HandleKey(ctrlK('_'))
	if in.Value() != "hello world" {
		t.Fatalf("undo the kill: %q", in.Value())
	}
	in = in.HandleKey(ctrlK('_'))
	if in.Value() != "" {
		t.Errorf("undo the typing, one step: %q", in.Value())
	}
}
