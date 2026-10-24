package main

import (
	"image"
	"image/color"
	"image/draw"
	"io"
	"strings"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// The colours a render assumes for "default": the same pair the emulator
// reports when a program asks (OSC 10/11), so a program that picks a dark or
// light theme from the answer is drawn in the theme it picked.
var (
	defaultFg = color.RGBA{0xd4, 0xd4, 0xd4, 0xff}
	defaultBg = color.RGBA{0x16, 0x18, 0x1c, 0xff}
)

// term is a vt emulator plus what a recorder needs around it: the replies it
// generates (cursor reports, colour answers) are handed to reply, or
// discarded, so a write never blocks on them.
type term struct {
	mu            sync.Mutex
	em            *vt.Emulator
	cursorVisible bool
}

func newTerm(cols, rows int, reply io.Writer) *term {
	t := &term{em: vt.NewEmulator(cols, rows), cursorVisible: true}
	t.em.SetDefaultForegroundColor(defaultFg)
	t.em.SetDefaultBackgroundColor(defaultBg)
	t.em.SetDefaultCursorColor(defaultFg)
	t.em.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(v bool) { t.cursorVisible = v },
	})
	if reply == nil {
		reply = io.Discard
	}
	go func() { _, _ = io.Copy(reply, t.em) }()
	return t
}

func (t *term) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.em.Write(p)
}

// text is the screen as plain text: one line per row, trailing blanks cut,
// trailing empty rows dropped.
func (t *term) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	w, h := t.em.Width(), t.em.Height()
	lines := make([]string, h)
	for y := 0; y < h; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			c := t.em.CellAt(x, y)
			if c == nil {
				b.WriteByte(' ')
				continue
			}
			if c.Width == 0 && c.Content == "" {
				continue // the right half of a wide character
			}
			if c.Content == "" {
				b.WriteByte(' ')
			} else {
				b.WriteString(c.Content)
			}
		}
		lines[y] = strings.TrimRight(b.String(), " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// resolve turns a cell colour into RGBA; nil is the default.
func (t *term) resolve(c color.Color, def color.RGBA) color.RGBA {
	if c == nil {
		return def
	}
	r, g, b, _ := c.RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 0xff}
}

func blend(a, b color.RGBA, f float64) color.RGBA {
	m := func(x, y uint8) uint8 { return uint8(float64(x)*(1-f) + float64(y)*f + 0.5) }
	return color.RGBA{m(a.R, b.R), m(a.G, b.G), m(a.B, b.B), 0xff}
}

// render draws the screen. alphaLevels > 0 rounds glyph coverage to that many
// steps, which bounds the colours a frame uses (a GIF has 256).
func (t *term) render(fs *fontSet, alphaLevels int) *image.RGBA {
	t.mu.Lock()
	defer t.mu.Unlock()
	w, h := t.em.Width(), t.em.Height()
	img := image.NewRGBA(image.Rect(0, 0, w*fs.cellW, h*fs.cellH))
	draw.Draw(img, img.Rect, image.NewUniform(defaultBg), image.Point{}, draw.Src)
	cur := t.em.CursorPosition()
	showCursor := t.cursorVisible
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := t.em.CellAt(x, y)
			if c == nil {
				continue
			}
			if c.Width == 0 && c.Content == "" {
				continue
			}
			cells := max(c.Width, 1)
			st := c.Style
			fg := t.resolve(st.Fg, defaultFg)
			bg := t.resolve(st.Bg, defaultBg)
			if st.Attrs&uv.AttrReverse != 0 {
				fg, bg = bg, fg
			}
			if showCursor && cur.X == x && cur.Y == y {
				fg, bg = bg, fg
			}
			if st.Attrs&uv.AttrFaint != 0 {
				fg = blend(fg, bg, 0.45)
			}
			r := image.Rect(x*fs.cellW, y*fs.cellH, (x+cells)*fs.cellW, (y+1)*fs.cellH)
			if bg != defaultBg {
				draw.Draw(img, r, image.NewUniform(bg), image.Point{}, draw.Src)
			}
			if st.Attrs&uv.AttrConceal != 0 {
				continue
			}
			bold := st.Attrs&uv.AttrBold != 0
			if m := fs.glyph(c.Content, bold, cells); m != nil {
				if alphaLevels > 0 {
					m = quantize(m, alphaLevels)
				}
				draw.DrawMask(img, r, image.NewUniform(fg), image.Point{}, m, image.Point{}, draw.Over)
			}
			if st.Underline != 0 {
				uy := r.Min.Y + fs.ascent + 1
				ul := fg
				if st.UnderlineColor != nil {
					ul = t.resolve(st.UnderlineColor, fg)
				}
				draw.Draw(img, image.Rect(r.Min.X, uy, r.Max.X, uy+1), image.NewUniform(ul), image.Point{}, draw.Src)
			}
			if st.Attrs&uv.AttrStrikethrough != 0 {
				sy := r.Min.Y + fs.cellH/2
				draw.Draw(img, image.Rect(r.Min.X, sy, r.Max.X, sy+1), image.NewUniform(fg), image.Point{}, draw.Src)
			}
			x += cells - 1
		}
	}
	return img
}

var quantCache = map[*image.Alpha]*image.Alpha{}

func quantize(m *image.Alpha, levels int) *image.Alpha {
	if q, ok := quantCache[m]; ok {
		return q
	}
	q := image.NewAlpha(m.Rect)
	step := 255.0 / float64(levels)
	for i, a := range m.Pix {
		n := int(float64(a)/step + 0.5)
		q.Pix[i] = uint8(min(255, float64(n)*step+0.5))
	}
	quantCache[m] = q
	return q
}
