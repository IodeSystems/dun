package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var (
	kSpace    = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	kShiftTab = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
)

// formsAnswer is a reply with a DETAILS fold and a tabs form, prose around
// them.
var formsAnswer = "Intro line.\n\n> [!DETAILS] Full log\n> The long part, quoted.\n\nBetween.\n\n" +
	"> [!TABS]\n#### Alpha\nalpha body\n\n#### Beta\nbeta body\n\n---\n\nThe end.\n"

// formsConvo is a session at w×h with formsAnswer streamed in and finished,
// the conversation focused and the answer selected.
func formsConvo(t *testing.T, w, h int) (*vtui, int) {
	t.Helper()
	return answerConvo(t, formsAnswer, w, h)
}

// answerConvo is formsConvo with the reply src.
func answerConvo(t *testing.T, src string, w, h int) (*vtui, int) {
	t.Helper()
	v := newVtui(w, h)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(1)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.event(map[string]any{"type": "settings", "model": "m", "url": "http://llm/v1"})
	v.send("show me")
	for i := 0; i < len(src); i += 9 {
		v.event(map[string]any{"type": "token", "text": src[i:min(i+9, len(src))]})
	}
	v.event(map[string]any{"type": "done"})
	i := answerIndex(v)
	v.m = key(v.m, kTab) // focus the conversation: the newest block, the answer
	if v.m.sel != i {
		t.Fatalf("sel %d, want the answer %d", v.m.sel, i)
	}
	return v, i
}

// Enter on the selected answer gives its forms the keys; enter folds and
// opens the DETAILS, tab moves to the tabs form, → shows one tab and steps
// to the next. The screen changes each time; the answer stays selected.
func TestDoc_FoldAndTabs(t *testing.T) {
	v, i := formsConvo(t, 80, 40)
	plain := func() string { return ansi.Strip(v.view()) }
	if s := plain(); !strings.Contains(s, "The long part") || !strings.Contains(s, "alpha body") || !strings.Contains(s, "beta body") {
		t.Fatalf("streamed layout:\n%s", s)
	}
	if !strings.Contains(lastRow(v.m), "enter forms") {
		t.Errorf("status names no way in: %q", lastRow(v.m))
	}
	v.m = key(v.m, kEnter)
	if v.m.docEntry() == nil || v.m.sel != i {
		t.Fatalf("enter did not give the forms the keys (docAt %d sel %d)", v.m.docAt, v.m.sel)
	}
	if !strings.Contains(lastRow(v.m), "forms") || !strings.Contains(lastRow(v.m), "esc leave") {
		t.Errorf("status: %q", lastRow(v.m))
	}
	v.m = key(v.m, kEnter) // fold the DETAILS
	if s := plain(); strings.Contains(s, "The long part") || !strings.Contains(s, "Full log") {
		t.Fatalf("enter did not fold the details:\n%s", s)
	}
	v.m = key(v.m, kSpace) // and open it again
	if s := plain(); !strings.Contains(s, "The long part") {
		t.Fatalf("space did not open the details:\n%s", s)
	}
	v.m = key(v.m, kTab) // to the tabs form
	if v.m.focus != focusConvo || v.m.docEntry() == nil {
		t.Fatalf("tab left the forms with one form to go (focus %d)", v.m.focus)
	}
	v.m = key(v.m, kRight) // one tab: Alpha
	if s := plain(); !strings.Contains(s, "alpha body") || strings.Contains(s, "beta body") {
		t.Fatalf("→ did not show one tab:\n%s", s)
	}
	v.m = key(v.m, kRight) // Beta
	if s := plain(); strings.Contains(s, "alpha body") || !strings.Contains(s, "beta body") {
		t.Fatalf("→ did not switch the tab:\n%s", s)
	}
	v.m = key(v.m, kLeft) // back to Alpha: ← is the tab's, not "leave"
	if s := plain(); !strings.Contains(s, "alpha body") || v.m.docEntry() == nil {
		t.Fatalf("← did not switch the tab back:\n%s", s)
	}
}

// The forms' state is the reply's: a resize makes a new Doc with the same
// folds and tabs, and leaving and coming back keeps them.
func TestDoc_StateSurvives(t *testing.T) {
	v, _ := formsConvo(t, 80, 40)
	v.m = key(v.m, kEnter)
	v.m = key(v.m, kEnter) // fold
	v.m = key(v.m, kEsc)   // leave
	if v.m.docEntry() != nil || v.m.focus != focusConvo {
		t.Fatalf("esc: docAt %d focus %d", v.m.docAt, v.m.focus)
	}
	v.resize(60, 40)
	if s := ansi.Strip(v.view()); strings.Contains(s, "The long part") || !strings.Contains(s, "Full log") {
		t.Fatalf("the fold did not survive a resize:\n%s", s)
	}
	v.resize(100, 40)
	if s := ansi.Strip(v.view()); strings.Contains(s, "The long part") {
		t.Fatalf("the fold did not survive a second resize:\n%s", s)
	}
}

// Tab past the last form and shift+tab before the first leave the forms and
// stay in the conversation, the answer selected; esc does too.
func TestDoc_LeaveOneLevel(t *testing.T) {
	v, i := formsConvo(t, 80, 40)
	v.m = key(v.m, kEnter)
	v.m = key(v.m, kTab)
	v.m = key(v.m, kTab) // past the last
	if v.m.docEntry() != nil || v.m.focus != focusConvo || v.m.sel != i {
		t.Fatalf("tab past the last form: docAt %d focus %d sel %d", v.m.docAt, v.m.focus, v.m.sel)
	}
	v.m = key(v.m, kRight) // → comes back in (no wide block here)
	if v.m.docEntry() == nil {
		t.Fatal("→ did not give the forms the keys")
	}
	v.m = key(v.m, kShiftTab) // before the first (Focus returned to the last stop: two back)
	v.m = key(v.m, kShiftTab)
	if v.m.docEntry() != nil || v.m.focus != focusConvo || v.m.sel != i {
		t.Fatalf("shift+tab before the first form: docAt %d focus %d sel %d", v.m.docAt, v.m.focus, v.m.sel)
	}
	// Outside the forms, tab is the zone cycle again.
	v.m = key(v.m, kTab)
	if v.m.focus == focusConvo {
		t.Error("tab outside the forms did not move the zone")
	}
}

// ↑/↓ stay dun's scroll with the forms focused: in an answer taller than
// the pane they move the view a row and the forms keep the keys; ↑ that
// moves the selection to another block takes the keys back.
func TestDoc_UpDownScroll(t *testing.T) {
	v, i := formsConvo(t, 80, 12)
	v.m = key(v.m, kEnter)
	if v.m.docEntry() == nil {
		t.Fatal("no forms focus")
	}
	top, h := v.m.selGeom()
	if h <= v.m.vp.Height {
		t.Fatalf("the answer (%d rows) fits the pane (%d): no scroll to test", h, v.m.vp.Height)
	}
	y := v.m.vp.YOffset
	if y <= top {
		t.Fatalf("view at %d, answer top %d: want the view inside the answer", y, top)
	}
	v.m = key(v.m, kUp)
	if v.m.vp.YOffset != y-1 || v.m.docEntry() == nil || v.m.sel != i {
		t.Fatalf("↑: yoff %d (want %d), docAt %d sel %d", v.m.vp.YOffset, y-1, v.m.docAt, v.m.sel)
	}
	v.m = key(v.m, kDown)
	if v.m.vp.YOffset != y || v.m.docEntry() == nil {
		t.Fatalf("↓: yoff %d (want %d), docAt %d", v.m.vp.YOffset, y, v.m.docAt)
	}
	for k := 0; k < 40 && v.m.sel == i; k++ {
		v.m = key(v.m, kUp)
	}
	if v.m.sel == i || v.m.docAt != 0 {
		t.Fatalf("↑ off the answer: sel %d docAt %d", v.m.sel, v.m.docAt)
	}
	if st := v.m.convo[i].ans.State(); st.Focused != "" {
		t.Errorf("the answer kept its focus cue off the selection: %q", st.Focused)
	}
}

// A fold that closes is the person's action: the answer shrinks, its held
// height starts over, so no blank rows stand where the body was.
func TestDoc_FoldShrinks(t *testing.T) {
	v, i := formsConvo(t, 80, 40)
	h0 := v.m.frame.blockH[i]
	v.m = key(v.m, kEnter)
	v.m = key(v.m, kEnter)
	h1 := v.m.frame.blockH[i]
	if h1 >= h0 {
		t.Fatalf("fold: block %d rows, was %d", h1, h0)
	}
	rows := strings.Split(ansi.Strip(v.m.convo[i].collapsed), "\n")
	if got := len(rows); got != h1 {
		t.Errorf("block draws %d rows, its render has %d: held blank rows", h1, got)
	}
}

// An answer with forms and a block cut at the edge: → descends into the wide
// block (as before), enter into the forms. A sideways scroll after a fold
// keeps the fold: the natural render it cuts from has the same State.
func TestDoc_WideAndForms(t *testing.T) {
	v, i := answerConvo(t, formsAnswer+"\n"+wideAnswer, 100, 60)
	if !strings.Contains(lastRow(v.m), "→ scroll sideways · enter forms") {
		t.Errorf("status: %q", lastRow(v.m))
	}
	v.m = key(v.m, kEnter)
	v.m = key(v.m, kEnter) // fold
	v.m = key(v.m, kEsc)
	folded := ansi.Strip(v.view())
	if strings.Contains(folded, "The long part") {
		t.Fatalf("not folded:\n%s", folded)
	}
	v.m = key(v.m, kRight) // into the wide blocks, not the forms
	if v.m.insideWide() == nil || v.m.docEntry() != nil {
		t.Fatalf("→: insideWide %v docAt %d", v.m.insideWide() != nil, v.m.docAt)
	}
	for k := 0; k < 10; k++ {
		v.m = key(v.m, kRight)
	}
	s := ansi.Strip(v.view())
	if !strings.Contains(s, "// END") {
		t.Errorf("the scroll did not reach the end:\n%s", s)
	}
	if strings.Contains(s, "The long part") || len(strings.Split(s, "\n")) != len(strings.Split(folded, "\n")) {
		t.Errorf("the sideways scroll lost the fold:\n%s", s)
	}
	if v.m.convo[i].wide.off == 0 {
		t.Error("no sideways offset")
	}
}

// A focus change scrolls the focused form into view (dun's scroll: the Doc
// has no height).
func TestDoc_FocusRevealsForm(t *testing.T) {
	v, i := formsConvo(t, 80, 8)
	in := func() bool {
		s, ok := v.m.convo[i].ans.Focused()
		r := v.m.convo[i].rowOffset + s.Line - v.m.convo[i].ansLead
		return ok && r >= v.m.vp.YOffset && r < v.m.vp.YOffset+v.m.vp.Height
	}
	v.m = key(v.m, kEnter) // the DETAILS, near the answer's top
	if !in() {
		t.Fatalf("details not in view: yoff %d h %d", v.m.vp.YOffset, v.m.vp.Height)
	}
	if !strings.Contains(ansi.Strip(v.view()), "Full log") {
		t.Errorf("the details title is not on screen:\n%s", ansi.Strip(v.view()))
	}
	v.m = key(v.m, kTab) // the tabs, further down
	if !in() {
		t.Fatalf("tabs not in view: yoff %d h %d", v.m.vp.YOffset, v.m.vp.Height)
	}
	if !strings.Contains(ansi.Strip(v.view()), "Alpha · Beta") {
		t.Errorf("the tabs header is not on screen:\n%s", ansi.Strip(v.view()))
	}
}
