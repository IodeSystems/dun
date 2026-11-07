package dun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func isoHarness(t *testing.T) (*Harness, string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	sf := filepath.Join(t.TempDir(), "sessions", "20261106-220000.jsonl")
	if err := os.MkdirAll(filepath.Dir(sf), 0o700); err != nil {
		t.Fatal(err)
	}
	return &Harness{cfg: Config{SessionFile: sf}}, tmp
}

// One dir per session under the temp dir; a child under its parent's.
func TestIsoTmpDir(t *testing.T) {
	t.Setenv("TMPDIR", "/x")
	if got := IsoTmpDir("/s/20261106-1.jsonl"); got != "/x/dun-20261106-1" {
		t.Errorf("root: %s", got)
	}
	if got := IsoTmpDir("/s/20261106-1.sub3.jsonl"); got != "/x/dun-20261106-1/child-3" {
		t.Errorf("child: %s", got)
	}
	if IsoTmpDir("") != "" {
		t.Error("an in-memory session has no isolation tmp")
	}
}

// A clipped result is written to both places, and the model is given the
// isolation tmp's path.
func TestSpillExec_WritesBothGivesTmpPath(t *testing.T) {
	h, tmp := isoHarness(t)
	p := h.spillExec("ls", "big output")
	if !strings.HasPrefix(p, filepath.Join(tmp, "dun-20261106-220000")+"/") {
		t.Fatalf("path is not in the isolation tmp: %s", p)
	}
	for _, f := range []string{p, filepath.Join(h.bgLogDir(), filepath.Base(p))} {
		b, err := os.ReadFile(f)
		if err != nil || !strings.Contains(string(b), "big output") {
			t.Errorf("%s: %q %v", f, b, err)
		}
	}
}

// A job log is written to both while it runs.
func TestCreateBoth_JobLogInBoth(t *testing.T) {
	h, _ := isoHarness(t)
	w, p := h.createBoth("job-1.log")
	if w == nil {
		t.Fatal("no log")
	}
	_, _ = w.Write([]byte("line\n"))
	_ = w.Close()
	for _, f := range []string{p, filepath.Join(h.bgLogDir(), "job-1.log")} {
		if b, _ := os.ReadFile(f); string(b) != "line\n" {
			t.Errorf("%s: %q", f, b)
		}
	}
}

// /tmp wiped (a reboot): the isolation comes up with its saved files back,
// and one that is still there is not touched.
func TestRestoreIsoTmp_AfterWipe(t *testing.T) {
	h, _ := isoHarness(t)
	gone := h.spillExec("a", "first")
	kept := h.spillExec("b", "second")
	if err := os.WriteFile(kept, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	h.restoreIsoTmp()
	if b, _ := os.ReadFile(gone); !strings.Contains(string(b), "first") {
		t.Errorf("not restored: %q", b)
	}
	if b, _ := os.ReadFile(kept); string(b) != "live" {
		t.Errorf("an existing file was overwritten: %q", b)
	}
}

// --docker sees the isolation tmp at the same path, read-only.
func TestDockerExec_MountsIsoTmpAtItsPath(t *testing.T) {
	args := DockerExec{Dir: "/w", Image: "i", TmpDir: "/tmp/dun-x"}.runArgs("n", "true")
	if !slices.Contains(args, "/tmp/dun-x:/tmp/dun-x:ro") {
		t.Errorf("no same-path mount: %v", args)
	}
	if slices.ContainsFunc(DockerExec{Dir: "/w", Image: "i"}.runArgs("n", "true"), func(a string) bool {
		return strings.HasPrefix(a, "/tmp/dun-")
	}) {
		t.Error("mounted a tmp with none set")
	}
}
