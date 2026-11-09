package ragd

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/iodesystems/raglit"
)

// docPaths is st's document paths, sorted.
func docPaths(t *testing.T, st *raglit.Store) []string {
	t.Helper()
	docs, err := documents(st)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for p := range docs {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// The worktree copies an older dund stored in the main checkout's index
// (paths under the worktree) are removed by the first rag_index of the
// main checkout, once. A path spelled through a symlink into the main
// checkout is under it by fsutil.Canon and kept; so is a document that is
// not a file:// path.
func TestIndexDropsOldWorktreeCopies(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h := open(t, t.TempDir())
	main, wt := branchRepo(t)
	link := filepath.Join(realTemp(t), "link")
	if err := os.Symlink(main, link); err != nil {
		t.Fatal(err)
	}

	// The old state: the worktree's files and a link-spelled main file in
	// dun/code, and a text document, before any rag_index of this dund.
	st, err := h.store("dun/code", true)
	if err != nil {
		t.Fatal(err)
	}
	var items []raglit.EnqueueItem
	for _, f := range []string{filepath.Join(wt, "b.go"), filepath.Join(wt, "c.go"), filepath.Join(link, "d.go")} {
		items = append(items, raglit.EnqueueItem{URL: "file://" + f})
	}
	if _, err := st.EnqueueMany(items); err != nil {
		t.Fatal(err)
	}
	if err := ingestText(st, "note://kept", "kept", "a note", nil); err != nil {
		t.Fatal(err)
	}
	waitStore(t, st)
	if got := docPaths(t, st); len(got) != 4 {
		t.Fatalf("old state: %v, want 4 documents", got)
	}

	r := indexAt(t, h, main)
	if r.Code.Removed != 2 {
		t.Errorf("first index of the main checkout: %+v, want the 2 worktree copies removed", r.Code)
	}
	f := func(dir, name string) string { return "file://" + filepath.Join(dir, name) }
	want := []string{f(main, "a.go"), f(main, "b.go"), f(main, "d.go"), f(link, "d.go"), "note://kept"}
	sort.Strings(want)
	if got := docPaths(t, st); !slices.Equal(got, want) {
		t.Errorf("after the first index: %v, want %v", got, want)
	}
	if v, ok := st.Meta(outsideKey); !ok || v != main {
		t.Errorf("%s = %q, %v; want %q", outsideKey, v, ok, main)
	}

	// Once: a document outside the root added since stays.
	if _, err := st.EnqueueMany([]raglit.EnqueueItem{{URL: f(wt, "c.go")}}); err != nil {
		t.Fatal(err)
	}
	waitStore(t, st)
	if r := indexAt(t, h, main); r.Code.Removed != 0 {
		t.Errorf("second index: %+v, want nothing removed", r.Code)
	}
}

// A rag_index root that is not a checkout's top (a subdirectory) removes
// nothing outside it and leaves the cleanup for the top.
func TestIndexDropsNothingFromASubdirectory(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h := open(t, t.TempDir())
	main, _ := branchRepo(t)
	sub := filepath.Join(main, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sub, "e.go"), "package sub\n\n// Eel swims.\nfunc Eel() {}\n")
	st, err := h.store("dun/code", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueMany([]raglit.EnqueueItem{{URL: "file://" + filepath.Join(main, "a.go")}}); err != nil {
		t.Fatal(err)
	}
	waitStore(t, st)
	if r := indexAt(t, h, sub); r.Code.Removed != 0 {
		t.Errorf("index of a subdirectory: %+v, want nothing removed", r.Code)
	}
	if got := docPaths(t, st); len(got) != 2 {
		t.Errorf("after the subdirectory: %v, want a.go and sub/e.go", got)
	}
	if _, ok := st.Meta(outsideKey); ok {
		t.Errorf("a subdirectory set %s", outsideKey)
	}
}
