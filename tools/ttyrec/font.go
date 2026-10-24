package main

import (
	"fmt"
	"image"
	"os"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// The fonts tried, in order. The first regular face found is the primary: it
// sets the cell size. Every face found is also a fallback for a glyph the
// faces before it lack (DejaVu Sans Mono has the box drawing; Symbola has
// most of the dingbats). Go Mono is built in and always last, so a render
// never fails for want of a font.
var defaultFonts = []fontFile{
	{"DejaVu Sans Mono", "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf", "/usr/share/fonts/truetype/dejavu/DejaVuSansMono-Bold.ttf"},
	{"Noto Sans Mono", "/usr/share/fonts/truetype/noto/NotoSansMono-Regular.ttf", "/usr/share/fonts/truetype/noto/NotoSansMono-Bold.ttf"},
	{"DejaVu Sans", "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf"},
	{"Symbola", "/usr/share/fonts/truetype/ancient-scripts/Symbola_hint.ttf", ""},
	{"Noto Sans Symbols 2", "/usr/share/fonts/truetype/noto/NotoSansSymbols2-Regular.ttf", ""},
}

type fontFile struct{ name, regular, bold string }

// face is one loaded font at the render size, regular and (maybe) bold.
type face struct {
	name          string
	regular, bold *sfnt.Font
	fr, fb        font.Face
}

// fontSet renders one cell's grapheme: it picks the first face that has the
// glyph and caches the resulting mask per (text, bold, cells).
type fontSet struct {
	faces        []*face
	cellW, cellH int
	ascent       int
	size         float64
	cache        map[glyphKey]*image.Alpha
	buf          sfnt.Buffer
	Primary      string   // the face that set the cell size
	Used         []string // every face that drew at least one glyph
	used         map[string]bool
}

type glyphKey struct {
	s     string
	bold  bool
	cells int
}

// loadFonts loads the faces in files that exist (plus Go Mono) at size px.
// override, when set, is a path tried first.
func loadFonts(size float64, override string) (*fontSet, error) {
	files := defaultFonts
	if override != "" {
		files = append([]fontFile{{override, override, ""}}, files...)
	}
	fs := &fontSet{size: size, cache: map[glyphKey]*image.Alpha{}, used: map[string]bool{}}
	for _, f := range files {
		b, err := os.ReadFile(f.regular)
		if err != nil {
			continue
		}
		fc, err := newFace(f.name, b, f.bold, size)
		if err != nil {
			continue
		}
		fs.faces = append(fs.faces, fc)
	}
	gm, err := newFaceBytes("Go Mono", gomono.TTF, gomonobold.TTF, size)
	if err != nil {
		return nil, err
	}
	fs.faces = append(fs.faces, gm)

	p := fs.faces[0]
	fs.Primary = p.name
	adv, ok := p.fr.GlyphAdvance('M')
	if !ok {
		return nil, fmt.Errorf("font %s has no M", p.name)
	}
	m := p.fr.Metrics()
	fs.cellW = adv.Ceil()
	fs.ascent = m.Ascent.Ceil()
	fs.cellH = fs.ascent + m.Descent.Ceil()
	return fs, nil
}

func newFace(name string, regular []byte, boldPath string, size float64) (*face, error) {
	var bold []byte
	if boldPath != "" {
		bold, _ = os.ReadFile(boldPath)
	}
	return newFaceBytes(name, regular, bold, size)
}

func newFaceBytes(name string, regular, bold []byte, size float64) (*face, error) {
	r, err := opentype.Parse(regular)
	if err != nil {
		return nil, err
	}
	opts := &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull}
	fr, err := opentype.NewFace(r, opts)
	if err != nil {
		return nil, err
	}
	f := &face{name: name, regular: r, fr: fr}
	if bold != nil {
		if b, err := opentype.Parse(bold); err == nil {
			if fb, err := opentype.NewFace(b, opts); err == nil {
				f.bold, f.fb = b, fb
			}
		}
	}
	return f, nil
}

// has reports whether f draws every rune of s (a variation selector or a
// zero-width joiner is not required).
func (fs *fontSet) has(f *sfnt.Font, s string) bool {
	for _, r := range s {
		if r == 0xFE0F || r == 0xFE0E || r == 0x200D {
			continue
		}
		i, err := f.GlyphIndex(&fs.buf, r)
		if err != nil || i == 0 {
			return false
		}
	}
	return true
}

// glyph returns the coverage mask for s drawn into a box cells wide, or nil
// for a blank.
func (fs *fontSet) glyph(s string, bold bool, cells int) *image.Alpha {
	if s == "" || s == " " {
		return nil
	}
	k := glyphKey{s, bold, cells}
	if m, ok := fs.cache[k]; ok {
		return m
	}
	if m := boxGlyph(s, fs.cellW, fs.cellH, bold); m != nil {
		fs.cache[k] = m
		return m
	}
	var pick *face
	for _, f := range fs.faces {
		if fs.has(f.regular, s) {
			pick = f
			break
		}
	}
	if pick == nil {
		pick = fs.faces[len(fs.faces)-1] // draws the .notdef box
	}
	if !fs.used[pick.name] {
		fs.used[pick.name] = true
		fs.Used = append(fs.Used, pick.name)
	}
	fc := pick.fr
	fakeBold := false
	if bold {
		if pick.fb != nil && fs.has(pick.bold, s) {
			fc = pick.fb
		} else {
			fakeBold = true
		}
	}
	w := fs.cellW * cells
	m := image.NewAlpha(image.Rect(0, 0, w, fs.cellH))
	adv := font.MeasureString(fc, s)
	x := fixed.I(0)
	if adv < fixed.I(w) && pick.name != fs.Primary {
		x = (fixed.I(w) - adv) / 2 // a fallback glyph is centred in its cells
	}
	d := font.Drawer{Dst: m, Src: image.Opaque, Face: fc, Dot: fixed.Point26_6{X: x, Y: fixed.I(fs.ascent)}}
	d.DrawString(s)
	if fakeBold {
		d.Dot = fixed.Point26_6{X: x + fixed.I(1), Y: fixed.I(fs.ascent)}
		d.DrawString(s)
	}
	fs.cache[k] = m
	return m
}

// fontNote is the line recorded in render metadata.
func (fs *fontSet) fontNote() string {
	return fmt.Sprintf("%s %gpx (cell %dx%d); glyphs from: %s", fs.Primary, fs.size, fs.cellW, fs.cellH, strings.Join(fs.Used, ", "))
}
