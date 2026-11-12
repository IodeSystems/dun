package tui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	glamouransi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/iodesystems/bubbleteandmd"
)

// Answers render through bubbleteandmd: one Doc (a Stream with interactive
// forms, docfocus.go) per reply, Frame per token, Final once at the end. Blocks before the tail are rendered once and
// kept in answerCache, so a frame costs the tail, not the reply; a finished
// reply rendered again (a resize, a replayed history) is the cache's.
//
// The renderer wraps prose at the prose column (proseW) and, with
// WideBlocks(pane), renders a top-level code block or table wider than that
// at its own width, each row cut at the pane with "… +N cols →" (wide.go
// has the why). A Stream's key names the style, the prose column and the
// pane: a new one of any of them is a new Renderer and a new Doc, which takes
// the old Doc's State (folds, tabs, focus: keyed by byte offset in the
// source, the same at every width).

// answerCache is the rendered-block cache, one per program (mutex-safe).
var answerCache = bubbleteandmd.NewCache(512)

// natPane is the pane a natural render is cut at: bubbleteandmd's widest
// wide block, so nothing a sideways scroll can reach is cut.
const natPane = 512

// answerRenderersMax bounds the renderers an answerKit keeps; past it the
// map is dropped. A handful of (prose, pane) widths in a session.
const answerRenderersMax = 32

// mdStyleConfig is mdStyle as a glamour style.
func mdStyleConfig() glamouransi.StyleConfig {
	if s, ok := styles.DefaultStyles[mdStyle]; ok {
		return *s
	}
	return styles.DarkStyleConfig
}

// warmAnswerRenderer builds the first renderer. bubbleteandmd registers its
// chroma formatter there, once; it must happen before any other goroutine
// renders code, so it runs with the style, before the program starts.
func warmAnswerRenderer() {
	_, _ = bubbleteandmd.NewRenderer(mdStyleConfig(), 78)
}

// answerKit is a model's answer renderers, by prose column and pane, and
// the streaming reply's Doc.
type answerKit struct {
	rs     map[[2]int]*bubbleteandmd.DirectiveRenderer
	cur    *bubbleteandmd.Doc // the reply streaming now (m.cur)
	curKey string
}

func (m *tuiModel) kit() *answerKit {
	if m.ans == nil {
		m.ans = &answerKit{}
	}
	return m.ans
}

// answerKey names what a render depends on besides the source.
func answerKey(proseW, pane int) string { return fmt.Sprintf("%s/%d/%d", mdStyle, proseW, pane) }

// doc is a new Doc for prose column proseW and pane, and its key; nil when
// no renderer can be built (the caller shows the source). st, when not nil,
// is the State it renders in (a copy: the old Doc of a reply at another
// width, or the Doc a prefix or natural render must match).
func (k *answerKit) doc(proseW, pane int, st *bubbleteandmd.State) (*bubbleteandmd.Doc, string) {
	at := [2]int{proseW, pane}
	r, ok := k.rs[at]
	if !ok {
		initMarkdownStyle() // no-op after the first call
		var err error
		if r, err = bubbleteandmd.NewRenderer(mdStyleConfig(), proseW); err != nil {
			return nil, ""
		}
		r.WideBlocks(pane)
		if k.rs == nil || len(k.rs) >= answerRenderersMax {
			k.rs = map[[2]int]*bubbleteandmd.DirectiveRenderer{}
		}
		k.rs[at] = r
	}
	key := answerKey(proseW, pane)
	d := bubbleteandmd.NewDoc(r, key, answerCache)
	if st != nil {
		*d.State() = *st
	}
	return d, key
}

// stateOf is d's State, nil for no Doc.
func stateOf(d *bubbleteandmd.Doc) *bubbleteandmd.State {
	if d == nil {
		return nil
	}
	return d.State()
}

// answerOut renders src through s: a frame while it streams, the final
// render at the end. On an error (or no Doc) it is the source, sanitized.
func answerOut(s *bubbleteandmd.Doc, src string, final bool) string {
	out, _ := answerOutLead(s, src, final)
	return out
}

// answerOutLead is answerOut and the blank lines it trimmed from the top:
// the Doc's stop lines count them.
func answerOutLead(s *bubbleteandmd.Doc, src string, final bool) (string, int) {
	if s == nil {
		return bubbleteandmd.Sanitize(src), 0
	}
	in := demoteStepHeads(src)
	var out string
	var err error
	if final {
		out, err = s.Final(in)
	} else {
		out, err = s.Frame(in)
	}
	if err != nil {
		return bubbleteandmd.Sanitize(src), 0
	}
	top := strings.TrimLeft(out, "\n")
	return strings.TrimRight(top, "\n"), len(out) - len(top)
}

// curAnswer renders the streaming reply (m.cur) for a pane avail columns
// wide: a Frame of its Doc, made again (State kept) when the widths moved.
func (m *tuiModel) curAnswer(avail int) string {
	k := m.kit()
	pw := m.proseW()
	if key := answerKey(pw, avail); k.cur == nil || k.curKey != key {
		k.cur, k.curKey = k.doc(pw, avail, stateOf(k.cur))
	}
	return answerOut(k.cur, m.cur, false)
}

// finishAnswer is the finished reply src as an entry: the streaming reply's
// Doc ends with Final (it must: a frame can hide a partial table row),
// and the entry keeps it for the renders to come.
func (m *tuiModel) finishAnswer(src string) convoEntry {
	e := convoEntry{mdSource: src}
	k := m.kit()
	if k.cur != nil {
		e.ans, e.ansKey = k.cur, k.curKey
		e.collapsed, e.ansLead = answerOutLead(e.ans, src, true)
	} else {
		e.collapsed = m.renderEntry(&e, max(1, m.vp.Width-1))
	}
	k.cur, k.curKey = nil, ""
	return e
}

// edgeNoteRe is bubbleteandmd's note at a cut row's right edge.
var edgeNoteRe = regexp.MustCompile(`… \+(\d+) cols →\s*$`)

// cutMax is the largest sideways offset that still shows something new in
// out, rendered cut at the pane: 0 when nothing is cut. The note says how
// many cells are hidden past the cut (N); the cut is at least 12 cells
// before the pane's edge, so N+4 scrolls the widest row fully in with the
// "…" at its left.
func cutMax(out string) int {
	if !strings.Contains(out, "cols →") {
		return 0
	}
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if mm := edgeNoteRe.FindStringSubmatch(ansi.Strip(l)); mm != nil {
			v, _ := strconv.Atoi(mm[1])
			n = max(n, v)
		}
	}
	if n == 0 {
		return 0
	}
	return n + wideIndicatorRoom - 12
}

// rowsOf is how many rows src renders to at pane avail in State st,
// trailing blank rows left out. The blocks come from answerCache.
func (m *tuiModel) rowsOf(src string, avail int, st *bubbleteandmd.State) int {
	s, _ := m.kit().doc(m.proseW(), avail, st)
	rows := strings.Split(answerOut(s, src, true), "\n")
	for len(rows) > 0 && strings.TrimSpace(ansi.Strip(rows[len(rows)-1])) == "" {
		rows = rows[:len(rows)-1]
	}
	return len(rows)
}

// panAnswer is answer src scrolled sideways by off: cut is its render at
// off 0 (cut at the pane by bubbleteandmd). The rows of each wide block
// that has a cut row are taken from the natural render (cut at natPane)
// and cut at avail from column off (cutRow); prose rows stay as they are.
// A block's rows are found by rendering the source up to it and through it:
// the renderer joins blocks as a whole render does, so a prefix's rows are
// the whole render's first rows. Every render here is in State st (the
// reply's folds and tabs), so it has the layout of cut.
func (m *tuiModel) panAnswer(src, cut string, avail, off int, st *bubbleteandmd.State) string {
	ns, _ := m.kit().doc(m.proseW(), natPane, st)
	nat := strings.Split(answerOut(ns, src, true), "\n")
	rows := strings.Split(cut, "\n")
	if len(nat) != len(rows) {
		return cut // not the same layout: show it unscrolled
	}
	var pre []string
	for _, s := range splitWide(src) {
		if s.wide {
			p := 0
			if len(pre) > 0 {
				p = m.rowsOf(strings.Join(pre, "\n"), avail, st)
			}
			q := min(m.rowsOf(strings.Join(append(pre, s.text), "\n"), avail, st), len(rows))
			cutHere := false
			for i := p; i < q && !cutHere; i++ {
				cutHere = rows[i] != nat[i]
			}
			if cutHere {
				for i := p; i < q; i++ {
					rows[i] = cutRow(trimRightCells(nat[i]), off, avail)
				}
			}
		}
		pre = append(pre, s.text)
	}
	return strings.Join(rows, "\n")
}
