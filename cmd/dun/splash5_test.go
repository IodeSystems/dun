package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// splash5 is a fresh, logged-in session at w×h with its live splash.
func splash5(t *testing.T, w, h int) *vtui {
	t.Helper()
	v := newVtui(w, h)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}, "servers": []any{
		map[string]any{"id": "shell", "running": true, "tools": float64(1)},
		map[string]any{"id": "raglit", "slash": "raglit"},
		map[string]any{"id": "poly-lsp", "slash": "poly-lsp", "starting": true, "startingSeconds": float64(2)},
	}})
	v.event(map[string]any{"type": "settings", "model": "chat-standard", "url": "http://llm.example.com/v1", "provider": "work"})
	if len(v.m.convo) == 0 || !v.m.convo[0].liveSplash {
		t.Fatalf("no live splash:\n%s", v.convo())
	}
	return v
}

// costEvent is a context_cost event: system 2.1k of which context files
// 1.5k, built-in tools 1.6k, skills 4.0k, an MCP server 5.2k, shared 0.5k.
func costEvent(exact bool, window int) map[string]any {
	return map[string]any{"type": "context_cost", "system_tokens": float64(2100 + 1600 + 4000 + 5200 + 500),
		"system_exact": exact, "system_prompt": float64(2100), "system_shared": float64(500),
		"system_rules": float64(1500), "system_rules_mode": "full",
		"system_parts": []any{
			map[string]any{"name": "built-in tools", "tokens": float64(1600)},
			map[string]any{"name": "skills", "tokens": float64(4000)},
			map[string]any{"name": "mcp: notes", "tokens": float64(5200)},
		},
		"system_files": []any{
			map[string]any{"name": "AGENTS.md", "tokens": float64(900)},
			map[string]any{"name": "docs/rules.md", "tokens": float64(600)},
		},
		"window": float64(window)}
}

func spendEvent(today, week float64, remaining, last24h float64) map[string]any {
	ev := map[string]any{"type": "spend", "calls": float64(3), "costed_calls": float64(3), "session_cost": today, "currency": "USD",
		"ledger": map[string]any{"today": map[string]any{"USD": today}, "week": map[string]any{"USD": week},
			"month": map[string]any{"USD": week}, "year": map[string]any{"USD": week},
			"by_model": map[string]any{"chat-standard": map[string]any{"USD": week}},
			"last_24h": map[string]any{"USD": last24h}, "entries": true}}
	if remaining >= 0 {
		ev["key"] = map[string]any{"spend": 1.0, "remaining": remaining, "currency": "USD", "source": "litellm"}
	}
	return ev
}

func splashText(v *vtui) string { return stripANSI(v.m.convo[0].collapsed) }

// The splash is one height at a width, in every state — counting,
// estimated, exact, spend arriving, a server coming up — with the foreman
// and without, and no row is wider than the pane: a change rewrites rows in
// place.
func TestSplash5_FixedHeightAtEveryWidth(t *testing.T) {
	for _, art := range []bool{false, true} {
		mascotColour = art
		t.Cleanup(func() { mascotColour = false })
		for _, w := range []int{49, 100, 120, 180} {
			fixedHeightAt(t, w)
		}
	}
}

func fixedHeightAt(t *testing.T, w int) {
	t.Helper()
	{
		v := splash5(t, w, 30)
		want := strings.Count(splashText(v), "\n")
		steps := []map[string]any{
			costEvent(false, 32000),
			costEvent(true, 32000),
			spendEvent(0.42, 3.10, 12.30, 1.0),
			{"type": "server", "servers": []any{
				map[string]any{"id": "shell", "running": true, "tools": float64(1)},
				map[string]any{"id": "raglit", "slash": "raglit", "running": true},
				map[string]any{"id": "poly-lsp", "slash": "poly-lsp", "running": true, "tools": float64(4)},
			}},
			{"type": "catalog", "skills": []any{map[string]any{"name": "deploy", "description": "ship it", "source": "user"}}},
		}
		check := func(when string) {
			s := splashText(v)
			if n := strings.Count(s, "\n"); n != want || n < 6 {
				t.Errorf("%d cols (art %v), %s: %d rows, want %d (at least the small foreman's 6):\n%s", w, mascotColour, when, n, want, s)
			}
			for _, r := range strings.Split(s, "\n") {
				if ansi.StringWidth(r) > v.m.splashW() {
					t.Errorf("%d cols, %s: row wider than %d: %q", w, when, v.m.splashW(), r)
				}
			}
		}
		check("at start")
		for i, ev := range steps {
			v.event(ev)
			check(ev["type"].(string) + string(rune('0'+i)))
		}
	}
}

// The context row: the pre-typing load of the window, "~" while estimated,
// its parts on the second row; orange from 60% of the window.
func TestSplash5_ContextRow(t *testing.T) {
	v := splash5(t, 100, 30)
	if s := splashText(v); !strings.Contains(s, "context        counting…\n") {
		t.Fatalf("before the count:\n%s", s)
	}
	v.event(costEvent(false, 32000)) // 13.4k of 32k = 42%
	s := splashText(v)
	for _, want := range []string{
		"context        ~13.4k tokens before you type (of 32k, 42%)\n",
		"               system 600 · tools 0 (2.1k) · mcp 1 server (5.2k)\n", "               skills 0 (4.0k) · context files 2 (1.5k)\n",
		"context files  AGENTS.md, docs/rules.md\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(v.m.convo[0].collapsed, "38;5;214m~13.4k") {
		t.Error("42% is orange")
	}
	v.event(costEvent(true, 20000)) // 67%: exact, heavy
	if s := splashText(v); !strings.Contains(s, "context        13.4k tokens before you type (of 20k, 67%)\n") {
		t.Errorf("exact:\n%s", s)
	}
	if !strings.Contains(v.m.convo[0].collapsed, stNote.Render("13.4k tokens before you type (of 20k, 67%)")) {
		t.Errorf("67%% is not orange: %q", v.m.convo[0].collapsed)
	}
	v.event(costEvent(true, 0))
	if s := splashText(v); !strings.Contains(s, "context        13.4k tokens before you type\n") {
		t.Errorf("no window:\n%s", s)
	}
	// At 49 the words that carry it stay: the count, the window, the share.
	p := splash5(t, 49, 21)
	p.event(costEvent(false, 32000))
	s = splashText(p)
	if !strings.Contains(s, "context        ~13.4k tokens (of 32k, 42%)\n") || !strings.Contains(s, "\n                 system 600 · tools 0 (2.1k)…\n") {
		t.Errorf("at 49:\n%s", s)
	}
}

// The spend row picks the smallest period with a significant spend, says
// what the key has left, orange when that is a day or less at the last 24
// hours' rate, and never says $0 for a provider that reports nothing.
func TestSplash5_SpendRow(t *testing.T) {
	v := splash5(t, 100, 30)
	if s := splashText(v); !strings.Contains(s, "spend          none yet\n") {
		t.Fatalf("before any call:\n%s", s)
	}
	for _, tc := range []struct {
		today, week float64
		want        string
	}{
		{0.42, 3.10, "$0.42 today"},
		{0.004, 3.10, "$3.10 this week"},
		{0.0, 0.009, "< $0.01 this year"},
	} {
		v.event(spendEvent(tc.today, tc.week, -1, 0))
		if s := splashText(v); !strings.Contains(s, "spend          "+tc.want+"\n") {
			t.Errorf("today %v week %v: want %q:\n%s", tc.today, tc.week, tc.want, s)
		}
	}
	v.event(spendEvent(0.42, 3.10, 12.30, 1.0))
	if s := splashText(v); !strings.Contains(s, "spend          $0.42 today · $12.30 left on work\n") {
		t.Errorf("budget:\n%s", s)
	}
	if strings.Contains(v.m.convo[0].collapsed, "38;5;214m$12.30") {
		t.Error("12 days left is orange")
	}
	v.event(spendEvent(0.42, 3.10, 0.80, 1.0)) // less than a day at $1/24h
	if !strings.Contains(v.m.convo[0].collapsed, stNote.Render("$0.80 left on work")) {
		t.Errorf("a day or less is not orange: %q", v.m.convo[0].collapsed)
	}
	// Credits: one is the floor.
	v.event(map[string]any{"type": "spend", "calls": float64(1), "costed_calls": float64(1),
		"ledger": map[string]any{"today": map[string]any{"credits": 0.5}, "week": map[string]any{"credits": 2.0}, "entries": true}})
	if s := splashText(v); !strings.Contains(s, "spend          2 credits this week\n") {
		t.Errorf("credits:\n%s", s)
	}
	// A provider that reports no cost.
	v.event(map[string]any{"type": "spend", "calls": float64(4), "costed_calls": float64(0), "ledger": map[string]any{}})
	if s := splashText(v); !strings.Contains(s, "spend          not reported by work\n") {
		t.Errorf("not reported:\n%s", s)
	}
}

// The extensions row names each installed extension on or off and where to
// read about it; poly-lsp is a built-in server, on the servers row with its
// live state.
func TestSplash5_ExtensionsAndServers(t *testing.T) {
	v := splash5(t, 100, 30)
	v.m.srvSince["poly-lsp"] = time.Now().Add(-12 * time.Second)
	v.m.refresh()
	s := splashText(v)
	for _, want := range []string{"servers        poly-lsp starting 12s · shell running · 1 tool\n",
		"extensions     raglit off · /help raglit\n", "skills         none\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	v.event(map[string]any{"type": "catalog", "skills": []any{
		map[string]any{"name": "deploy", "description": "ship it", "source": "user"},
		map[string]any{"name": "review", "description": "review a diff", "source": "project"},
	}})
	if s := splashText(v); !strings.Contains(s, "skills         2 · /skill\n") {
		t.Errorf("skills:\n%s", s)
	}
}

// /status: the splash's rows and the detail — each server's, skill's and
// context file's share, and the spend by period, by model and this session.
func TestStatus5_Detail(t *testing.T) {
	v := splash5(t, 100, 40)
	v.event(map[string]any{"type": "server", "servers": []any{
		map[string]any{"id": "notes", "running": true, "tools": float64(1)},
		map[string]any{"id": "shell", "running": true, "tools": float64(1)},
	}})
	v.event(map[string]any{"type": "catalog", "skills": []any{map[string]any{"name": "deploy", "description": "ship it to production", "source": "user"}}})
	v.event(costEvent(true, 32000))
	v.event(spendEvent(0.42, 3.10, 12.30, 1.0))
	v.m.statusSlash()
	s := stripANSI(v.m.convo[len(v.m.convo)-1].collapsed)
	for _, want := range []string{
		"  build          ", "  context        13.4k tokens before you type (of 32k, 42%)\n",
		"  servers\n", "    notes  running · 1 tool · 5.2k\n",
		"  skills\n", "    deploy  ~8\n",
		"  context files\n", "    AGENTS.md      ~900\n", "    docs/rules.md  ~600\n",
		"  spend\n", "    today          $0.42\n", "    this week      $3.10\n", "    this year      $3.10\n",
		"    chat-standard  $3.10 this month\n", "    this session   $0.42 · 3 priced calls\n", "    work key       $12.30 left\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("/status lacks %q:\n%s", want, s)
		}
	}
}
