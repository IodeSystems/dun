package dun

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The two behaviors the persistent shell exists for: an `export` in one call
// is visible in the next, while the working directory resets to the project
// root every time.
func TestHostShell_EnvPersistsAndCwdResets(t *testing.T) {
	dir := t.TempDir()
	// A subdir to cd into, to prove the reset is real (cd actually changes dir).
	sub := dir + "/sub"
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = sub
	hs := &HostShell{Dir: dir}
	ctx := context.Background()

	// 1. Export a variable.
	r := hs.Run(ctx, "export DUN_TEST_VAR=hello123", nil)
	if r.Failed() {
		t.Fatalf("export call failed: %v", r)
	}
	// 2. Read it back in a fresh call — it must still be set.
	r = hs.Run(ctx, "echo $DUN_TEST_VAR", nil)
	if r.Failed() || strings.TrimSpace(r.Output) != "hello123" {
		t.Fatalf("export did not persist: got %q (failed=%v)", r.Output, r.Failed())
	}

	// 3. cd away inside a call; the NEXT call must start back in Dir.
	r = hs.Run(ctx, "cd /tmp && echo inside-tmp", nil)
	if r.Failed() || strings.TrimSpace(r.Output) != "inside-tmp" {
		t.Fatalf("cd call failed: %q (failed=%v)", r.Output, r.Failed())
	}
	r = hs.Run(ctx, "pwd", nil)
	if r.Failed() || strings.TrimSpace(r.Output) != dir {
		t.Fatalf("cwd did not reset to %q: got %q (failed=%v)", dir, r.Output, r.Failed())
	}
}

// A non-zero exit code must come through the sentinel.
func TestHostShell_ExitCode(t *testing.T) {
	hs := &HostShell{Dir: t.TempDir()}
	r := hs.Run(context.Background(), "exit 7", nil)
	if r.Code != 7 {
		t.Fatalf("want exit 7, got %d (%q)", r.Code, r.Output)
	}
}

// A command that prints a lot must not corrupt the sentinel or the output.
func TestHostShell_MultiLineOutput(t *testing.T) {
	hs := &HostShell{Dir: t.TempDir()}
	r := hs.Run(context.Background(), "for i in 1 2 3 4 5; do echo line-$i; done", nil)
	if r.Failed() {
		t.Fatalf("failed: %v", r)
	}
	for i := 1; i <= 5; i++ {
		if !strings.Contains(r.Output, "line-"+strconv.Itoa(i)) {
			t.Fatalf("missing line-%d in %q", i, r.Output)
		}
	}
	if strings.Contains(r.Output, shSentinelPrefix) {
		t.Fatalf("sentinel leaked into output: %q", r.Output)
	}
}
