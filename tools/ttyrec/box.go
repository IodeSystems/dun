package main

import "image"

// Box drawing and block elements are drawn, not taken from a font: a font's
// box glyph spans its own ascent and descent, which leaves a pixel gap
// between rows when the cell is rounded up — a TUI's borders then render as
// dashes. Drawn here they meet edge to edge, as in a real terminal.

// arms: weight of the up, down, left, right arm (0 none, 1 light, 2 heavy,
// 3 double).
var boxArms = map[rune][4]uint8{
	'─': {0, 0, 1, 1}, '━': {0, 0, 2, 2}, '│': {1, 1, 0, 0}, '┃': {2, 2, 0, 0},
	'┌': {0, 1, 0, 1}, '┏': {0, 2, 0, 2}, '┐': {0, 1, 1, 0}, '┓': {0, 2, 2, 0},
	'└': {1, 0, 0, 1}, '┗': {2, 0, 0, 2}, '┘': {1, 0, 1, 0}, '┛': {2, 0, 2, 0},
	'├': {1, 1, 0, 1}, '┣': {2, 2, 0, 2}, '┤': {1, 1, 1, 0}, '┫': {2, 2, 2, 0},
	'┬': {0, 1, 1, 1}, '┳': {0, 2, 2, 2}, '┴': {1, 0, 1, 1}, '┻': {2, 0, 2, 2},
	'┼': {1, 1, 1, 1}, '╋': {2, 2, 2, 2},
	'╭': {0, 1, 0, 1}, '╮': {0, 1, 1, 0}, '╯': {1, 0, 1, 0}, '╰': {1, 0, 0, 1},
	'╴': {0, 0, 1, 0}, '╵': {1, 0, 0, 0}, '╶': {0, 0, 0, 1}, '╷': {0, 1, 0, 0},
	'╸': {0, 0, 2, 0}, '╹': {2, 0, 0, 0}, '╺': {0, 0, 0, 2}, '╻': {0, 2, 0, 0},
	'═': {0, 0, 3, 3}, '║': {3, 3, 0, 0}, '╔': {0, 3, 0, 3}, '╗': {0, 3, 3, 0},
	'╚': {3, 0, 0, 3}, '╝': {3, 0, 3, 0}, '╠': {3, 3, 0, 3}, '╣': {3, 3, 3, 0},
	'╦': {0, 3, 3, 3}, '╩': {3, 0, 3, 3}, '╬': {3, 3, 3, 3},
}

func boxGlyph(s string, w, h int, bold bool) *image.Alpha {
	rs := []rune(s)
	if len(rs) != 1 {
		return nil
	}
	r := rs[0]
	if arms, ok := boxArms[r]; ok {
		return drawArms(arms, w, h, bold)
	}
	if r >= 0x2580 && r <= 0x2595 {
		return drawBlock(r, w, h)
	}
	return nil
}

func fillRect(m *image.Alpha, x0, y0, x1, y1 int, a uint8) {
	for y := max(y0, 0); y < min(y1, m.Rect.Dy()); y++ {
		for x := max(x0, 0); x < min(x1, m.Rect.Dx()); x++ {
			m.Pix[y*m.Stride+x] = a
		}
	}
}

func drawArms(arms [4]uint8, w, h int, bold bool) *image.Alpha {
	m := image.NewAlpha(image.Rect(0, 0, w, h))
	light := max(1, w/9)
	if bold {
		light++
	}
	heavy := light * 2
	cx, cy := w/2, h/2
	thick := func(wt uint8) int {
		if wt == 2 {
			return heavy
		}
		return light
	}
	// horizontal arms
	for i, wt := range []uint8{arms[2], arms[3]} {
		if wt == 0 {
			continue
		}
		x0, x1 := 0, cx+heavy/2+1
		if i == 1 {
			x0, x1 = cx-heavy/2, w
		}
		if wt == 3 {
			fillRect(m, x0, cy-light-1, x1, cy-1, 255)
			fillRect(m, x0, cy+1, x1, cy+light+1, 255)
			continue
		}
		t := thick(wt)
		fillRect(m, x0, cy-t/2, x1, cy-t/2+t, 255)
	}
	// vertical arms
	for i, wt := range []uint8{arms[0], arms[1]} {
		if wt == 0 {
			continue
		}
		y0, y1 := 0, cy+heavy/2+1
		if i == 1 {
			y0, y1 = cy-heavy/2, h
		}
		if wt == 3 {
			fillRect(m, cx-light-1, y0, cx-1, y1, 255)
			fillRect(m, cx+1, y0, cx+light+1, y1, 255)
			continue
		}
		t := thick(wt)
		fillRect(m, cx-t/2, y0, cx-t/2+t, y1, 255)
	}
	return m
}

func drawBlock(r rune, w, h int) *image.Alpha {
	m := image.NewAlpha(image.Rect(0, 0, w, h))
	eighthsH := func(n int) int { return (h*n + 4) / 8 }
	eighthsW := func(n int) int { return (w*n + 4) / 8 }
	switch {
	case r == 0x2580: // ▀
		fillRect(m, 0, 0, w, h/2, 255)
	case r >= 0x2581 && r <= 0x2588: // ▁..█ lower n/8
		n := int(r-0x2581) + 1
		fillRect(m, 0, h-eighthsH(n), w, h, 255)
	case r >= 0x2589 && r <= 0x258F: // ▉..▏ left 7/8..1/8
		n := 8 - int(r-0x2588)
		fillRect(m, 0, 0, eighthsW(n), h, 255)
	case r == 0x2590: // ▐
		fillRect(m, w/2, 0, w, h, 255)
	case r >= 0x2591 && r <= 0x2593: // ░▒▓
		fillRect(m, 0, 0, w, h, uint8(64*int(r-0x2590)))
	case r == 0x2594: // ▔
		fillRect(m, 0, 0, w, eighthsH(1), 255)
	case r == 0x2595: // ▕
		fillRect(m, w-eighthsW(1), 0, w, h, 255)
	default:
		return nil
	}
	return m
}
