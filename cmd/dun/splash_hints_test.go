package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Each row set by a command says which, dim, after its value; a phone
// drops the hint before the value would wrap.
func TestSplashHints_RowsAtWideAndNarrow(t *testing.T) {
	v := splash5(t, 110, 30)
	v.event(costEvent(false, 32000))
	s := splashText(v)
	for _, want := range []string{"model       chat-standard on work · /model\n",
		"tools       1 (7.3k) · poly-lsp starting 2s · /mcp info\n", "extensions  0 of 1 on · /ext info\n",
		"rules       AGENTS.md, docs/rules.md · /rules\n", "(of 32k, 42%) · /context config\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("110: lacks %q:\n%s", want, s)
		}
	}
	if !strings.Contains(v.m.convo[0].collapsed, stDim.Render(" · /model")) {
		t.Error("the model hint is not dim")
	}
	p := splash5(t, 49, 21)
	p.event(costEvent(false, 32000))
	n := splashText(p)
	for _, line := range strings.Split(n, "\n") {
		if ansi.StringWidth(line) > 49 {
			t.Errorf("49: %q is wider than the pane", line)
		}
	}
	for _, want := range []string{"model       chat-standard on work · /model\n", "context     ~13.4k tokens (of 32k, 42%)\n"} {
		if !strings.Contains(n, want) {
			t.Errorf("49: lacks %q:\n%s", want, n)
		}
	}
	if strings.Contains(n, "/context config") {
		t.Errorf("49: a hint pushed a value:\n%s", n)
	}
}

// usageSpend is a spend event with the session's tokens.
func usageSpend(calls, prompt, cached, completion int, cacheReported bool) map[string]any {
	ev := map[string]any{"type": "spend", "calls": float64(calls), "costed_calls": float64(0),
		"prompt_tokens": float64(prompt), "cached_tokens": float64(cached), "completion_tokens": float64(completion)}
	if cacheReported {
		ev["cache_reported"] = true
	}
	return ev
}

// The spend row: requests, tokens processed and generated, the cache rate
// only when the provider reports cached tokens, the cost when it is known.
func TestSplashHints_SpendUsage(t *testing.T) {
	v := splash5(t, 110, 30)
	v.event(usageSpend(12, 41300, 33900, 2100, true))
	if s := splashText(v); !strings.Contains(s, "spend       12 requests · 41.3k in · 2.1k out · 82% cached · cost not reported by work\n") {
		t.Errorf("cached:\n%s", s)
	}
	v.event(usageSpend(1, 900, 0, 40, false))
	if s := splashText(v); !strings.Contains(s, "spend       1 request · 900 in · 40 out · cost not reported by work\n") {
		t.Errorf("no cache report:\n%s", s)
	}
	// Reported and zero is 0%, not silence.
	v.event(usageSpend(2, 1000, 0, 40, true))
	if s := splashText(v); !strings.Contains(s, "· 0% cached") {
		t.Errorf("0%% cached:\n%s", s)
	}
	ev := spendEvent(0.42, 3.10, 12.30, 1.0)
	ev["prompt_tokens"], ev["cached_tokens"], ev["completion_tokens"], ev["cache_reported"] = float64(2000), float64(1000), float64(100), true
	v.event(ev)
	if s := splashText(v); !strings.Contains(s, "spend       3 requests · 2.0k in · 100 out · 50% cached · $0.42 today · $12.30 left on work\n") {
		t.Errorf("priced:\n%s", s)
	}
	// A phone keeps the requests and the tokens, then drops words.
	p := splash5(t, 49, 21)
	p.event(usageSpend(12, 41300, 33900, 2100, true))
	seen := false
	for _, line := range strings.Split(splashText(p), "\n") {
		if ansi.StringWidth(line) > 49 {
			t.Errorf("49: %q is wider than the pane", line)
		}
		if strings.Contains(line, "spend") {
			seen = true
			if !strings.Contains(line, "12 req · 41.3k in · 82% cached") {
				t.Errorf("49: spend row %q", line)
			}
		}
	}
	if !seen {
		t.Error("49: no spend row")
	}
}
