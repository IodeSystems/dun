package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/x/ansi"
	dun "github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// The request stats on the top bar, right side (owner, 2026-10-27: "quick
// stats when there's room on the topbar … realtime ctx len"):
//
//	dun · dun ⎇ main · chat          ctx 41.3k/131k · cache 96% · ttft 0.8s · pp 1.2k/s · tg 45/s
//
// The figures are the last turn round's (dun/reqstats.go says what each one
// measures and where); tg and ctx move while a reply streams. Each keeps its
// last value until a new one arrives: a request's start used to clear them to
// "–", so between requests — most of the time — the bar said nothing (USER
// 2026-10-28). pp and tg also show the session's average, weighted by tokens
// (processed for pp, generated for tg), after a "~": tg  45/s ~41.
//
// No bounce: every piece has a fixed width (its number padded), so a number
// changing never moves the others, and which pieces fit depends only on the
// width of the terminal and of the title. The title is never cut for them.
// As the width shrinks they go in the order pp, ttft, cache, ctx, tg; under
// narrowW there are none. cache is absent until the provider has reported
// cached tokens once in the session: an unreported cache is not 0%.

// reqBar is what the TUI keeps of the request events.
type reqBar struct {
	seen      bool // a request event arrived
	r         proto.Request
	cacheSeen bool // some request reported the cache field

	// The last value of each figure, kept across requests.
	ttft     int64
	pp, tg   float64
	cachePct int
	cacheHas bool
	// Token-weighted sums for the averages: Σ rate·tokens and Σ tokens.
	ppSum, ppN, tgSum, tgN float64
}

// requestEvent is the engine's figures as the protocol's event.
func requestEvent(r dun.RequestStat) *proto.Request {
	return &proto.Request{Phase: r.Phase, TTFTMS: r.TTFTMS, Prompt: r.Prompt, Cached: r.Cached,
		CacheReported: r.CacheReported, PPS: r.PPS, PPSServer: r.PPSServer, Gen: r.Gen, TGS: r.TGS,
		TGSServer: r.TGSServer, Exact: r.Exact, Ctx: r.Ctx, Window: r.Window}
}

// noteRequest folds one request event in.
func (b *reqBar) noteRequest(ev evMsg) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	var r proto.Request
	if json.Unmarshal(raw, &r) != nil {
		return
	}
	b.seen, b.r = true, r
	if r.CacheReported {
		b.cacheSeen = true
	}
	if r.TTFTMS > 0 {
		b.ttft = r.TTFTMS
	}
	if r.PPS > 0 {
		b.pp = r.PPS
	}
	if r.TGS > 0 {
		b.tg = r.TGS
	}
	if r.Phase != "done" {
		return
	}
	if r.CacheReported && r.Prompt > 0 {
		b.cachePct, b.cacheHas = int(math.Round(float64(r.Cached)*100/float64(r.Prompt))), true
	}
	if n := float64(r.Prompt - r.Cached); r.PPS > 0 && n > 0 {
		b.ppSum, b.ppN = b.ppSum+r.PPS*n, b.ppN+n
	}
	if n := float64(r.Gen); r.TGS > 0 && n > 0 {
		b.tgSum, b.tgN = b.tgSum+r.TGS*n, b.tgN+n
	}
}

// avg is a token-weighted average, "" before there is one.
func avg(sum, n float64) string {
	if n <= 0 {
		return ""
	}
	return "~" + tokK(int(sum/n))
}

// reqPiece is one stat: its label and its value padded to a fixed width.
type reqPiece struct {
	label, value string
	width        int // the value's padded width
	drop         int // lower goes first
}

func (p reqPiece) render() string {
	return stDim.Render(p.label+" ") + fmt.Sprintf("%*s", p.width, p.value)
}

func (p reqPiece) cells() int { return ansi.StringWidth(p.label) + 1 + p.width }

const reqDash = "–"

// pieces are the stats in display order.
func (b reqBar) pieces() []reqPiece {
	r := b.r
	dash := func(ok bool, s string) string {
		if !ok {
			return reqDash
		}
		return s
	}
	ctx := reqPiece{label: "ctx", value: tokK(r.Ctx), width: 5, drop: 3}
	if r.Window > 0 {
		w := tokK(r.Window)
		ctx.value = fmt.Sprintf("%5s/%s", tokK(r.Ctx), w)
		ctx.width = 6 + len(w)
	}
	out := []reqPiece{ctx}
	if b.cacheSeen {
		out = append(out, reqPiece{label: "cache", value: dash(b.cacheHas, fmt.Sprintf("%d%%", b.cachePct)), width: 4, drop: 2})
	}
	// A rate and its average, each padded, so neither moves the other.
	withAvg := func(v float64, a string, w int) string {
		return fmt.Sprintf("%*s %5s", w, dash(v > 0, rate(v)), a)
	}
	out = append(out,
		reqPiece{label: "ttft", value: dash(b.ttft > 0, ttftText(b.ttft)), width: 5, drop: 1},
		reqPiece{label: "pp", value: withAvg(b.pp, avg(b.ppSum, b.ppN), 7), width: 13, drop: 0},
		reqPiece{label: "tg", value: withAvg(b.tg, avg(b.tgSum, b.tgN), 6), width: 12, drop: 4},
	)
	return out
}

// ttftText is a wait the short way, always in seconds so its width holds
// still: 0.8s, 12.3s, 140s.
func ttftText(ms int64) string {
	s := float64(ms) / 1000
	if s >= 100 {
		return fmt.Sprintf("%ds", int(s))
	}
	return fmt.Sprintf("%.1fs", s)
}

// view is the stats that fit in room cells, or "" when none do or the bar
// is narrower than narrowW.
func (b reqBar) view(w, room int) string {
	if !b.seen || w < narrowW {
		return ""
	}
	ps := b.pieces()
	sep := 3 // " · "
	width := func(ps []reqPiece) int {
		n := 0
		for i, p := range ps {
			if i > 0 {
				n += sep
			}
			n += p.cells()
		}
		return n
	}
	for len(ps) > 0 && width(ps) > room {
		low := 0
		for i, p := range ps {
			if p.drop < ps[low].drop {
				low = i
			}
		}
		ps = append(ps[:low:low], ps[low+1:]...)
	}
	if len(ps) == 0 {
		return ""
	}
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.render()
	}
	return strings.Join(parts, stDim.Render(" · "))
}

// withStats is the title with the stats right-aligned after it, at least
// two cells apart; the title alone when they do not fit.
func (b reqBar) withStats(head string, w int) string {
	hw := ansi.StringWidth(head)
	stats := b.view(w, w-hw-2)
	if stats == "" {
		return head
	}
	return head + strings.Repeat(" ", w-hw-ansi.StringWidth(stats)) + stats
}
