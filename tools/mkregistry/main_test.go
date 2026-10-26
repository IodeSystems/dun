package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/registry"
)

// The site's entries build into an index this dun reads, every extension
// manifest loading: a broken entry fails make test, not the site deploy.
func TestBuild_TheSiteEntries(t *testing.T) {
	b, err := build("../../site/registry/v1", "../..", time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	ix, err := registry.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Extensions) == 0 || len(ix.Presets) == 0 || len(ix.Tools) == 0 {
		t.Errorf("an empty kind: %d extensions, %d presets, %d tools", len(ix.Extensions), len(ix.Presets), len(ix.Tools))
	}
	if !strings.Contains(string(b), `">=0.1.3"`) {
		t.Error("ranges are escaped")
	}
	for _, tl := range ix.Tools {
		if len(tl.Files) != 4 {
			t.Errorf("%s: %d files", tl.ID, len(tl.Files))
		}
	}
}

func TestBuild_RefusesABadEntry(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "extensions", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "extensions", "x", "1.0.0.json"),
		[]byte(`{"id": "x", "version": "1.0.0", "dun": "*", "tools": "*", "manifest": {"id": "y", "server": {"command": "y"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := build(src, ".", time.Now()); err == nil || !strings.Contains(err.Error(), "not the entry's") {
		t.Errorf("an id that differs built: %v", err)
	}
}
