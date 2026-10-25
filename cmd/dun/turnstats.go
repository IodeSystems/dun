package main

import (
	"fmt"
	"strings"
	"time"
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
	head += " · " + plural(rounds, "round") + " · " + secs(wall)
	timeRow := ": model " + secs(model)
	if timed := int(n("timed")); timed > 0 && timed == rounds {
		pn, pms, gn, gms := n("prompt_n"), n("prompt_ms"), n("gen_n"), n("gen_ms")
		parts := []string{"prefill " + secs(pms) + ", " + tokN(int(pn))}
		if pms > 0 && pn > 0 {
			parts[0] += " at " + rate(pn/(pms/1000))
		}
		parts = append(parts, "generate "+secs(gms)+", "+tokN(int(gn)))
		timeRow += " (" + strings.Join(parts, " · ") + ")"
	}
	if rest := wall - model; wall > 0 && rest >= 500 {
		timeRow += " · tools and waits " + secs(rest)
	}
	tokens := []string{tokN(int(n("prompt"))) + " in"}
	if c, p := int(n("cached")), n("prompt"); c > 0 && p > 0 {
		tokens[0] += fmt.Sprintf(" (%s cached, %d%%)", tokN(c), int(float64(c)*100/p))
	}
	if w := int(n("cache_write")); w > 0 {
		tokens = append(tokens, tokN(w)+" written to the cache")
	}
	tokens = append(tokens, tokN(int(n("completion")))+" out")
	if costed := int(n("costed")); costed > 0 {
		c := turnMoney(n("cost"), str(ts["cur"]))
		if ts["mixed"] == true {
			c += " and more in another currency"
		}
		if costed < rounds {
			c += fmt.Sprintf(" (%d of %d rounds priced)", costed, rounds)
		}
		tokens = append(tokens, c)
	}
	if sa := int(n("sub_agents")); sa > 0 {
		tokens = append(tokens, "incl. "+plural(sa, "sub-agent"))
	}
	return stDim.Render(head+timeRow) + "\n" + stDim.Render("     "+strings.Join(tokens, " · "))
}

// secs is milliseconds as "41.2s", or "850ms" under a second.
func secs(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", int(ms))
	}
	return fmt.Sprintf("%.1fs", (time.Duration(ms) * time.Millisecond).Seconds())
}

// rate is a speed in tokens a second, the short way: "6.7k/s".
func rate(tps float64) string { return tokK(int(tps)) + "/s" }

// turnMoney is a turn's cost: a turn is often under a cent, which two
// decimals would show as nothing.
func turnMoney(v float64, cur string) string {
	if v > 0 && v < 0.01 {
		s := fmt.Sprintf("%.4f", v)
		switch cur {
		case "USD":
			return "$" + s
		case "":
			return s
		}
		return s + " " + cur
	}
	return moneyOf(v, cur)
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
