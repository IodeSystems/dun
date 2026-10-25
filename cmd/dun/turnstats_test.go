package main

import (
	"strings"
	"testing"
)

// A turn's cost goes on the message that started it, and enter on the
// message opens it; a turn the engine ran on its own adds a line under it.
func TestTurnStats_OnTheMessage(t *testing.T) {
	v := newVtui(140, 30)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("fix the flaky test")
	v.event(map[string]any{"type": "usage", "turn_stats": map[string]any{
		"rounds": float64(6), "wall_ms": float64(41200), "model_ms": float64(29800),
		"prompt": float64(412000), "cached": float64(351000), "completion": float64(1400),
		"timed": float64(6), "prompt_n": float64(61000), "prompt_ms": float64(9100), "gen_n": float64(1400), "gen_ms": float64(20700),
		"cost": float64(0.0031), "cur": "USD", "costed": float64(6), "sub_agents": float64(1)}})
	v.event(map[string]any{"type": "done"})
	if strings.Contains(v.convo(), "6 rounds") {
		t.Fatalf("the stats show before the message is opened:\n%s", v.convo())
	}
	i := -1
	for j, e := range v.m.convo {
		if e.userText == "fix the flaky test" {
			i = j
		}
	}
	if i < 0 || v.m.convo[i].stats == "" {
		t.Fatalf("no stats on the message: %+v", v.m.convo)
	}
	v.m.setFocus(focusConvo)
	v.m.sel = i
	v.m = key(v.m, kEnter)
	if v.m.convo[i].state != viewExpanded {
		t.Fatalf("enter did not open the message: state %v", v.m.convo[i].state)
	}
	if got := v.view(); !strings.Contains(got, "tools and waits 11.4s") || !strings.Contains(got, "incl. 1 sub-agent") {
		t.Errorf("the screen lacks the stats:\n%s", got)
	}
	got := v.convo()
	for _, want := range []string{"turn · 6 rounds · 41.2s: model 29.8s (prefill 9.1s, 61.0k at 6.7k/s · generate 20.7s, 1.4k) · tools and waits 11.4s",
		"412.0k in (351.0k cached) · 1.4k out · $0.0031 · incl. 1 sub-agent"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	// The engine's own turn after it: a second line, priced in part.
	v.event(map[string]any{"type": "usage", "turn_stats": map[string]any{
		"rounds": float64(2), "wall_ms": float64(900), "model_ms": float64(800), "prompt": float64(2000), "completion": float64(40),
		"cost": float64(1.5), "cur": "USD", "costed": float64(1)}})
	v.m.refresh()
	if got := v.convo(); !strings.Contains(got, "then on its own · 2 rounds · 900ms: model 800ms") || !strings.Contains(got, "$1.50 (1 of 2 rounds priced)") {
		t.Errorf("background turn:\n%s", got)
	}
	// An engine that sends no stats, or a turn with no request: nothing.
	if _, ok := turnStatsOf(evMsg{"type": "usage"}); ok {
		t.Error("no turn_stats is no stats")
	}
}
