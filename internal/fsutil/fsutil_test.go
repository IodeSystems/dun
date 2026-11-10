package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// A path through a symlink, existing or not, is spelled with the link
// resolved.
func TestCanonResolvesSymlinks(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(real, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(real, "link")
	if err := os.Symlink(filepath.Join(real, "repo"), link); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		link:                                filepath.Join(real, "repo"),
		link + "/":                          filepath.Join(real, "repo"),
		filepath.Join(link, "gone", "x.go"): filepath.Join(real, "repo", "gone", "x.go"),
		filepath.Join(real, "repo", "..", "repo"): filepath.Join(real, "repo"),
	} {
		if got := Canon(in); got != want {
			t.Errorf("Canon(%q) = %q, want %q", in, got, want)
		}
	}
}
