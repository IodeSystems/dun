package ragd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/raglit"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/proto"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	args = append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// realTemp is a temp directory spelled with its symlinks resolved, as
// dund spells the paths it stores.
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// branchRepo is a committed repo of a.go, b.go and d.go and a linked
// worktree of it, beside it, where b.go changed, c.go is new and a.go is
// deleted (d.go as committed).
func branchRepo(t *testing.T) (main, wt string) {
	t.Helper()
	base := realTemp(t)
	main = filepath.Join(base, "repo")
	if err := os.Mkdir(main, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(main, "a.go"), "package a\n\n// Aardvark digs.\nfunc Aardvark() {}\n")
	write(t, filepath.Join(main, "b.go"), "package a\n\n// Bison grazes.\nfunc Bison() {}\n")
	write(t, filepath.Join(main, "d.go"), "package a\n\n// Dingo howls.\nfunc Dingo() {}\n")
	git(t, main, "init", "-q")
	git(t, main, "add", ".")
	git(t, main, "commit", "-q", "-m", "init")
	wt = filepath.Join(base, "wt")
	git(t, main, "worktree", "add", "-q", "-b", "wt", wt)
	write(t, filepath.Join(wt, "b.go"), "package a\n\n// Buffalo roams.\nfunc Buffalo() {}\n")
	write(t, filepath.Join(wt, "c.go"), "package a\n\n// Cobra coils.\nfunc Cobra() {}\n")
	if err := os.Remove(filepath.Join(wt, "a.go")); err != nil {
		t.Fatal(err)
	}
	return main, wt
}

// indexAt is rag_index of root for project dun: the workspace rules, Index,
// and a wait for the jobs of the index or branch it wrote.
func indexAt(t *testing.T, h *Host, root string) *proto.RagIndexed {
	t.Helper()
	files, _ := dun.IndexFiles(root)
	r, err := h.Index(proto.RagIndex{Project: "dun", Root: root}, files)
	if err != nil {
		t.Fatal(err)
	}
	name := "dun/code"
	if _, ok := h.Worktree(root); ok {
		name = branchName("dun", "code", fsutil.Canon(root))
	}
	st, err := h.Existing(name)
	if err != nil {
		t.Fatalf("%s is not open: %v", name, err)
	}
	waitStore(t, st)
	return r
}

func waitStore(t *testing.T, st *raglit.Store) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		busy := 0
		for _, s := range []raglit.JobState{raglit.JobPending, raglit.JobRunning} {
			js, err := st.Jobs(string(s), 100)
			if err != nil {
				t.Fatal(err)
			}
			busy += len(js)
		}
		if busy == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d jobs still queued after 30s", busy)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// hitPaths is the paths of a code search for q from the workspace root.
func hitPaths(t *testing.T, h *Host, root, q string) []string {
	t.Helper()
	r := search(t, h, proto.RagSearch{Project: "dun", Index: "code", Query: q, Mode: "fts", Root: root,
		Filter: proto.RagFilter{PathPrefix: "file://" + root + "/"}})
	var out []string
	for _, x := range r.Hits {
		out = append(out, x.Path)
	}
	return out
}

func wantHits(t *testing.T, h *Host, root, q string, want ...string) {
	t.Helper()
	got := hitPaths(t, h, root, q)
	if len(got) != len(want) {
		t.Errorf("search %q from %s: %v, want %v", q, root, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("search %q from %s: %v, want %v", q, root, got, want)
			return
		}
	}
}

// A worktree is a branch of the main checkout's index: it finds its own
// b and c and not the deleted a, the main checkout's search is unchanged,
// and the branch stores only the files that differ.
func TestWorktreeIsABranch(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h := open(t, t.TempDir())
	main, wt := branchRepo(t)

	if r := indexAt(t, h, main); r.Code.Queued != 3 {
		t.Fatalf("main index: %+v, want 3 code files queued", r)
	}
	r := indexAt(t, h, wt)
	if r.Code.Queued != 2 || r.Code.Removed != 1 || r.Code.Current != 1 || r.Deferred != 0 {
		t.Fatalf("worktree index: %+v, want b and c queued, a removed, d current", r)
	}

	f := func(dir, name string) string { return "file://" + filepath.Join(dir, name) }
	wantHits(t, h, wt, "buffalo", f(wt, "b.go"))
	wantHits(t, h, wt, "cobra", f(wt, "c.go"))
	wantHits(t, h, wt, "dingo", f(wt, "d.go")) // the parent's, at its worktree path
	wantHits(t, h, wt, "aardvark")             // deleted in the worktree
	wantHits(t, h, wt, "bison")                // the old b, shadowed

	wantHits(t, h, main, "aardvark", f(main, "a.go"))
	wantHits(t, h, main, "bison", f(main, "b.go"))
	wantHits(t, h, main, "dingo", f(main, "d.go"))
	wantHits(t, h, main, "buffalo")
	wantHits(t, h, main, "cobra")

	br, err := h.Existing(branchName("dun", "code", wt))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := br.BranchDiff()
	if err != nil {
		t.Fatal(err)
	}
	own := map[string]string{}
	for _, c := range cs {
		own[c.Path] = c.Change
	}
	if len(own) != 3 || own[f(wt, "b.go")] != "changed" || own[f(wt, "c.go")] != "added" || own[f(wt, "a.go")] != "deleted" {
		t.Errorf("branch diff %v, want b changed, c added, a deleted, and no copy of d", own)
	}
	ds, err := documents(br)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 3 {
		t.Errorf("the branch shows %d documents, want b, c, d", len(ds))
	}
	pst, _ := h.Existing("dun/code")
	if pd, _ := documents(pst); len(pd) != 3 {
		t.Errorf("the parent holds %d documents, want its own a, b, d", len(pd))
	}

	if r := indexAt(t, h, wt); r.Queued != 0 || r.Removed != 0 || r.Current != 3 {
		t.Errorf("worktree re-index: %+v, want nothing queued, 3 current", r)
	}
	if r := indexAt(t, h, main); r.Queued != 0 || r.Removed != 0 || r.Current != 3 {
		t.Errorf("main re-index after the worktree's: %+v, want nothing queued, 3 current", r)
	}
}

// A worktree whose main checkout is not indexed is refused by Index (the
// rag_index handler indexes the main checkout first).
func TestWorktreeNeedsItsMainIndexed(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h := open(t, t.TempDir())
	_, wt := branchRepo(t)
	files, _ := dun.IndexFiles(wt)
	if _, err := h.Index(proto.RagIndex{Project: "dun", Root: wt}, files); err == nil {
		t.Fatal("a worktree indexed with no main checkout index")
	}
	if r := search(t, h, proto.RagSearch{Project: "dun", Index: "code", Query: "cobra", Mode: "fts", Root: wt}); len(r.Hits) != 0 || r.Note == "" {
		t.Errorf("search of an unindexed worktree: %+v, want no hits and a note", r)
	}
}

// GC removes a branch whose worktree is gone, by command and at start, and
// leaves a live one.
func TestGCRemovesDeadBranches(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ragRoot := t.TempDir()
	h := open(t, ragRoot)
	main, wt := branchRepo(t)
	indexAt(t, h, main)
	indexAt(t, h, wt)
	home := filepath.Join(ragRoot, branchName("dun", "code", wt))

	if r, err := h.GC(); err != nil || r.Removed != 0 || r.Branches != 1 {
		t.Fatalf("gc with the worktree alive: %+v, %v", r, err)
	}
	git(t, main, "worktree", "remove", "--force", wt)
	if r, err := h.GC(); err != nil || r.Removed != 1 {
		t.Fatalf("gc after the worktree was removed: %+v, %v", r, err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("branch home still there: %v", err)
	}
	if _, err := h.Existing(branchName("dun", "code", wt)); err == nil {
		t.Error("the removed branch is still open")
	}
	wantHits(t, h, main, "aardvark", "file://"+filepath.Join(main, "a.go"))

	// At start: a second worktree, gone while dund is down.
	wt2 := filepath.Join(filepath.Dir(wt), "wt2")
	git(t, main, "worktree", "add", "-q", "-b", "wt2", wt2)
	indexAt(t, h, wt2)
	h.Close(time.Second)
	git(t, main, "worktree", "remove", "--force", wt2)
	open(t, ragRoot)
	if _, err := os.Stat(filepath.Join(ragRoot, branchName("dun", "code", wt2))); !os.IsNotExist(err) {
		t.Errorf("start left the dead branch: %v", err)
	}
}

// A root reached through a symlink is stored, searched and re-indexed as
// one spelling: its resolved path.
func TestRootThroughASymlink(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h := open(t, t.TempDir())
	main, wt := branchRepo(t)
	link := filepath.Join(realTemp(t), "link")
	if err := os.Symlink(main, link); err != nil {
		t.Fatal(err)
	}
	wlink := filepath.Join(filepath.Dir(link), "wlink")
	if err := os.Symlink(wt, wlink); err != nil {
		t.Fatal(err)
	}

	if r := indexAt(t, h, link); r.Code.Queued != 3 {
		t.Fatalf("index through the link: %+v", r)
	}
	f := func(dir, name string) string { return "file://" + filepath.Join(dir, name) }
	// Searched through the link: the stored paths are the resolved ones.
	wantHits(t, h, link, "aardvark", f(main, "a.go"))
	if r := indexAt(t, h, main); r.Queued != 0 || r.Removed != 0 || r.Current != 3 {
		t.Errorf("index by the resolved path after the link: %+v, want all current", r)
	}

	// The worktree through its link is a branch over the main checkout.
	if r := indexAt(t, h, wlink); r.Code.Queued != 2 || r.Code.Removed != 1 || r.Code.Current != 1 {
		t.Fatalf("worktree through its link: %+v, want b and c queued, a removed, d current", r)
	}
	wantHits(t, h, wlink, "dingo", f(wt, "d.go"))
	wantHits(t, h, wlink, "aardvark")
	if r := indexAt(t, h, wt); r.Queued != 0 || r.Removed != 0 || r.Current != 3 {
		t.Errorf("worktree by its resolved path: %+v, want all current", r)
	}
}
