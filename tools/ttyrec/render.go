package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// renderMeta is written beside the outputs as render.json.
type renderMeta struct {
	Cast      string     `json:"cast"`
	Cols      int        `json:"cols"`
	Rows      int        `json:"rows"`
	Font      string     `json:"font"`
	CellW     int        `json:"cell_w"`
	CellH     int        `json:"cell_h"`
	GIF       string     `json:"gif,omitempty"`
	GIFFrames int        `json:"gif_frames,omitempty"`
	Duration  float64    `json:"duration_s"`
	Marks     []markFile `json:"marks"`
}

type markFile struct {
	N     int     `json:"n"`
	Label string  `json:"label"`
	T     float64 `json:"t"`
	PNG   string  `json:"png"`
	TXT   string  `json:"txt"`
}

type renderOpts struct {
	gifPath  string
	markDir  string
	fontSize float64
	fontPath string
	fps      float64
	maxIdle  float64
}

func cmdRender(args []string) error {
	fl := flag.NewFlagSet("render", flag.ExitOnError)
	var o renderOpts
	fl.StringVar(&o.gifPath, "gif", "", "write an animated GIF here")
	fl.StringVar(&o.markDir, "marks", "", "write NN-<label>.png and .txt for each marker into this directory")
	fl.Float64Var(&o.fontSize, "font-size", 16, "font size in pixels")
	fl.StringVar(&o.fontPath, "font", "", "a TTF/OTF to try before the built-in list")
	fl.Float64Var(&o.fps, "fps", 12, "most GIF frames per second")
	fl.Float64Var(&o.maxIdle, "max-idle", 1.5, "longest pause kept in the GIF, seconds")
	fl.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ttyrec render [flags] file.cast")
		fl.PrintDefaults()
	}
	_ = fl.Parse(args)
	if fl.NArg() != 1 {
		fl.Usage()
		return errors.New("want one cast file")
	}
	meta, err := render(fl.Arg(0), o)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(meta, "", "  ")
	fmt.Println(string(b))
	return nil
}

func render(castPath string, o renderOpts) (*renderMeta, error) {
	f, err := os.Open(castPath)
	if err != nil {
		return nil, err
	}
	h, evs, err := readCast(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	if o.fontSize == 0 {
		o.fontSize = 16
	}
	if o.fps == 0 {
		o.fps = 12
	}
	if o.maxIdle == 0 {
		o.maxIdle = 1.5
	}
	fs, err := loadFonts(o.fontSize, o.fontPath)
	if err != nil {
		return nil, err
	}
	t := newTerm(h.Width, h.Height, nil)
	meta := &renderMeta{Cast: filepath.Base(castPath), Cols: h.Width, Rows: h.Height, CellW: fs.cellW, CellH: fs.cellH}
	if o.markDir != "" {
		if err := os.MkdirAll(o.markDir, 0o755); err != nil {
			return nil, err
		}
	}

	var g *gifBuilder
	if o.gifPath != "" {
		g = &gifBuilder{interval: 1 / o.fps}
	}
	// The GIF runs on a compressed clock: every gap between events is cut to
	// maxIdle, so a wait for a slow answer does not become dead air.
	clock, prevT := 0.0, 0.0
	dirty := false
	for i, e := range evs {
		dt := e.T - prevT
		prevT = e.T
		if dt > 0 {
			clock += min(dt, o.maxIdle)
		}
		switch e.Code {
		case "o":
			_, _ = t.Write([]byte(e.Data))
			dirty = true
		case "r":
			var c, r int
			if _, err := fmt.Sscanf(e.Data, "%dx%d", &c, &r); err == nil {
				t.mu.Lock()
				t.em.Resize(c, r)
				t.mu.Unlock()
				dirty = true
			}
		case "m":
			n := len(meta.Marks) + 1
			base := fmt.Sprintf("%02d-%s", n, slug(e.Data))
			mf := markFile{N: n, Label: e.Data, T: e.T}
			if o.markDir != "" {
				mf.PNG, mf.TXT = base+".png", base+".txt"
				if err := writePNG(filepath.Join(o.markDir, mf.PNG), t.render(fs, 0)); err != nil {
					return nil, err
				}
				if err := os.WriteFile(filepath.Join(o.markDir, mf.TXT), []byte(markText(t.text())), 0o644); err != nil {
					return nil, err
				}
			}
			meta.Marks = append(meta.Marks, mf)
		}
		// A frame when the screen changed and the next event is far enough
		// off that this state is worth showing (or it is the last event).
		if g != nil && dirty {
			gap := o.maxIdle
			if i+1 < len(evs) {
				gap = min(evs[i+1].T-e.T, o.maxIdle)
			}
			if len(g.times) == 0 || clock-g.lastT >= g.interval || gap >= g.interval || i+1 == len(evs) {
				g.add(t.render(fs, 4), clock)
				dirty = false
			}
		}
	}
	meta.Duration = roundT(clock)
	if g != nil {
		if dirty {
			g.add(t.render(fs, 4), clock)
		}
		if err := g.write(o.gifPath); err != nil {
			return nil, err
		}
		meta.GIF, meta.GIFFrames = filepath.Base(o.gifPath), len(g.g.Image)
	}
	meta.Font = fs.fontNote()
	if o.markDir != "" {
		b, _ := json.MarshalIndent(meta, "", "  ")
		if err := os.WriteFile(filepath.Join(o.markDir, "render.json"), append(b, '\n'), 0o644); err != nil {
			return nil, err
		}
	}
	return meta, nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		s = "mark"
	}
	return s
}

func writePNG(path string, img image.Image) error {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		return err
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// markText is the screen, then the labels on it a person can act on. A
// terminal has no buttons to enumerate, so the labels are read off the text:
// key hints ("enter send"), commands ("/login"), checkboxes and options.
func markText(screen string) string {
	var b strings.Builder
	b.WriteString("--- THE KEYS AND CONTROLS THE SCREEN NAMES ---\n")
	labels := screenLabels(screen)
	if len(labels) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, l := range labels {
		b.WriteString("  " + l + "\n")
	}
	b.WriteString("\n--- EVERY WORD VISIBLE ON THE SCREEN ---\n")
	b.WriteString(screen)
	b.WriteString("\n")
	return b.String()
}

var (
	keyHintRe = regexp.MustCompile(`^((?:ctrl|alt|shift)\+\S+|enter|esc|tab|space|backspace|pgup|pgdn|[↑↓←→](?:/[↑↓←→])?|ctrl\+[↑↓←→](?:/[↑↓←→])?)\s+(.+)$`)
	cmdRe     = regexp.MustCompile(`(?:^|\s)(/[a-z][a-z0-9-]*)`)
	checkRe   = regexp.MustCompile(`\[[ xX✓]\]\s+[^\[\]]+`)
	optRe     = regexp.MustCompile(`\([ •*]\)\s+[^()]+?(?:\s{2,}|$)`)
	splitRe   = regexp.MustCompile(`\s+·\s+|\s{2,}`)
)

func screenLabels(screen string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(kind, s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		l := "[" + kind + "] " + s
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	for _, line := range strings.Split(screen, "\n") {
		for _, frag := range splitRe.Split(strings.TrimSpace(line), -1) {
			if m := keyHintRe.FindStringSubmatch(frag); m != nil {
				add("key", m[1]+": "+m[2])
			}
		}
		for _, m := range checkRe.FindAllString(line, -1) {
			add("checkbox", m)
		}
		for _, m := range optRe.FindAllString(line, -1) {
			add("option", m)
		}
	}
	var cmds []string
	for _, ix := range cmdRe.FindAllStringSubmatchIndex(screen, -1) {
		c := screen[ix[2]:ix[3]]
		if ix[3] < len(screen) && screen[ix[3]] == '/' {
			continue // a path (/tmp/…), not a command
		}
		if !seen["[command] "+c] {
			seen["[command] "+c] = true
			cmds = append(cmds, c)
		}
	}
	for _, c := range cmds {
		out = append(out, "[command] "+c)
	}
	return out
}

// gifBuilder collects frames as paletted rectangles: each frame after the
// first is only the region that changed, over the frame before it.
type gifBuilder struct {
	g        gif.GIF
	prev     *image.RGBA
	times    []float64
	lastT    float64
	interval float64
}

func (b *gifBuilder) add(img *image.RGBA, t float64) {
	r := img.Rect
	if b.prev != nil {
		r = diffRect(b.prev, img)
		if r.Empty() {
			return
		}
	}
	b.g.Image = append(b.g.Image, paletted(img, r))
	b.g.Disposal = append(b.g.Disposal, gif.DisposalNone)
	b.times = append(b.times, t)
	b.prev, b.lastT = img, t
}

func (b *gifBuilder) write(path string) error {
	if len(b.g.Image) == 0 {
		return errors.New("no frames")
	}
	for i := range b.g.Image {
		d := 2.0 // the last frame holds
		if i+1 < len(b.times) {
			d = b.times[i+1] - b.times[i]
		}
		b.g.Delay = append(b.g.Delay, max(2, int(d*100+0.5)))
	}
	b.g.Config = image.Config{Width: b.g.Image[0].Rect.Dx(), Height: b.g.Image[0].Rect.Dy()}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := gif.EncodeAll(f, &b.g); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func diffRect(a, b *image.RGBA) image.Rectangle {
	r := image.Rectangle{}
	w := b.Rect.Dx()
	for y := 0; y < b.Rect.Dy(); y++ {
		row := y * b.Stride
		if bytes.Equal(a.Pix[row:row+w*4], b.Pix[row:row+w*4]) {
			continue
		}
		x0, x1 := -1, -1
		for x := 0; x < w; x++ {
			o := row + x*4
			if !bytes.Equal(a.Pix[o:o+4], b.Pix[o:o+4]) {
				if x0 < 0 {
					x0 = x
				}
				x1 = x
			}
		}
		r = r.Union(image.Rect(x0, y, x1+1, y+1))
	}
	return r
}

// paletted converts r of img: exact when it has 256 colours or fewer (the
// usual case: glyph coverage is rounded to a few levels), else the 256 most
// used colours and the nearest of them for the rest.
func paletted(img *image.RGBA, r image.Rectangle) *image.Paletted {
	counts := map[color.RGBA]int{}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			counts[img.RGBAAt(x, y)]++
		}
	}
	cols := make([]color.RGBA, 0, len(counts))
	for c := range counts {
		cols = append(cols, c)
	}
	sort.Slice(cols, func(i, j int) bool {
		if counts[cols[i]] != counts[cols[j]] {
			return counts[cols[i]] > counts[cols[j]]
		}
		ci, cj := cols[i], cols[j]
		return uint32(ci.R)<<16|uint32(ci.G)<<8|uint32(ci.B) < uint32(cj.R)<<16|uint32(cj.G)<<8|uint32(cj.B)
	})
	if len(cols) > 256 {
		cols = cols[:256]
	}
	pal := make(color.Palette, len(cols))
	idx := map[color.RGBA]uint8{}
	for i, c := range cols {
		pal[i] = c
		idx[c] = uint8(i)
	}
	p := image.NewPaletted(r, pal)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := img.RGBAAt(x, y)
			i, ok := idx[c]
			if !ok {
				i = uint8(pal.Index(c))
				idx[c] = i
			}
			p.Pix[p.PixOffset(x, y)] = i
		}
	}
	return p
}
