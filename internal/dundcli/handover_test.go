//go:build unix

package dundcli

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeDun writes a script that says it is build ver and answers
// `serve --check` with checkExit (and the arguments it was given, to
// argsLog when set).
func fakeDun(t *testing.T, path, ver string, checkExit int, argsLog string) {
	t.Helper()
	log := ""
	if argsLog != "" {
		log = `echo "$*" > ` + argsLog + "\n"
	}
	script := "#!/bin/sh\n" +
		`case " $* " in *" --version "*) echo "dun ` + ver + `"; exit 0;; esac` + "\n" +
		log +
		`echo "dund: check said no" >&2` + "\n" +
		"exit " + strconv.Itoa(checkExit) + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// leftovers are the temp files a handover left beside the service's binary.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

var serviceArgs = []string{"dund", "--home", "/h", "serve", "--http", "127.0.0.1:1"}

// The new build is copied over the service's binary once it says the build
// it was asked for and passes `serve --check` with the service's own flags
// (as a handover from this pid); nothing else is left beside it.
func TestInstallHandoverReplacesTheServiceBinary(t *testing.T) {
	svcDir, newDir := t.TempDir(), t.TempDir()
	self, exe := filepath.Join(svcDir, "dun"), filepath.Join(newDir, "dun")
	argsLog := filepath.Join(t.TempDir(), "args")
	fakeDun(t, self, "v1", 0, "")
	fakeDun(t, exe, "v2", 0, argsLog)
	if err := canHandOver(self); err != nil {
		t.Fatal(err)
	}
	if err := installHandover(self, exe, "v2", serviceArgs); err != nil {
		t.Fatal(err)
	}
	if read(t, self) != read(t, exe) || ProbeVersion(self) != "v2" {
		t.Fatalf("the service's binary is not the new build: %q", ProbeVersion(self))
	}
	want := strings.Join(serviceArgs, " ") + " --check --handover-from " + strconv.Itoa(os.Getpid()) + "\n"
	if got := read(t, argsLog); got != want {
		t.Errorf("check args %q, want %q", got, want)
	}
	if l := leftovers(t, svcDir); len(l) != 0 {
		t.Errorf("left beside it: %v", l)
	}
}

// Nothing is replaced when the new build fails its check, is another build
// than asked, or is another kind of binary; the error says which.
func TestInstallHandoverRefusals(t *testing.T) {
	for _, c := range []struct {
		name, exeBase, ver string
		checkExit          int
		want               string
	}{
		{"check fails", "dun", "v2", 1, "fails `serve --check`"},
		{"other build", "dun", "v3", 0, "is build v3, not v2"},
		{"other kind", "dund", "v2", 0, "not the same kind of binary"},
	} {
		svcDir := t.TempDir()
		self, exe := filepath.Join(svcDir, "dun"), filepath.Join(t.TempDir(), c.exeBase)
		fakeDun(t, self, "v1", 0, "")
		fakeDun(t, exe, c.ver, c.checkExit, "")
		before := read(t, self)
		err := installHandover(self, exe, "v2", serviceArgs)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
		if read(t, self) != before {
			t.Errorf("%s: the service's binary was replaced", c.name)
		}
		if l := leftovers(t, svcDir); len(l) != 0 {
			t.Errorf("%s: left beside it: %v", c.name, l)
		}
	}
}

// The service's binary is the new build already (its file was replaced):
// checked, not copied.
func TestInstallHandoverOfItsOwnFile(t *testing.T) {
	self := filepath.Join(t.TempDir(), "dun")
	fakeDun(t, self, "v2", 0, "")
	if err := installHandover(self, self, "v2", serviceArgs); err != nil {
		t.Fatal(err)
	}
	fakeDun(t, self, "v2", 1, "")
	if err := installHandover(self, self, "v2", serviceArgs); err == nil {
		t.Fatal("a build that fails its check was taken")
	}
}

// A service whose binary sits where this user cannot write cannot upgrade
// itself, and says so.
func TestCanHandOverNeedsAWritableDirectory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	self := filepath.Join(dir, "dun")
	fakeDun(t, self, "v1", 0, "")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if err := canHandOver(self); err == nil || !strings.Contains(err.Error(), "cannot write in "+dir) {
		t.Fatalf("canHandOver: %v", err)
	}
	if err := canHandOver(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing binary can hand over")
	}
}

// The check beside a live dund does not hold the live one's pidfile and
// HTTP port against the new build; the plain check still does.
func TestCheckBesideTheLiveDund(t *testing.T) {
	home := t.TempDir()
	l := &loader{home: home}
	live := os.Getppid() // a process that is alive and is not this one
	if err := os.WriteFile(l.pidfile(), []byte(strconv.Itoa(live)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	bin := filepath.Join(t.TempDir(), "dun")
	fakeDun(t, bin, "v2", 0, "")
	if err := check(l, bin, ln.Addr().String(), "", live); err != nil {
		t.Fatalf("handover check: %v", err)
	}
	if err := check(l, bin, ln.Addr().String(), "", 0); err == nil || !strings.Contains(err.Error(), "already runs") {
		t.Fatalf("plain check: %v", err)
	}
	if err := check(l, bin, "not an address", "", live); err == nil {
		t.Fatal("a bad HTTP address passed the handover check")
	}
}
