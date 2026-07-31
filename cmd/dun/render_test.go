package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsDiff(t *testing.T) {
	yes := []string{
		"@@ -1,2 +1,3 @@\n-a\n+b",
		"diff --git a/x.go b/x.go\nindex 1..2",
		"--- a/x.go\n+++ b/x.go",
	}
	for _, s := range yes {
		if !isDiff(s) {
			t.Errorf("should be diff: %q", s)
		}
	}
	if isDiff("the function returns a string") {
		t.Error("prose should not be a diff")
	}
}

func TestColorizeDiff_PreservesContent(t *testing.T) {
	in := "@@ -1 +1 @@\n-old line\n+new line\n unchanged"
	out := colorizeDiff(in)
	for _, want := range []string{"old line", "new line", "unchanged"} {
		if !strings.Contains(out, want) {
			t.Errorf("colorizeDiff dropped %q\n%s", want, out)
		}
	}
	if strings.Count(out, "\n") != strings.Count(in, "\n") {
		t.Errorf("line count changed: %d vs %d", strings.Count(out, "\n"), strings.Count(in, "\n"))
	}
}

func TestRenderMarkdown_FallsBackWhenNil(t *testing.T) {
	if got := renderMarkdown(nil, "# Heading"); got != "# Heading" {
		t.Fatalf("nil renderer should pass through, got %q", got)
	}
	// A real renderer keeps the text content.
	if got := renderMarkdown(newMarkdown(80), "**bold** words"); !strings.Contains(got, "bold") {
		t.Fatalf("rendered markdown dropped content: %q", got)
	}
}

// The markdown style must never cost a terminal round trip on the input loop.
// glamour's auto-style asks the terminal for its background and waits up to
// five seconds; unanswered — tmux without passthrough, a bare pty — that was a
// 5s stall, once per resize, inside Update.
func TestMarkdownStyle_NoQueryInAMultiplexer(t *testing.T) {
	t.Setenv("DUN_MD_STYLE", "")
	t.Setenv("COLORFGBG", "")
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	mdStyleOnce = sync.Once{}
	mdStyle = ""

	done := make(chan string, 1)
	go func() { initMarkdownStyle(); done <- mdStyle }()
	select {
	case got := <-done:
		if got != "dark" {
			t.Errorf("inside a multiplexer the style should default without querying, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initMarkdownStyle queried the terminal inside a multiplexer")
	}
}

// An explicit setting skips everything.
func TestMarkdownStyle_Override(t *testing.T) {
	for _, want := range []string{"dark", "light", "notty"} {
		t.Setenv("DUN_MD_STYLE", want)
		mdStyleOnce = sync.Once{}
		mdStyle = ""
		initMarkdownStyle()
		if mdStyle != want {
			t.Errorf("DUN_MD_STYLE=%s gave %q", want, mdStyle)
		}
	}
}

// COLORFGBG answers for free when the terminal exports it.
func TestParseColorFGBG(t *testing.T) {
	cases := []struct {
		in string
		bg int
		ok bool
	}{
		{"15;0", 0, true},  // light text on dark
		{"0;15", 15, true}, // dark text on light
		{"15;default;0", 0, true},
		{"", 0, false},
		{"nonsense", 0, false},
	}
	for _, c := range cases {
		_, bg, ok := parseColorFGBG(c.in)
		if ok != c.ok || (ok && bg != c.bg) {
			t.Errorf("parseColorFGBG(%q) = bg %d, ok %v; want bg %d, ok %v", c.in, bg, ok, c.bg, c.ok)
		}
	}
}
