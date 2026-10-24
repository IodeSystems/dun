package dundcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A service's dund upgrades the way a client-started one does — the same
// pending status, quiet point, deadline and "/upgrade now" — but it cannot
// re-exec into the binary a client hands it: the manager starts the unit's
// own binary on its next restart, the old build again. So at the quiet point
// it copies the new binary over the one the service starts (its own file),
// and exits with handoverExit; the manager starts the service again, on the
// new build, through the unit's own checks.
//
// Copy, not point the unit at the client's binary: that binary is wherever
// the client was built (~/.local/bin, a checkout's ./dun, a temp dir) and may
// be gone tomorrow, and a unit that names a missing file crash-loops;
// rewriting the unit is also a reload of the manager from inside its own
// service, which a launchd agent cannot do to itself. The copy is a temp file
// beside the service's binary, fsynced, made executable, asked its build and
// run through `serve --check`, and only then renamed over it: the running
// file is never written, and a failure at any step leaves it as it was.

// handoverExit is the code a service's dund exits with to be restarted on a
// new build: 75, EX_TEMPFAIL ("try again"). It is not zero, so the units
// written before it existed (Restart=on-failure) and launchd's
// KeepAlive.SuccessfulExit=false restart it; units written since also name it
// in RestartForceExitStatus and SuccessExitStatus.
const handoverExit = 75

// errHandover is what serve returns after a handover: Main exits handoverExit.
var errHandover = errors.New("exiting for the service manager to start the new build")

// canHandOver says why a service's dund cannot upgrade itself, nil when it
// can: its binary must be a regular file of this user in a directory this
// user can write (a temp file there is created and removed to prove it).
func canHandOver(self string) error {
	st, err := os.Stat(self)
	if err != nil {
		return fmt.Errorf("its binary %s is gone (%v)", self, err)
	}
	if !st.Mode().IsRegular() || !ownedByMe(st) {
		return fmt.Errorf("its binary %s is not a regular file of this user", self)
	}
	f, err := os.CreateTemp(filepath.Dir(self), "."+filepath.Base(self)+".probe-*")
	if err != nil {
		return fmt.Errorf("this user cannot write in %s, where its binary is", filepath.Dir(self))
	}
	f.Close()
	os.Remove(f.Name())
	return nil
}

// handoverCandidate checks exe as an upgrade for the service whose binary is
// self, and returns its build: the same checks as a client-started dund
// (checkUpgradeExe), the same kind of binary as self (the unit runs
// `<exe> dund serve` for dun, `<exe> serve` for the standalone dund), and it
// says which build it is — version, when one is given.
func handoverCandidate(self, exe, version string) (string, error) {
	if err := checkUpgradeExe(exe); err != nil {
		return "", err
	}
	if (servePrefix(self) == nil) != (servePrefix(exe) == nil) {
		return "", fmt.Errorf("upgrade: the service runs %s, and %s is not the same kind of binary (dun or the standalone dund)", self, exe)
	}
	got := probeBuild(exe)
	if got == "" {
		return "", fmt.Errorf("upgrade: %s does not say which build it is (--version)", exe)
	}
	if version != "" && got != version {
		return "", fmt.Errorf("upgrade: %s is build %s, not %s", exe, got, version)
	}
	return got, nil
}

// probeBuild is ProbeVersion, asked again after a moment when it does not
// answer: a file just written can be refused for a moment ("text file busy")
// while another goroutine's fork still holds its descriptor.
func probeBuild(exe string) string {
	for i := 0; ; i++ {
		if v := ProbeVersion(exe); v != "" || i == 2 {
			return v
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// installHandover puts exe (build version) in place as self, the binary the
// service starts, for serveArgs (this dund's os.Args[1:]). Nothing is
// replaced unless the copy is the expected build and passes `serve --check`
// with the service's own flags. exe already self (its file was replaced, or
// the unit runs the client's binary): checked, nothing copied.
func installHandover(self, exe, version string, serveArgs []string) error {
	ver, err := handoverCandidate(self, exe, version)
	if err != nil {
		return err
	}
	selfSt, err := os.Stat(self)
	if err != nil {
		return fmt.Errorf("upgrade: the service's binary: %w", err)
	}
	if exeSt, err := os.Stat(exe); err == nil && os.SameFile(selfSt, exeSt) {
		return serveCheck(self, serveArgs)
	}
	dir := filepath.Dir(self)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(self)+".upgrade-*")
	if err != nil {
		return fmt.Errorf("upgrade: cannot write beside %s: %w", self, err)
	}
	placed := false
	defer func() {
		if !placed {
			os.Remove(tmp.Name())
		}
	}()
	err = copyInto(tmp, exe)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), selfSt.Mode().Perm()|0o700)
	}
	if err != nil {
		return fmt.Errorf("upgrade: copy %s beside %s: %w", exe, self, err)
	}
	if got := probeBuild(tmp.Name()); got != ver {
		return fmt.Errorf("upgrade: the copy of %s says build %q, not %s (was it replaced meanwhile?)", exe, got, ver)
	}
	if err := serveCheck(tmp.Name(), serveArgs); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), self); err != nil {
		return fmt.Errorf("upgrade: put the new build at %s: %w", self, err)
	}
	placed = true
	if d, err := os.Open(dir); err == nil { // the rename survives a crash
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// copyInto copies the file src into f and syncs it.
func copyInto(f *os.File, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if _, err := io.Copy(f, in); err != nil {
		return err
	}
	return f.Sync()
}

// serveCheck runs bin's `serve --check` with this dund's own flags (args:
// its os.Args[1:]), as the one beside a live dund: --handover-from leaves
// out what only the live one holds (the pidfile, the HTTP port) and what
// would disturb it (New's changes to the registry). The unit's ExecStartPre
// runs the whole check again on the restart.
func serveCheck(bin string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := append(slices.Clone(args), "--check", "--handover-from", strconv.Itoa(os.Getpid()))
	cmd := exec.CommandContext(ctx, bin, a...)
	cmd.Env = append(os.Environ(), "DUN_CHILD=1", "DUN_NO_AUTOBUILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		last := strings.TrimSpace(string(out))
		if i := strings.LastIndexByte(last, '\n'); i >= 0 {
			last = last[i+1:]
		}
		return fmt.Errorf("upgrade: the new build fails `serve --check` with the service's flags: %v: %s", err, last)
	}
	return nil
}
