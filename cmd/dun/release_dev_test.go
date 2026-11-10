//go:build !release

package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/release"
)

// A build without -tags release has no update key or pinned tools, and no
// non-release source file names a download location (owner: only release
// builds have the download paths) — except the release manifest's URL in
// release/key.go, which a source build reads only on an explicit /upgrade
// --channel=release (Carl 2026-11-10, docs/decisions.md).
func TestDevBuildHasNoDownloadPaths(t *testing.T) {
	if release.Built || release.PublicKey != "" || release.Deps != nil {
		t.Fatal("a dev build carries release data")
	}
	if buildinfo.IsRelease() {
		t.Fatal("a dev build would check for updates")
	}
	if _, _, ok := pinnedTool("mcpshell"); ok {
		t.Fatal("a dev build has a pinned tool")
	}
	root := "../.."
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".claude" || d.Name() == "node_modules" || d.Name() == "tmp") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.HasPrefix(b, []byte("//go:build release\n")) || filepath.ToSlash(p) == "../../release/key.go" {
			return nil
		}
		for _, s := range []string{"releases/download", "releases/latest"} {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s names %q outside a release-tagged file", p, s)
			}
		}
		return nil
	})
}
