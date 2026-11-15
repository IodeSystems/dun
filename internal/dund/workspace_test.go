package dund

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun"
)

// fakeHome points userHome at a temporary directory for one test.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	old := userHome
	userHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHome = old })
	return home
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNormalizeWorkspace(t *testing.T) {
	home := fakeHome(t)
	aw4 := filepath.Join(home, "local", "src", "aw4")
	mkdirs(t, aw4)
	file := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		aw4:                     aw4,
		aw4 + "/":               aw4,
		"  " + aw4 + "/./ ":     aw4,
		"~/local/src/aw4":       aw4,
		"~":                     home,
		"~/":                    home,
		"local/src/aw4":         aw4, // the reported case: no leading slash is under the home
		"local/src/../src/aw4/": aw4,
	} {
		got, err := NormalizeWorkspace(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for in, msg := range map[string]string{
		"":                "enter a directory",
		"   ":             "enter a directory",
		"~bob/src":        "~user is not supported",
		"local/src/nope":  "no directory " + filepath.Join(home, "local/src/nope") + ": it does not exist",
		"/no/such/dir/at": "no directory /no/such/dir/at: it does not exist",
		"notes.txt":       file + " is a file, not a directory",
	} {
		_, err := NormalizeWorkspace(in)
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: error %v, want one containing %q", in, err, msg)
		}
	}
}

// Create reads the workspace the way NormalizeWorkspace does, and the
// session records the cleaned absolute path.
func TestCreateNormalizesTheWorkspace(t *testing.T) {
	home := fakeHome(t)
	ws := filepath.Join(home, "src", "aw4")
	mkdirs(t, ws)
	svc, sp, _ := newSvc(t)
	id, err := svc.Create(alice, "src/aw4/", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	ses, _ := svc.Get(alice, id)
	if ses.workspace != ws {
		t.Errorf("workspace %q, want %q", ses.workspace, ws)
	}
	if _, err := svc.Create(alice, "src/missing", nil, ""); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing dir: %v", err)
	}
	if n := len(sp.spawnArgs()); n != 1 {
		t.Errorf("%d engines spawned, want 1", n)
	}
}

func TestRootDecoderReadsBackRootDir(t *testing.T) {
	base := t.TempDir()
	plain := filepath.Join(base, "a", "b")
	dashed := filepath.Join(base, "my-repo", "x-y")
	spaced := filepath.Join(base, "two words", "z")
	// "p-q" and "p/q" encode alike; both exist, and each name decodes to one
	// that exists.
	ambig := filepath.Join(base, "p-q")
	mkdirs(t, plain, dashed, spaced, ambig, filepath.Join(base, "p", "q"))
	d := newRootDecoder()
	for _, p := range []string{plain, dashed, spaced, ambig} {
		got, ok := d.decode(filepath.Base(dun.RootDir(p)))
		if !ok || filepath.Base(dun.RootDir(got)) != filepath.Base(dun.RootDir(p)) || !isDir(got) {
			t.Errorf("%s: decoded %q, %v", p, got, ok)
		}
	}
	if got, ok := d.decode(filepath.Base(dun.RootDir(filepath.Join(base, "gone", "away")))); ok {
		t.Errorf("a removed directory decoded to %q", got)
	}
	if _, ok := d.decode("no-leading-marker"); ok {
		t.Error("a name without the leading - decoded")
	}
}

// Workspaces: registry sessions and on-disk session directories, most
// recent first, a directory that no longer exists, never had a conversation
// or is under the temp directory left out, the on-disk ones only with disk. A
// registry session counts wherever it runs.
func TestWorkspacesListsWhereSessionsRan(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	svc, _, _ := newSvc(t)
	base := t.TempDir()
	old, recent, gone, empty := filepath.Join(base, "old"), filepath.Join(base, "recent-one"),
		filepath.Join(base, "gone"), filepath.Join(base, "empty")
	// TMPDIR moves next to base: base is outside it, as a home is, and a
	// scratch run inside it is left out.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	scratch := filepath.Join(tmp, "scratch")
	mkdirs(t, old, recent, gone, empty, scratch)
	write := func(ws, name string, size int, age time.Duration) {
		dir := dun.RootDir(ws)
		mkdirs(t, dir)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		_ = os.Chtimes(p, mt, mt)
	}
	write(old, "19990101-000000.jsonl", 5, 72*time.Hour)
	write(recent, "19990102-000000.jsonl", 5, time.Hour)
	write(recent, "19990102-000000.plan.jsonl", 5, 0) // a sidecar is not activity
	write(gone, "19990103-000000.jsonl", 5, 0)
	write(empty, "19990104-000000.jsonl", 0, 0)
	write(scratch, "19990105-000000.jsonl", 5, 0)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	reg := t.TempDir()
	id, err := svc.Create(alice, reg, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	write(reg, id+".jsonl", 5, 0)

	got := svc.Workspaces(alice, true)
	var paths []string
	for _, w := range got {
		paths = append(paths, w.Path)
	}
	want := []string{reg, recent, old}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
	if !got[0].Running || got[1].Running || got[1].LastActive == "" {
		t.Errorf("rows %+v", got)
	}
	if mine := svc.Workspaces(alice, false); len(mine) != 1 || mine[0].Path != reg {
		t.Errorf("without disk: %+v", mine)
	}
	if len(svc.Workspaces(bob, false)) != 0 {
		t.Error("bob sees alice's workspace")
	}
}

func TestBrowseDirs(t *testing.T) {
	home := fakeHome(t)
	mkdirs(t, filepath.Join(home, "src", "aw4"), filepath.Join(home, "src", "Dun"),
		filepath.Join(home, "src", "dun-web"), filepath.Join(home, "src", ".git"),
		filepath.Join(home, ".config"), filepath.Join(home, "docs"))
	if err := os.WriteFile(filepath.Join(home, "src", "dunfile"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	names := func(l DirListing) string {
		var out []string
		for _, p := range l.Items {
			out = append(out, filepath.Base(p))
		}
		return strings.Join(out, " ")
	}
	cases := []struct {
		typed, dir, items, workspace, problem string
	}{
		{"", home, "docs src", home, ""},
		{"~", home, "docs src", home, ""},
		{"~/src/", filepath.Join(home, "src"), "aw4 Dun dun-web", filepath.Join(home, "src"), ""},
		{"src/du", filepath.Join(home, "src"), "Dun dun-web", filepath.Join(home, "src", "du"),
			"no directory " + filepath.Join(home, "src", "du") + ": it does not exist"},
		{"~/src/dunfile", filepath.Join(home, "src"), "", filepath.Join(home, "src", "dunfile"),
			filepath.Join(home, "src", "dunfile") + " is a file, not a directory"},
		{"~/src/.g", filepath.Join(home, "src"), ".git", filepath.Join(home, "src", ".g"),
			"no directory " + filepath.Join(home, "src", ".g") + ": it does not exist"},
		{"~/.", home, ".config", home, ""},
		{"~/nope/x", filepath.Join(home, "nope"), "", filepath.Join(home, "nope", "x"),
			"no directory " + filepath.Join(home, "nope", "x") + ": it does not exist"},
	}
	for _, c := range cases {
		l, err := BrowseDirs(c.typed)
		if err != nil {
			t.Errorf("%q: %v", c.typed, err)
			continue
		}
		if l.Dir != c.dir || names(l) != c.items || l.Workspace != c.workspace || l.Problem != c.problem {
			t.Errorf("%q: got dir %q items %q workspace %q problem %q", c.typed, l.Dir, names(l), l.Workspace, l.Problem)
		}
	}
	if _, err := BrowseDirs("~bob/"); err == nil {
		t.Error("~bob accepted")
	}
}

func TestBrowseDirsIsBounded(t *testing.T) {
	home := fakeHome(t)
	for i := 0; i < dirsMax+10; i++ {
		mkdirs(t, filepath.Join(home, fmt.Sprintf("d%03d", i)))
	}
	l, err := BrowseDirs("~/")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != dirsMax || !l.Truncated {
		t.Errorf("%d items, truncated %v", len(l.Items), l.Truncated)
	}
}
