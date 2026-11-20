package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/iodesystems/dun/internal/buildinfo"
)

func TestVersionsReport(t *testing.T) {
	plain := func(ls []string) string {
		for i := range ls {
			ls[i] = ansi.Strip(ls[i])
		}
		return strings.Join(ls, "\n")
	}
	same := plain(versionsReport("v0.9.0-3-gabc1234", "v0.9.0-3-gabc1234", "v0.9.0-3-gabc1234"))
	if strings.Contains(same, "⚠") || !strings.Contains(same, "TUI v0.9.0-3-gabc1234 · dund v0.9.0-3-gabc1234 · engine v0.9.0-3-gabc1234") {
		t.Errorf("equal versions:\n%s", same)
	}
	diff := plain(versionsReport("v0.9.0-3-gabc1234", "v0.8.1-9-g67f1169", "v0.8.1-9-g67f1169"))
	if !strings.Contains(diff, "⚠ this TUI is v0.9.0-3-gabc1234 but dund/engine is not: /upgrade now restarts them into this build") ||
		!strings.Contains(diff, "dund v0.8.1-9-g67f1169") {
		t.Errorf("differing versions:\n%s", diff)
	}
	only := plain(versionsReport("v0.9.0", "v0.9.0", "v0.8.0"))
	if !strings.Contains(only, "but engine is not") {
		t.Errorf("engine only:\n%s", only)
	}
}

func TestUpgradeSlash_ShowsVersions(t *testing.T) {
	old, oldAsk := runTools, askDundUpgrade
	defer func() { runTools, askDundUpgrade = old, oldAsk }()
	runTools = func(bool) ([]toolState, error) { return nil, nil }
	oldV := buildinfo.Version
	buildinfo.Version = "v0.9.0"
	defer func() { buildinfo.Version = oldV }()
	dundSeen.Store("v0.8.1-9-g67f1169")
	defer dundSeen.Store("")

	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	m.engineVersion = "v0.8.1"
	upgradeSlash(&m, nil)
	var all []string
	for _, c := range m.convo {
		all = append(all, ansi.Strip(c.view()))
	}
	got := strings.Join(all, "\n")
	for _, want := range []string{"TUI v0.9.0", "dund v0.8.1-9-g67f1169", "engine v0.8.1", "⚠ this TUI is v0.9.0 but dund/engine is not"} {
		if !strings.Contains(got, want) {
			t.Errorf("/upgrade lacks %q:\n%s", want, got)
		}
	}
}

func TestCompareBuilds(t *testing.T) {
	for _, c := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"v0.9.0", "v0.8.1", 1, true},
		{"v0.8.1-9-g67f1169", "v0.8.1", 1, true},
		{"v0.8.1-9-g67f1169", "v0.8.1-12-gaaaaaaa", -1, true},
		{"v0.8.1-9-g67f1169", "v0.8.1-9-g67f1169", 0, true},
		{"v0.8.1-9-g67f1169", "v0.8.1-9-gaaaaaaa", 0, false},
		{"v0.8.1-9-g67f1169", "v0.8.1-9-g67f1169-dirty", 0, false},
		{"dev", "v0.8.1", 0, false},
	} {
		if cmp, ok := compareBuilds(c.a, c.b); cmp != c.cmp || ok != c.ok {
			t.Errorf("compareBuilds(%q, %q) = %d, %v; want %d, %v", c.a, c.b, cmp, ok, c.cmp, c.ok)
		}
	}
}

// c in the jobs table against an engine whose /jobs has no clear: the hint,
// nothing sent (the engine would answer with its usage string).
func TestTUI_CAgainstOldEngineSaysUpgrade(t *testing.T) {
	v, rec := jobsVtui(t)
	v.event(map[string]any{"type": "commands", "items": []any{
		map[string]any{"name": "jobs", "usage": "[kill <n>|kill all]", "description": "background jobs"},
	}}) // an old engine: its /jobs lists no clear
	v.m.engineVersion = "v0.8.1"
	v.m.setFocus(focusActivity)
	v.m = key(v.m, kC)
	if rec.Len() != 0 {
		t.Errorf("sent to an engine that lacks it: %q", rec.String())
	}
	got := v.convo()
	if !strings.Contains(got, "engine is older (v0.8.1) and has no /jobs clear: /upgrade now") || strings.Contains(got, "usage:") {
		t.Errorf("c on an old engine:\n%s", got)
	}

	// An engine that lists clear gets it.
	v.event(map[string]any{"type": "commands", "items": []any{
		map[string]any{"name": "jobs", "usage": "[kill <n>|kill all|clear]", "description": "background jobs",
			"choices": []any{map[string]any{"value": "kill"}, map[string]any{"value": "clear"}}},
	}})
	rec.Reset()
	v.m = key(v.m, kC)
	if !strings.Contains(rec.String(), `"action":"jobs clear"`) {
		t.Errorf("c on a current engine sent %q", rec.String())
	}
}
