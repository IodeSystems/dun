package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// ctrl+c with text empties the composer and ctrl+y brings it back; ctrl+p
// and ctrl+n walk the messages sent, as ctrl+up and ctrl+down do.
func TestComposerKeys_ShellMeanings(t *testing.T) {
	edits = &editState{}
	v := newVtui(80, 24)
	v.m.history, v.m.histIdx = []string{"first", "second"}, 2
	v.m = typeStr(v.m, "draft")
	v.m = key(v.m, ctrlK('c'))
	if got := v.m.input.Value(); got != "" || v.m.quitting {
		t.Fatalf("ctrl+c with text: %q, quitting %v", got, v.m.quitting)
	}
	v.m = key(v.m, ctrlK('y'))
	if got := v.m.input.Value(); got != "draft" {
		t.Fatalf("ctrl+y after ctrl+c: %q", got)
	}
	v.m.input.Reset()
	v.m = key(v.m, ctrlK('p'))
	if got := v.m.input.Value(); got != "second" {
		t.Fatalf("ctrl+p: %q", got)
	}
	v.m = key(v.m, ctrlK('p'))
	v.m = key(v.m, ctrlK('n'))
	if got := v.m.input.Value(); got != "second" {
		t.Errorf("ctrl+p ctrl+p ctrl+n: %q", got)
	}
	v.m = key(v.m, ctrlK('t')) // transpose now, not the thinking toggle
	if got := v.m.input.Value(); got != "secodn" {
		t.Errorf("ctrl+t at the end: %q", got)
	}
}

// ctrl+x waits for its next key; a chord bound to nothing says so and types
// nothing.
func TestComposerKeys_UnboundChord(t *testing.T) {
	v := newVtui(80, 24)
	v.m = key(v.m, ctrlK('x'))
	if v.m.chord != "ctrl+x" {
		t.Fatalf("chord %q", v.m.chord)
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: 'q', Text: "q"})
	if v.m.chord != "" || v.m.input.Value() != "" {
		t.Fatalf("chord %q, input %q", v.m.chord, v.m.input.Value())
	}
	if last := stripANSI(v.m.convo[len(v.m.convo)-1].collapsed); !strings.Contains(last, "ctrl+x q does nothing") {
		t.Errorf("no word on the unbound chord: %q", last)
	}
}
