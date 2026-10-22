package dundcli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWithFlag(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{[]string{"dun", "dund", "serve", "--dun", "/old"}, []string{"dun", "dund", "serve", "--dun", "/new"}},
		{[]string{"dun", "serve", "--dun=/old"}, []string{"dun", "serve", "--dun=/new"}},
		{[]string{"dun", "serve"}, []string{"dun", "serve", "--dun", "/new"}},
	} {
		if got := withFlag(c.in, "--dun", "/new"); !slices.Equal(got, c.want) {
			t.Errorf("withFlag(%v) = %v", c.in, got)
		}
	}
}

// An upgrade names an absolute, executable, regular file of this user.
func TestRequestUpgradeChecksTheBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "dun")
	os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
	plain := filepath.Join(dir, "notes")
	os.WriteFile(plain, []byte("x"), 0o644)
	for _, bad := range []string{"dun", plain, dir, filepath.Join(dir, "missing")} {
		if err := checkUpgradeExe(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
