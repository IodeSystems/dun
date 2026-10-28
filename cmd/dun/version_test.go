package main

import (
	"strings"
	"testing"
	"time"
)

// /version says which dun this is and whether it updates itself, and from
// where; a source build says it does not look releases up (USER 2026-10-28).
func TestVersion_SourceBuildSaysItsPolicy(t *testing.T) {
	oldV, oldSrc := version, srcDir
	t.Cleanup(func() { version, srcDir = oldV, oldSrc })
	version = "v0.1.11-3-gabcdef1-dirty"

	srcDir = ""
	lines := versionLines(time.Now())
	if !strings.HasPrefix(lines[0], "dun dev abcdef1* · source build") || !strings.Contains(lines[1], "updates: none") {
		t.Errorf("plain build:\n%s", strings.Join(lines, "\n"))
	}
	srcDir = "/src/dun"
	t.Setenv("DUN_NO_AUTOBUILD", "")
	if l := versionLines(time.Now())[1]; !strings.Contains(l, "rebuilds itself from /src/dun") || !strings.Contains(l, "releases are not looked up") {
		t.Errorf("checkout build: %q", l)
	}
	t.Setenv("DUN_NO_AUTOBUILD", "1")
	if l := versionLines(time.Now())[1]; !strings.Contains(l, "DUN_NO_AUTOBUILD=1") {
		t.Errorf("rebuild off: %q", l)
	}

	v := newVtui(100, 30)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	if cmd := versionSlash(&v.m, nil); cmd != nil {
		t.Error("a source build must not look releases up")
	}
	if !strings.Contains(v.convo(), "updates: ") {
		t.Errorf("/version printed nothing:\n%s", v.convo())
	}
}
