package main

import (
	"strings"
	"testing"
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
