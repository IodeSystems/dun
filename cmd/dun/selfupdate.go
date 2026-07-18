package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Dev self-update — a source-stamped `dun` rebuilds itself in place when the
// tree changed, then re-execs the fresh binary. This kills the "my installed
// dun is out of date" problem: edit source, run `dun` again, and you transparently
// get the new build (dun re-execs itself for -p/-tui/-serve, so the whole tree
// is consistent). Guards:
//
//   - srcDir is stamped ONLY by `make build|install` (-ldflags -X main.srcDir).
//     A released `go install …@version` leaves it empty → self-update is a no-op.
//   - DUN_CHILD is set on every spawned subprocess → children never rebuild.
//   - DUN_AUTOBUILD_DONE guards the one re-exec after a rebuild (no loop).
//   - DUN_NO_AUTOBUILD=1 disables it entirely.
//   - A build failure (e.g. a dirty tree that doesn't compile) is non-fatal:
//     warn and run the current binary.

// srcDir is the module directory, stamped at build time (see Makefile). Empty
// for released/plain builds → self-update disabled.
var srcDir = ""

func selfUpdate() {
	if srcDir == "" ||
		os.Getenv("DUN_CHILD") != "" ||
		os.Getenv("DUN_AUTOBUILD_DONE") != "" ||
		os.Getenv("DUN_NO_AUTOBUILD") == "1" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	st, err := os.Stat(exe)
	if err != nil || !sourceNewerThan(srcDir, st.ModTime()) {
		return // up to date (or can't tell) — proceed normally
	}

	fmt.Fprintln(os.Stderr, "dun: source changed — rebuilding…")
	if _, err := rebuildDun(srcDir, exe); err != nil {
		fmt.Fprintf(os.Stderr, "dun: rebuild failed (%v) — running the current binary\n", err)
		return
	}
	// Re-exec the freshly built binary with the same args; the guard stops a loop.
	env := append(os.Environ(), "DUN_AUTOBUILD_DONE=1")
	if err := syscall.Exec(exe, os.Args, env); err != nil {
		fmt.Fprintf(os.Stderr, "dun: re-exec failed (%v) — running the current binary\n", err)
	}
}

// rebuildDun rebuilds the dun binary at `exe` from srcDir (version + source
// stamped) and returns the new version. Shared by selfUpdate and the launcher's
// central builder (launcher.go).
func rebuildDun(srcDir, exe string) (string, error) {
	ver := gitDescribe(srcDir)
	build := exec.Command("go", "build",
		"-o", exe,
		"-ldflags", "-X main.version="+ver+" -X main.srcDir="+srcDir,
		"./cmd/dun")
	build.Dir = srcDir
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		return "", err
	}
	return ver, nil
}

// sourceNewerThan reports whether any source file under dir is newer than t.
// Only files that affect the build are considered; .git and the like are pruned.
func sourceNewerThan(dir string, t time.Time) bool {
	newer := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || newer {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "tools":
				return filepath.SkipDir // tools/ = separate nested modules, not dun's build
			}
			return nil
		}
		if !buildInput(d.Name()) {
			return nil
		}
		if info, e := d.Info(); e == nil && info.ModTime().After(t) {
			newer = true
		}
		return nil
	})
	return newer
}

// buildInput reports whether a filename affects the compiled binary (Go source,
// go.mod/sum, and the embedded web assets).
func buildInput(name string) bool {
	switch name {
	case "go.mod", "go.sum":
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".html", ".css", ".js":
		return true
	}
	return false
}

// gitDescribe returns a version stamp for the rebuild; "dev" if git is absent.
func gitDescribe(dir string) string {
	cmd := exec.Command("git", "describe", "--tags", "--always", "--dirty")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "dev"
	}
	return strings.TrimSpace(string(out))
}
