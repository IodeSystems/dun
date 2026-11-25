//go:build unix

package dundcli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A build that passes `serve --check` can still die at start: the check runs
// beside the live dund, out of its way (no pidfile, no HTTP port, no
// registry), and the failures it does not see are the ones that need the
// live dund gone (docs/decisions.md: handover rollback). Under a manager a
// crash-looping build is only caught by the manager's start limits, and by
// then the old binary is gone — installHandover's rename destroys it,
// nothing kept a copy.
//
// Rollback: installHandover keeps the service's previous build beside its
// binary as <exe>.prev. A guard at the top of serve (service-managed dunds
// only) counts the starts that die before the dund is serving, in
// <home>/dund/failed-starts; at the second such failure it restores .prev
// over the binary, clears the counter, and writes upgrade-refused.txt
// beside the binary. canHandOver consults the marker, so no upgrade is
// accepted while it is there; the marker and the counter are removed once
// a dund is actually serving, so the refusal ends when a good build is
// served.
//
// The count is per-home (DUN_HOME), not per binary: the binary may be the
// rolled-back build by the time the next start reads it, and the marker is
// what says the last build is not good. Two failures, not one: a single
// failed start after a handover can be the manager racing the rename.

// prevBuildPath is where installHandover keeps the service's previous build.
func prevBuildPath(self string) string { return self + ".prev" }

// upgradeRefusedPath is the marker that refuses upgrades until a build
// serves (rollbackGuard): beside the binary, so it survives the binary
// being replaced again.
func upgradeRefusedPath(self string) string { return self + ".upgrade-refused" }

// failedStartsPath is the counter of consecutive starts that died before
// the dund served, under <home>/dund.
func failedStartsPath(home string) string { return filepath.Join(home, "dund", "failed-starts") }

// rollbackFires is the N-th consecutive failed start that triggers the
// rollback: the first can be the manager racing the handover's rename.
const rollbackFires = 2

// rollbackGuard counts this start as failed (one file write) and, at the
// N-th consecutive failure, restores the previous build and refuses
// upgrades. It must run before anything that can fail on its own — after
// the pidfile is claimed, so a second dund on the same home cannot roll
// back a first one that is simply stopped.
func rollbackGuard(home, self string) {
	path := failedStartsPath(home)
	n := 0
	if b, err := os.ReadFile(path); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	n++
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(strconv.Itoa(n)+"\n"), 0o600); err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "dund: start %d in a row died before serving\n", n)
	if n < rollbackFires {
		return
	}
	prev := prevBuildPath(self)
	if st, err := os.Stat(prev); err == nil && st.Mode().IsRegular() {
		fmt.Fprintf(os.Stderr, "dund: restoring the previous build from %s\n", prev)
		_ = os.Rename(prev, self)
	} else {
		fmt.Fprintf(os.Stderr, "dund: no previous build at %s to restore\n", prev)
	}
	_ = os.Remove(path)
	refuse := upgradeRefusedPath(self)
	_ = os.WriteFile(refuse, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
	fmt.Fprintf(os.Stderr, "dund: upgrades are refused (%s) until a build serves\n", refuse)
}

// rollbackClear removes the rollback's state: the counter, once the dund is
// serving, and the marker, once a build serves. Called from serve after the
// listener is up.
func rollbackClear(home, self string) {
	_ = os.Remove(failedStartsPath(home))
	_ = os.Remove(upgradeRefusedPath(self))
}
