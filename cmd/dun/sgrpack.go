package main

import (
	"bytes"
	"os"
	"strings"
)

// packSGR drops redundant colour escapes from rendered text.
//
// Measured, from a streaming replay: a repainted line reading "settled reply
// 26" cost ~150 bytes, because the renderers above dun style every WORD
// separately — `ESC[38;5;252m` before it and `ESC[0m` after, including around
// each trailing pad space. A 35-line region repaint came to 28KB for maybe 2KB
// of text. That overhead is paid on every frame, by every path: normal line
// diffs, scroll inserts, region syncs.
//
// The fix is a peephole, not a rewrite: a run of escapes with no text between
// them has one net effect, and if that effect is what the terminal is already
// in, none of them need to be sent. Nothing else is touched — cursor moves,
// margins, erases and every other sequence pass through byte for byte, in
// order, so the renderer's idea of the screen stays exactly right.
//
// It runs on the rows refresh() hands the pane, which is where the bytes come
// from for every path that paints the conversation: the renderer's line diff,
// the region's scroll inserts, and a full region sync alike.
//
// State is assumed to start at the terminal default for each string packed,
// which is conservative: a leading escape that was already redundant survives,
// but nothing a line depends on is ever dropped.
type sgrPacker struct {
	active string // SGR params the text is in, as last kept ("" = default)
}

// packSGR removes colour escapes that change nothing, line by line — each line
// is packed independently because the renderer writes them independently.
// DUN_PACK=0 turns packing off, for the same reason DUN_FAST_SCROLL=0 exists:
// an output transform that guesses wrong about a terminal needs a switch that
// is not "use an older dun".
var packOff = os.Getenv("DUN_PACK") == "0"

func packSGR(s string) string {
	if packOff || !strings.Contains(s, "\x1b[") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		p := &sgrPacker{}
		lines[i] = p.pack(ln)
	}
	return strings.Join(lines, "\n")
}

func (p *sgrPacker) pack(line string) string {
	b := []byte(line)
	var out bytes.Buffer
	out.Grow(len(b))
	for i := 0; i < len(b); {
		if b[i] != 0x1b {
			j := bytes.IndexByte(b[i:], 0x1b)
			if j < 0 {
				out.Write(b[i:])
				break
			}
			out.Write(b[i : i+j])
			i += j
			continue
		}
		// A run of back-to-back escapes collapses to its net SGR state; the
		// first non-SGR escape ends the run and is written untouched.
		state, consumed, ok := p.foldRun(b[i:], &out)
		if !ok { // truncated escape: pass the rest through untouched
			out.Write(b[i:])
			break
		}
		i += consumed
		if state != nil {
			p.emit(&out, *state)
		}
	}
	return out.String()
}

// foldRun consumes consecutive escape sequences at the front of b. SGR
// sequences are folded into a net state (returned, to be emitted only if it
// differs from what the terminal is in); anything else is written straight to
// out — after flushing any SGR folded so far, so ordering is preserved.
func (p *sgrPacker) foldRun(b []byte, out *bytes.Buffer) (state *string, consumed int, ok bool) {
	var net *string
	for consumed < len(b) && b[consumed] == 0x1b {
		params, seqLen, isSGR, complete := parseEscape(b[consumed:])
		if !complete {
			if net != nil {
				p.emit(out, *net)
			}
			return nil, 0, false
		}
		if !isSGR {
			if net != nil {
				p.emit(out, *net)
				net = nil
			}
			out.Write(b[consumed : consumed+seqLen])
			consumed += seqLen
			continue
		}
		s := foldSGR(deref(net, p.active), params)
		net = &s
		consumed += seqLen
	}
	return net, consumed, true
}

func deref(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// emit writes the shortest escape that moves the terminal from p.active to
// want — nothing at all when it is already there, which is the whole point.
func (p *sgrPacker) emit(out *bytes.Buffer, want string) {
	if want == p.active {
		return
	}
	if want == "" {
		out.WriteString("\x1b[0m")
	} else {
		out.WriteString("\x1b[" + want + "m")
	}
	p.active = want
}

// foldSGR applies one SGR sequence's params to a state. A reset (0, or no
// params at all) clears it; anything else layers on top. Layering is
// approximate for exotic combinations, and deliberately so: the fallback is
// carrying a param that was already implied, which costs bytes, never colour.
func foldSGR(state, params string) string {
	if params == "" || params == "0" || strings.HasPrefix(params, "0;") {
		if params == "" || params == "0" {
			return ""
		}
		return strings.TrimPrefix(params, "0;")
	}
	if state == "" {
		return params
	}
	return state + ";" + params
}

// parseEscape measures the escape sequence at the front of b, reporting
// whether it is an SGR (CSI … m) and its parameters.
func parseEscape(b []byte) (params string, length int, isSGR, complete bool) {
	if len(b) < 2 {
		return "", 0, false, false
	}
	if b[1] != '[' { // OSC, SS3, ESC 7/8 … not ours to touch
		return "", escapeLen(b), false, escapeLen(b) > 0
	}
	for i := 2; i < len(b); i++ {
		c := b[i]
		if c >= 0x40 && c <= 0x7e { // final byte
			return string(b[2:i]), i + 1, c == 'm', true
		}
		if c < 0x20 || c > 0x3f { // not a parameter byte: malformed, hand it back
			return "", i + 1, false, true
		}
	}
	return "", 0, false, false
}

// escapeLen measures a non-CSI escape, returning 0 when it is incomplete.
func escapeLen(b []byte) int {
	switch {
	case len(b) < 2:
		return 0
	case b[1] == ']': // OSC: ends at BEL or ST
		if i := bytes.IndexByte(b, 0x07); i >= 0 {
			return i + 1
		}
		if i := bytes.Index(b, []byte("\x1b\\")); i > 0 {
			return i + 2
		}
		return 0
	case b[1] == 'P' || b[1] == 'X' || b[1] == '^' || b[1] == '_': // DCS/SOS/PM/APC
		if i := bytes.Index(b, []byte("\x1b\\")); i > 0 {
			return i + 2
		}
		return 0
	default:
		return 2 // ESC 7, ESC 8, ESC M, SS3 …
	}
}
