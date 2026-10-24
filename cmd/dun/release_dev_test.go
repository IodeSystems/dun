//go:build !release

package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/release"
)

// A build without -tags release has no download location, key or pinned
// tools, and no non-release source file names one (owner: only release
// builds have the download paths).
func TestDevBuildHasNoDownloadPaths(t *testing.T) {
	if release.Built || release.ManifestURL != "" || release.PublicKey != "" || release.Deps != nil {
		t.Fatal("a dev build carries release data")
	}
	if isRelease() || newUpdater() != nil {
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
		if bytes.HasPrefix(b, []byte("//go:build release\n")) {
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
