package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// The selection band: the selected block's rows are drawn through a
// background FILTER across the whole pane, on top of the ▎ gutter, so the
// selection can be found at a glance.
//
// A filter, not a colour. A block is not one background: a code span, a diff
// line or a badge brings its own. bandRow reads the line's SGR state as it
// goes and, wherever a background is in effect, replaces it with that same
// background shifted — lightened on a dark theme, darkened on a light one.
// Where none is set, it shifts the theme's base instead. A reset brings the
// filtered default back, so the band survives the renderers' habit of ending
// every styled run with \x1b[0m, and the row is padded to the pane width so
// the band is a solid rectangle.
//
// The pulse animates the filter's STRENGTH (pulseLo..pulseHi) on a slow
// cosine, which is what keeps a shift of a few percent visible on any
// underlying colour. Only truecolor can do that exactly. On a 256-colour
// terminal the shifted colour is matched to the nearest palette entry, and
// since neighbouring strengths often land on the same entry, it is static. On
// 16 colours there is no shifting at all: one static background where the
// line has none. Without colour, only the gutter.
//
// The pulse costs a frame per step, so it runs only while there is a band to
// pulse — convo focus with a selection, or the pager's tree — and stops the
// moment there is not (pulseWanted). A step rewrites only the selected rows
// (repaintBand); everything else in the pane is untouched, and the renderer's
// cell diff sends just those cells. DUN_NO_ANIM=1 keeps the band and drops the
// pulse.

const (
	pulseSteps = 10
	pulseStep  = 120 * time.Millisecond // 10 × 120ms = a 1.2s cycle
	pulseLo    = 0.03                   // filter strength at the pulse's ends: subtle
	pulseHi    = 0.08                   // (0.06..0.14 drowned dim text: USER 2026-10-24)
)

type bandMode int

const (
	bandOff  bandMode = iota
	bandANSI          // 16 colours: one static background where the line has none
	band256           // filtered, matched to the 256 palette, static
	bandTrue          // filtered exactly, pulsing unless asked not to
)

type bandStyle struct {
	mode bandMode
	dark bool // lighten (dark theme) or darken (light theme)
	anim bool
}

// band is resolved once at TUI start (setupBand). The zero value draws no band,
// which is what a test that does not ask for one gets.
var band bandStyle

func (b bandStyle) on() bool    { return b.mode != bandOff }
func (b bandStyle) pulse() bool { return b.mode == bandTrue && b.anim }

// strength is the filter's strength at pulse step phase: a cosine between
// pulseLo and pulseHi, which eases at both ends so it breathes, not blinks.
func (b bandStyle) strength(phase int) float64 {
	if !b.pulse() {
		return (pulseLo + pulseHi) / 2
	}
	p := float64(((phase % pulseSteps) + pulseSteps) % pulseSteps)
	return pulseLo + (pulseHi-pulseLo)*(1-math.Cos(2*math.Pi*p/pulseSteps))/2
}

// setupBand resolves the band for this terminal. Call after
// initMarkdownStyle, which decides dark or light.
func setupBand() {
	band = newBandStyle(colorprofile.Detect(os.Stdout, os.Environ()), mdStyle != "light", mdStyle != "notty", os.Getenv("DUN_NO_ANIM") != "1")
}

func newBandStyle(p colorprofile.Profile, dark, color, anim bool) bandStyle {
	if !color {
		return bandStyle{}
	}
	b := bandStyle{dark: dark, anim: anim}
	switch p {
	case colorprofile.TrueColor:
		b.mode = bandTrue
	case colorprofile.ANSI256:
		b.mode = band256
	case colorprofile.ANSI:
		b.mode = bandANSI
	}
	return b
}

type rgb [3]int

// The theme's base: what "no background" is taken to be.
var (
	baseDark  = rgb{30, 30, 30}
	baseLight = rgb{255, 255, 255}
)

// shift is the filter: c moved s of the way toward white (dark theme) or
// black (light theme).
func (b bandStyle) shift(c rgb, s float64) rgb {
	var out rgb
	for i := range c {
		v := float64(c[i])
		if b.dark {
			v += (255 - v) * s
		} else {
			v *= 1 - s
		}
		out[i] = int(math.Round(v))
	}
	return out
}

// bg is the escape that puts the filtered c (nil = the default) on screen.
func (b bandStyle) bg(c *rgb, s float64) string {
	switch b.mode {
	case bandANSI:
		if c != nil {
			return "" // the line's own background stands
		}
		if b.dark {
			return "\x1b[100m" // bright black
		}
		return "\x1b[47m"
	case band256, bandTrue:
		base := baseLight
		if b.dark {
			base = baseDark
		}
		if c != nil {
			base = *c
		}
		f := b.shift(base, s)
		if b.mode == band256 {
			return fmt.Sprintf("\x1b[48;5;%dm", nearest256(f))
		}
		return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", f[0], f[1], f[2])
	}
	return ""
}

// row draws one display row of width w through the filter at pulse step
// phase: the filtered default first, the filtered background again after
// every escape that changes or clears it, and filtered padding out to w.
func (b bandStyle) row(row string, w, phase int) string {
	if !b.on() {
		return row
	}
	s := b.strength(phase)
	var cur *rgb // the line's own background; nil = default
	var out strings.Builder
	out.Grow(len(row)*2 + w)
	out.WriteString(b.bg(nil, s))
	for i := 0; i < len(row); {
		if row[i] == 0x1b && i+1 < len(row) && row[i+1] == '[' {
			j := i + 2
			for j < len(row) && (row[j] < 0x40 || row[j] > 0x7e) {
				j++
			}
			if j >= len(row) {
				out.WriteString(row[i:])
				break
			}
			out.WriteString(row[i : j+1])
			if row[j] == 'm' {
				if next, touched := sgrBG(row[i+2:j], cur); touched {
					cur = next
					out.WriteString(b.bg(cur, s))
				}
				out.WriteString(liftGrey(row[i+2 : j]))
			}
			i = j + 1
			continue
		}
		out.WriteByte(row[i])
		i++
	}
	if pad := w - lipgloss.Width(row); pad > 0 { // the filtered background is already in effect
		out.WriteString(strings.Repeat(" ", pad))
	}
	out.WriteString("\x1b[49m")
	return out.String()
}

func (b bandStyle) rows(rows []string, w, phase int) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = b.row(r, w, phase)
	}
	return out
}

// greyLift is how many steps of the 256-colour grey ramp a foreground on the
// band is raised, so dim text keeps its contrast against the lifted
// background: the band boosts its content as well as marking it (USER
// 2026-10-24: "a little of both").
const greyLift = 6

// liftGrey is the escape that raises a grey-ramp foreground (38;5;232..255)
// set by params, or "" when params set none.
func liftGrey(params string) string {
	ps := strings.Split(params, ";")
	for i := 0; i+2 < len(ps); i++ {
		if ps[i] == "38" && ps[i+1] == "5" {
			if n, err := strconv.Atoi(ps[i+2]); err == nil && n >= 232 && n <= 255 {
				return fmt.Sprintf("\x1b[38;5;%dm", min(n+greyLift, 255))
			}
			i += 2
		} else if ps[i] == "38" || ps[i] == "48" || ps[i] == "58" {
			if ps[i+1] == "2" {
				i += 4
			} else {
				i += 2
			}
		}
	}
	return ""
}

// sgrBG applies one SGR's parameters to the background state: a reset (bare,
// 0) or 49 clears it, 40–47 / 100–107 / 48;5;n / 48;2;r;g;b set it. touched
// says the background was named at all. The colour arguments of 38 and 58
// are skipped, so the 0 in "38;5;0" is a colour, not a reset.
func sgrBG(params string, cur *rgb) (*rgb, bool) {
	if params == "" {
		return nil, true
	}
	ps := strings.Split(params, ";")
	touched := false
	num := func(i int) int {
		if i < len(ps) {
			n, _ := strconv.Atoi(ps[i])
			return n
		}
		return 0
	}
	for i := 0; i < len(ps); i++ {
		n, err := strconv.Atoi(ps[i])
		if err != nil && ps[i] != "" {
			continue
		}
		switch {
		case ps[i] == "" || n == 0 || n == 49:
			cur, touched = nil, true
		case n >= 40 && n <= 47:
			c := ansi16[n-40]
			cur, touched = &c, true
		case n >= 100 && n <= 107:
			c := ansi16[n-100+8]
			cur, touched = &c, true
		case n == 38 || n == 48 || n == 58:
			var c rgb
			skip := 0
			switch num(i + 1) {
			case 5:
				c, skip = xterm256(num(i+2)), 2
			case 2:
				c, skip = rgb{num(i + 2), num(i + 3), num(i + 4)}, 4
			}
			if n == 48 && skip > 0 {
				cur, touched = &c, true
			}
			i += skip
		}
	}
	return cur, touched
}

// ansi16 is xterm's default 16-colour palette.
var ansi16 = [16]rgb{
	{0, 0, 0}, {205, 0, 0}, {0, 205, 0}, {205, 205, 0}, {0, 0, 238}, {205, 0, 205}, {0, 205, 205}, {229, 229, 229},
	{127, 127, 127}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0}, {92, 92, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255},
}

var cubeLevels = [6]int{0, 95, 135, 175, 215, 255}

// xterm256 is the RGB of a 256-palette index.
func xterm256(n int) rgb {
	switch {
	case n < 0 || n > 255:
		return rgb{}
	case n < 16:
		return ansi16[n]
	case n < 232:
		n -= 16
		return rgb{cubeLevels[n/36], cubeLevels[(n/6)%6], cubeLevels[n%6]}
	}
	g := 8 + (n-232)*10
	return rgb{g, g, g}
}

// nearest256 is the palette index closest to c, from the cube and the grey
// ramp (the first 16 are the terminal's to redefine, so they are not used).
func nearest256(c rgb) int {
	best, bestD := 16, math.MaxInt
	for n := 16; n < 256; n++ {
		p := xterm256(n)
		d := 0
		for i := range c {
			d += (c[i] - p[i]) * (c[i] - p[i])
		}
		if d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

// ── the pulse ──────────────────────────────────────────────────────

type pulseMsg struct{}

func pulseTick() tea.Cmd {
	return tea.Tick(pulseStep, func(time.Time) tea.Msg { return pulseMsg{} })
}

// bandShown says whether a band is on screen: a selection in convo focus, or
// the pager's tree.
func (m tuiModel) bandShown() bool {
	if !band.on() || m.inspecting {
		return false
	}
	if m.paging {
		return m.pg.tree()
	}
	return m.focus == focusConvo && !m.asking && m.sel >= 0 && m.sel < len(m.convo)
}

// pulseWanted is whether the pulse clock should be running.
func (m tuiModel) pulseWanted() bool { return band.pulse() && m.bandShown() }

// repaintBand moves the selected rows to the current pulse step without
// re-rendering anything else.
func (m *tuiModel) repaintBand() {
	if m.paging {
		m.syncPager() // the pager re-lays itself out; its rows are few
		return
	}
	raw, at := m.bandRaw, m.bandAt
	if raw == nil || at < 0 || at+len(raw) > len(m.vp.lines) {
		return
	}
	copy(m.vp.lines[at:], band.rows(raw, m.vp.Width, m.pulse))
	m.contentGen++
}
