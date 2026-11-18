package fsutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A held lock is waited for until ctx ends, then given up; released, it is
// taken.
func TestLockFileContext(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := LockFile(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := LockFileContext(ctx, p); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("held: %v", err)
	}
	unlock()
	un2, err := LockFileContext(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	un2()
}

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
