package tui

import (
	"fmt"
	"strings"

	"github.com/iodesystems/dun/internal/numfmt"
	"github.com/iodesystems/dun/internal/textfmt"
)

// A turn's cost, under the message that started it (USER 2026-10-24: "how
// much a turn costs in terms of time and usage cost ... displayed when you
// enter into a message"). The engine sends it with the turn's usage event
// (turn_stats, dun/roundstats.go); enter on the message opens it:
//
//	turn · 6 rounds · 41.2s: model 29.8s (prefill 9.1s, 61.0k at 6.7k/s · generate 20.7s, 1.4k) · tools and waits 11.4s
//	     412.0k in (351.0k cached) · 1.4k out · $0.031 · incl. 1 sub-agent
//
// A turn the engine ran on its own (a job's completion, a message queued
// mid-turn) adds its own line to the same message. The numbers include the
// turn's sub-agents and side calls, as its spend does: they share its
// clients, and a round does not say who asked.

// turnStatsOf reads a usage event's turn_stats; ok=false when the engine
// sent none or the turn made no request.
func turnStatsOf(ev evMsg) (map[string]any, bool) {
	ts, _ := ev["turn_stats"].(map[string]any)
	return ts, ts != nil && evNum(ts["rounds"]) > 0
}

// turnStatsLine is a turn's cost as one or two rows (the second indented);
// background marks a turn the engine ran on its own.
func turnStatsLine(ts map[string]any, background bool) string {
	n := func(k string) float64 { return evNum(ts[k]) }
	rounds := int(n("rounds"))
	wall, model := n("wall_ms"), n("model_ms")
	head := "turn"
	if background {
		head = "then on its own"
	}
	head += " · " + textfmt.Plural(rounds, "round") + " · " + numfmt.Secs(wall)
	timeRow := ": model " + numfmt.Secs(model)
	if timed := int(n("timed")); timed > 0 && timed == rounds {
		pn, pms, gn, gms := n("prompt_n"), n("prompt_ms"), n("gen_n"), n("gen_ms")
		parts := []string{"prefill " + numfmt.Secs(pms) + ", " + numfmt.TokN(int(pn))}
		if pms > 0 && pn > 0 {
			parts[0] += " at " + numfmt.Rate(pn/(pms/1000))
		}
		parts = append(parts, "generate "+numfmt.Secs(gms)+", "+numfmt.TokN(int(gn)))
		timeRow += " (" + strings.Join(parts, " · ") + ")"
	}
	if rest := wall - model; wall > 0 && rest >= 500 {
		timeRow += " · tools and waits " + numfmt.Secs(rest)
	}
	tokens := []string{numfmt.TokN(int(n("prompt"))) + " in"}
	if c, p := int(n("cached")), n("prompt"); c > 0 && p > 0 {
		tokens[0] += fmt.Sprintf(" (%s cached, %d%%)", numfmt.TokN(c), int(float64(c)*100/p))
	}
	if w := int(n("cache_write")); w > 0 {
		tokens = append(tokens, numfmt.TokN(w)+" written to the cache")
	}
	tokens = append(tokens, numfmt.TokN(int(n("completion")))+" out")
	if costed := int(n("costed")); costed > 0 {
		c := numfmt.TurnMoney(n("cost"), str(ts["cur"]))
		if ts["mixed"] == true {
			c += " and more in another currency"
		}
		if costed < rounds {
			c += fmt.Sprintf(" (%d of %d rounds priced)", costed, rounds)
		}
		tokens = append(tokens, c)
	}
	if sa := int(n("sub_agents")); sa > 0 {
		tokens = append(tokens, "incl. "+textfmt.Plural(sa, "sub-agent"))
	}
	return StDim.Render(head+timeRow) + "\n" + StDim.Render("     "+strings.Join(tokens, " · "))
}

// noteTurnStats puts a turn's cost on the message that started it: the last
// user message. A background turn adds its line under the first.
func (m *tuiModel) noteTurnStats(ev evMsg) {
	ts, ok := turnStatsOf(ev)
	if !ok {
		return
	}
	for i := len(m.convo) - 1; i >= 0; i-- {
		e := &m.convo[i]
		if e.userText == "" || e.provisional {
			continue
		}
		line := turnStatsLine(ts, e.stats != "")
		if e.stats != "" {
			line = e.stats + "\n" + line
		}
		e.stats, e.wrapped = line, ""
		return
	}
}
