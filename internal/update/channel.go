package update

import (
	"bytes"
	"os"
	"path/filepath"
)

// Where /upgrade --channel=release puts the release (Carl 2026-11-10,
// docs/decisions.md): over this binary's own file, as a release build's
// update does — except for the launcher's binary, which the launcher script
// rebuilds from the checkout on the next start; that one goes where
// install.sh puts it. Nothing on PATH is ever deleted: a dun that comes
// first and would win is named instead.

// LauncherBin is the binary `make install-launcher`'s script (tools/dun.sh)
// builds and runs: $DUN_BIN, else $XDG_CACHE_HOME/dun/dun, else
// ~/.cache/dun/dun.
func LauncherBin() string {
	if b := os.Getenv("DUN_BIN"); b != "" {
		return b
	}
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		home, _ := os.UserHomeDir()
		cache = filepath.Join(home, ".cache")
	}
	return filepath.Join(cache, "dun", "dun")
}

// InstallPath is where install.sh puts dun: $DUN_INSTALL_DIR/dun, else
// ~/.local/bin/dun.
func InstallPath() string {
	dir := os.Getenv("DUN_INSTALL_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "bin")
	}
	return filepath.Join(dir, "dun")
}

// SwitchTarget is where a channel switch installs the release for the
// binary at exe, and whether exe is the launcher's.
func SwitchTarget(exe string) (string, bool) {
	if sameFile(exe, LauncherBin()) {
		return InstallPath(), true
	}
	return exe, false
}

// FirstOnPath is the first executable file named name in the directories
// of pathList (a $PATH value), "" when there is none.
func FirstOnPath(name, pathList string) string {
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// IsLauncher: the file at path is the launcher script (tools/dun.sh).
func IsLauncher(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 256)
	n, _ := f.Read(head)
	return bytes.Contains(head[:n], []byte("# dun launcher"))
}

// Shadow is the dun that runs, instead of the one at target, when "dun" is
// typed with pathList as PATH: "" when target wins. A target whose
// directory is not on PATH at all is shadowed by whatever is first, or by
// nothing ("", with onPath false).
func Shadow(target, pathList string) (first string, onPath bool) {
	first = FirstOnPath(filepath.Base(target), pathList)
	if first != "" && sameFile(first, target) {
		return "", true
	}
	for _, dir := range filepath.SplitList(pathList) {
		if sameFile(dir, filepath.Dir(target)) {
			onPath = true
		}
	}
	return first, onPath
}

func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && os.SameFile(sa, sb)
}
