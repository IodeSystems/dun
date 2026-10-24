package main

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

func withBand(t *testing.T, b bandStyle) {
	t.Helper()
	old := band
	band = b
	t.Cleanup(func() { band = old })
}

func trueBG(c rgb) string { return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", c[0], c[1], c[2]) }

// A line with a code span inside plain text: each segment is shifted from its
// OWN background — the span from 236, the plain text from the theme's base —
// the default comes back after the reset, and the row fills the width.
func TestBand_FiltersEachSegmentFromItsOwnBackground(t *testing.T) {
	b := newBandStyle(colorprofile.TrueColor, true, true, true)
	s := b.strength(0)
	line := "plain \x1b[38;5;0;48;5;236mcode\x1b[0m tail"
	got := b.row(line, 30, 0)

	def := trueBG(b.shift(baseDark, s))
	code := trueBG(b.shift(xterm256(236), s))
	if !strings.HasPrefix(got, def+"plain ") {
		t.Errorf("plain text is not on the filtered default: %q", got)
	}
	if !strings.Contains(got, "\x1b[38;5;0;48;5;236m"+code+"code") {
		t.Errorf("the code span is not its own background, shifted: %q", got)
	}
	if !strings.Contains(got, "\x1b[0m"+def+" tail") {
		t.Errorf("the filtered default did not come back after the reset: %q", got)
	}
	if c := xterm256(236); b.shift(c, s)[0] <= c[0] {
		t.Errorf("a dark theme should lighten: %v → %v", c, b.shift(c, s))
	}
	if stripped := stripANSI(got); stripped != "plain code tail"+strings.Repeat(" ", 15) {
		t.Errorf("text changed or not padded to the width: %q", stripped)
	}
	if w := lipgloss.Width(got); w != 30 {
		t.Errorf("row is %d wide, want 30", w)
	}
}

// A light theme darkens, and the colour argument of a foreground is not
// mistaken for a reset.
func TestBand_LightDarkensAndForegroundZeroIsNotAReset(t *testing.T) {
	b := newBandStyle(colorprofile.TrueColor, false, true, true)
	if f := b.shift(baseLight, 0.1); f[0] >= 255 {
		t.Errorf("a light theme should darken: %v", f)
	}
	got := b.row("\x1b[48;2;10;20;30mx\x1b[38;5;0my", 2, 0)
	want := trueBG(b.shift(rgb{10, 20, 30}, b.strength(0)))
	if strings.Count(got, want) != 1 || !strings.HasSuffix(got, "y\x1b[49m") {
		t.Errorf("38;5;0 re-applied or reset the background: %q", got)
	}
}

// 256 colours: filtered to the nearest palette entry, and static. 16 colours:
// one background where the line has none, its own elsewhere. No colour: none.
func TestBand_Fallbacks(t *testing.T) {
	b := newBandStyle(colorprofile.ANSI256, true, true, true)
	if b.pulse() {
		t.Error("256 colours should not pulse")
	}
	if got := b.row("x", 1, 0); !strings.HasPrefix(got, "\x1b[48;5;") {
		t.Errorf("256: %q", got)
	}
	b = newBandStyle(colorprofile.ANSI, true, true, true)
	got := b.row("a\x1b[44mb\x1b[0mc", 3, 0)
	if got != "\x1b[100ma\x1b[44mb\x1b[0m\x1b[100mc\x1b[49m" {
		t.Errorf("16 colours: %q", got)
	}
	if b := newBandStyle(colorprofile.Ascii, true, true, true); b.on() {
		t.Error("no colour, no band")
	}
	if b := newBandStyle(colorprofile.TrueColor, true, false, true); b.on() {
		t.Error("notty style, no band")
	}
}

// The pulse moves the strength smoothly between its ends; the opt-out holds it.
func TestBand_PulseStrengthAndOptOut(t *testing.T) {
	b := newBandStyle(colorprofile.TrueColor, true, true, true)
	if b.strength(0) != pulseLo || b.strength(pulseSteps/2) != pulseHi {
		t.Errorf("ends: %v %v", b.strength(0), b.strength(pulseSteps/2))
	}
	if b.row("x", 1, 0) == b.row("x", 1, 3) {
		t.Error("the pulse does not change the band")
	}
	still := newBandStyle(colorprofile.TrueColor, true, true, false)
	if still.pulse() || !still.on() || still.row("x", 1, 0) != still.row("x", 1, 3) {
		t.Error("DUN_NO_ANIM should give a static band")
	}
}

// selectedRows are the pane rows of the selected block.
func selectedRows(m tuiModel) []string {
	top, h := m.selGeom()
	return append([]string(nil), m.vp.lines[top:top+h]...)
}

// In convo focus the selected block is banded across the pane; the pulse
// runs while it is shown, repaints only those rows, and stops on leaving.
func TestBand_PulseRunsOnlyWhileShown(t *testing.T) {
	withBand(t, newBandStyle(colorprofile.TrueColor, true, true, true))
	v := newVtui(60, 20)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("hello")
	v.event(map[string]any{"type": "token", "text": "a reply"})
	v.event(map[string]any{"type": "done"})
	if v.m.pulsePending {
		t.Fatal("pulse ticking with nothing selected")
	}

	v.m = key(v.m, kTab)
	if !v.m.pulsePending {
		t.Fatal("no pulse in convo focus with a selection")
	}
	rows := selectedRows(v.m)
	for _, r := range rows {
		if !strings.Contains(r, "\x1b[48;2;") || lipgloss.Width(r) != 60 {
			t.Fatalf("selected row not banded across the pane: %q", r)
		}
	}
	others := append([]string(nil), v.m.vp.lines...)

	nm, _ := v.m.Update(pulseMsg{})
	v.m = nm.(tuiModel)
	if v.m.pulse != 1 || !v.m.pulsePending {
		t.Fatalf("pulse did not step and re-arm: %d %v", v.m.pulse, v.m.pulsePending)
	}
	after := selectedRows(v.m)
	if after[0] == rows[0] {
		t.Error("the pulse did not change the band")
	}
	top, h := v.m.selGeom()
	for i, r := range v.m.vp.lines {
		if (i < top || i >= top+h) && r != others[i] {
			t.Errorf("row %d outside the selection was repainted", i)
		}
	}

	v.m = key(v.m, kTab) // back to the input
	nm, _ = v.m.Update(pulseMsg{})
	v.m = nm.(tuiModel)
	if v.m.pulsePending || v.m.pulse != 1 {
		t.Errorf("pulse kept running after focus left: pending=%v pulse=%d", v.m.pulsePending, v.m.pulse)
	}
	for _, r := range v.m.vp.lines {
		if strings.Contains(r, "\x1b[48;2;") {
			t.Fatalf("band left behind: %q", r)
		}
	}
}

// The pager's tree bands its selected node; esc stops the pulse.
func TestBand_PagerTreeAndEsc(t *testing.T) {
	withBand(t, newBandStyle(colorprofile.TrueColor, true, true, true))
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("go")
	for i := 0; i < 3; i++ {
		call(v, fmt.Sprintf("echo %d", i), "ok\n")
	}
	r := runIdx(v.m)
	v.m = key(v.m, kTab)
	for v.m.sel > r {
		v.m = key(v.m, kUp)
	}
	v.m = key(v.m, kEnter)
	if !v.m.paging || !v.m.pulsePending {
		t.Fatalf("paging=%v pulse=%v", v.m.paging, v.m.pulsePending)
	}
	if row := v.m.pg.rows[v.m.pg.nodeAt[v.m.pg.cur]]; !strings.Contains(row, "\x1b[48;2;") || lipgloss.Width(row) != 80 {
		t.Errorf("selected node not banded: %q", row)
	}
	v.m = key(v.m, kEsc) // back to convo focus: the band is still shown there
	v.m = key(v.m, kTab)
	nm, _ := v.m.Update(pulseMsg{})
	v.m = nm.(tuiModel)
	if v.m.pulsePending {
		t.Error("pulse kept running after the band was gone")
	}
}

// DUN_NO_ANIM: the band is drawn, nothing ticks.
func TestBand_OptOutDoesNotTick(t *testing.T) {
	withBand(t, newBandStyle(colorprofile.TrueColor, true, true, false))
	v := newVtui(60, 20)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("hello")
	v.m = key(v.m, kTab)
	if v.m.pulsePending {
		t.Error("a static band should not tick")
	}
	if rows := selectedRows(v.m); !strings.Contains(rows[0], "\x1b[48;2;") {
		t.Errorf("no static band: %q", rows[0])
	}
}
