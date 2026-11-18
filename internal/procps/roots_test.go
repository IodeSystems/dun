package procps

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// testdata/status.json is a real `poly-lsp-mcp status --json`, paths
// anonymized; the daemon is pid 200 of machine().
func fixtureRoots(t *testing.T) []Root {
	t.Helper()
	b, err := os.ReadFile("testdata/status.json")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := ParseStatus(b, 200)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestParseStatus_CollapsesLanguages(t *testing.T) {
	rs := fixtureRoots(t)
	if len(rs) != 7 {
		t.Fatalf("roots: %d", len(rs))
	}
	var a Root
	for _, r := range rs {
		if r.Root == "/w/a" {
			a = r
		}
	}
	if strings.Join(a.Langs, ",") != "go,typescript" || a.Children != 2 || a.Running != 0 ||
		a.Files <= 0 || a.IdleSec < 0 {
		t.Errorf("/w/a: %+v", a)
	}
	empty := rs[5]
	if empty.Root != "/tmp/scratch/ws" || empty.Children != 0 || empty.IdleSec != -1 {
		t.Errorf("a root with no child LSPs: %+v", empty)
	}
	if other, _ := ParseStatus([]byte(`[{"pid":9,"root":"/x"}]`), 200); len(other) != 0 {
		t.Errorf("another daemon's roots kept: %+v", other)
	}
	if _, err := ParseStatus([]byte("PID ROOT"), 0); err == nil {
		t.Error("a table parsed as JSON")
	}
}

func TestAttachRoots_OwnerAndRender(t *testing.T) {
	snap, list, cwd := machine()
	rows := Attribute(snap, list, cwd)
	rs := fixtureRoots(t)
	// Make /w/a's Go server run, to show the count.
	for i := range rs {
		if rs[i].Root == "/w/a" {
			rs[i].Running = 1
		}
	}
	AttachRoots(rows, 200, rs, nil, list)
	d := byPID(rows)[200]
	if len(d.Roots) != 7 || d.Roots[0].Root != "/w/a" || d.Roots[0].Group != "20261117-093015" {
		t.Fatalf("owned root not first: %+v", d.Roots)
	}
	if last := d.Roots[6]; last.Group != GroupNone {
		t.Errorf("unowned root last: %+v", last)
	}
	out := Table(Groups(rows), 0)
	for _, want := range []string{
		"root a  langs go,ts  children 1 running  index ",
		"root raglit  langs go,ts  children 0 running",
		"no child LSPs",
		"kept, no client",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	// The owner is in the SESSION column of the root's line.
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "root a ") && !strings.Contains(ln, "1117-0930") {
			t.Errorf("root a not under its session: %q", ln)
		}
		if strings.Contains(ln, "root raglit") && !strings.Contains(ln, "(none)") {
			t.Errorf("raglit has no owner: %q", ln)
		}
	}
}

func TestAttachRoots_Error(t *testing.T) {
	snap, list, cwd := machine()
	rows := Attribute(snap, list, cwd)
	AttachRoots(rows, 200, nil, errors.New("no answer in 2s"), list)
	if got := byPID(rows)[200].RootsErr; got != "poly-lsp-mcp status failed: no answer in 2s" {
		t.Errorf("error: %q", got)
	}
	if out := Table(Groups(rows), 0); !strings.Contains(out, "poly-lsp-mcp status failed: no answer in 2s") ||
		strings.Contains(out, "\x1b") {
		t.Errorf("piped table:\n%s", out)
	}
	if out := Table(Groups(rows), 100); !strings.Contains(out, "\x1b[2m") {
		t.Errorf("tty table not dim:\n%s", out)
	}
}

func TestRootNames_Collision(t *testing.T) {
	n := RootNames([]Root{{Root: "/x/app"}, {Root: "/y/app"}, {Root: "/x/lib"}})
	if n[0] != "/x/app" || n[1] != "/y/app" || n[2] != "lib" {
		t.Errorf("names: %v", n)
	}
}
