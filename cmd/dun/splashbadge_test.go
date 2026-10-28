package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Under the foreman: which dun this is, then the motto, each centred in his
// width (USER 2026-10-28). A source build says "dev" and its commit.
func TestSplash_BadgeUnderTheMascot(t *testing.T) {
	old, oldColour := version, mascotColour
	t.Cleanup(func() { version, mascotColour = old, oldColour })
	version, mascotColour = "v0.1.9-12-gb8596f1-dirty", true

	rows := splashBadge(13)
	if len(rows) != 2 {
		t.Fatalf("want two rows, got %d", len(rows))
	}
	for _, r := range rows {
		if w := ansi.StringWidth(r); w != 13 {
			t.Errorf("row %q is %d cells, want the mascot's 13", stripANSI(r), w)
		}
	}
	if got := strings.TrimSpace(stripANSI(rows[0])); !strings.HasPrefix(got, "dev ") || !strings.Contains(got, "*") {
		t.Errorf("a source build should say dev and its commit: %q", got)
	}
	if got := strings.TrimSpace(stripANSI(rows[1])); got != "git'r dun!" {
		t.Errorf("motto %q", got)
	}

	v := newVtui(100, 30)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.event(map[string]any{"type": "settings", "model": "chat-standard", "url": "http://llm/v1", "provider": "work"})
	splash := stripANSI(v.m.splashView(v.m.splashW()))
	lines := strings.Split(splash, "\n")
	if len(lines) < 8 || !strings.Contains(lines[6], "dev ") || !strings.Contains(lines[7], "git'r dun!") {
		t.Fatalf("the badge should be the two rows under the mascot's six:\n%s", splash)
	}
}
