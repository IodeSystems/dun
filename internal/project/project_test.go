package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// First sight mints an id (the directory's name plus a random suffix) and
// saves it; every later sight gets the same one. Reading never registers.
func TestProject_MintThenReuse(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "My Repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if Resolve(dir) != nil {
		t.Fatal("a directory dun has never seen has no project")
	}
	if _, err := os.Stat(registryFile()); !os.IsNotExist(err) {
		t.Fatal("Resolve must not create the registry")
	}
	p, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.ID, "my-repo-") || len(p.ID) != len("my-repo-")+6 {
		t.Errorf("id = %q, want my-repo-<6 hex>", p.ID)
	}
	if st, err := os.Stat(p.StateDir); err != nil || !st.IsDir() {
		t.Fatalf("state dir not created: %v", err)
	}
	again, err := Ensure(dir)
	if err != nil || again.ID != p.ID {
		t.Fatalf("second sight got %q (%v), want %q", again.ID, err, p.ID)
	}
	if r := Resolve(dir); r == nil || r.ID != p.ID {
		t.Fatalf("Resolve = %+v, want id %q", r, p.ID)
	}
	var reg registry
	b, _ := os.ReadFile(registryFile())
	if err := json.Unmarshal(b, &reg); err != nil || reg.Projects[RealPath(dir)] != p.ID {
		t.Fatalf("registry %s does not map the dir: %v", b, err)
	}
}

// The registry is written temp+rename under a lock: many first sights at once
// must all survive, and no temp file is left behind.
func TestProject_RegistryConcurrentMint(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	base := t.TempDir()
	const n = 16
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		dir := filepath.Join(base, fmt.Sprintf("p%d", i))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := Ensure(dir)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = p.ID
		}()
	}
	wg.Wait()
	reg, err := readRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Projects) != n {
		t.Fatalf("registry holds %d projects, want %d — a concurrent write was lost", len(reg.Projects), n)
	}
	for i := range n {
		if got := reg.Projects[RealPath(filepath.Join(base, fmt.Sprintf("p%d", i)))]; got != ids[i] {
			t.Errorf("p%d: registry %q, minted %q", i, got, ids[i])
		}
	}
	left, _ := filepath.Glob(filepath.Join(dunHome(), ".projects.json.tmp-*"))
	if len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// List returns every registry entry, sorted by id, and creates nothing.
func TestProject_List(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	base := t.TempDir()
	var want []string
	for _, name := range []string{"zeta", "alpha", "mid"} {
		dir := filepath.Join(base, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p, err := Ensure(dir)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, p.ID)
	}
	entries, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Fatalf("List = %d entries, want %d", len(entries), len(want))
	}
	for i, e := range entries {
		if e.ID == "" || e.Source == "" {
			t.Errorf("entry %d has an empty field: %+v", i, e)
		}
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("entries not sorted by id: %v", ids)
	}
	for _, id := range want {
		if Find(id) == nil {
			t.Errorf("Find(%q) = nil", id)
		}
	}
	if Find("no-such-id") != nil {
		t.Error("Find(no-such-id) should be nil")
	}
}

// Forget removes id's registry entry and leaves the project's state
// directory — sessions, memory, worktrees — in place.
func TestProject_Forget(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "My Repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Session state and memory state live under the project's state dir.
	sessionFile := filepath.Join(p.StateDir, "session-abc.json")
	memoryFile := filepath.Join(p.StateDir, "memory", "note.md")
	if err := os.MkdirAll(filepath.Dir(memoryFile), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{sessionFile, memoryFile} {
		if err := os.WriteFile(f, []byte("state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Forget(p.ID); err != nil {
		t.Fatal(err)
	}
	if r := Resolve(dir); r != nil {
		t.Fatalf("Resolve after Forget = %+v, want nil", r)
	}
	if Find(p.ID) != nil {
		t.Error("registry entry still present after Forget")
	}
	var reg registry
	b, _ := os.ReadFile(registryFile())
	if err := json.Unmarshal(b, &reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Projects) != 0 {
		t.Errorf("registry after Forget has %d entries, want 0", len(reg.Projects))
	}
	// The state — sessions and memory — is untouched.
	if _, err := os.Stat(sessionFile); err != nil {
		t.Errorf("session state removed by Forget: %v", err)
	}
	if _, err := os.Stat(memoryFile); err != nil {
		t.Errorf("memory state removed by Forget: %v", err)
	}
	if _, err := os.Stat(p.StateDir); err != nil {
		t.Errorf("state dir removed by Forget: %v", err)
	}
	// Forgetting an unknown id is an error, not a silent no-op.
	if err := Forget(p.ID); err == nil {
		t.Error("second Forget should fail: no such project")
	}
	if err := Forget("no-such-id"); err == nil {
		t.Error("Forget(no-such-id) should fail")
	}
	if err := Forget(""); err == nil {
		t.Error("Forget(\"\") should fail")
	}
}

// Repoint moves an entry to a new source directory: the id and everything
// filed under it follow, the old source is unregistered, and a source that
// is already somebody else's project is refused.
func TestProject_Repoint(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	base := t.TempDir()
	here := filepath.Join(base, "here")
	there := filepath.Join(base, "there")
	other := filepath.Join(base, "other")
	for _, d := range []string{here, there, other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := Ensure(here)
	if err != nil {
		t.Fatal(err)
	}
	otherP, err := Ensure(other)
	if err != nil {
		t.Fatal(err)
	}

	if err := Repoint(p.ID, there); err != nil {
		t.Fatal(err)
	}
	// The id now resolves from the new source, not the old.
	if r := Resolve(there); r == nil || r.ID != p.ID {
		t.Fatalf("Resolve(new) = %+v, want id %q", r, p.ID)
	}
	if r := Resolve(here); r != nil {
		t.Fatalf("Resolve(old) = %+v, want nil", r)
	}
	if e := Find(p.ID); e == nil || e.Source != RealPath(there) {
		t.Errorf("Find = %+v, want source %q", e, RealPath(there))
	}

	// The new source being already registered to a DIFFERENT project is
	// refused, and nothing changes.
	if err := Repoint(p.ID, other); err == nil {
		t.Fatal("Repoint onto another project's source should fail")
	}
	if r := Resolve(there); r == nil || r.ID != p.ID {
		t.Fatalf("a refused Repoint changed the registry: %+v", r)
	}
	if Find(otherP.ID) == nil {
		t.Error("the other project was disturbed by a refused Repoint")
	}

	// A Repoint of the new source onto itself is allowed and a no-op.
	if err := Repoint(p.ID, there); err != nil {
		t.Errorf("Repoint onto its own current source: %v", err)
	}

	// Missing ids and sources are refused.
	if err := Repoint("no-such-id", there); err == nil {
		t.Error("Repoint of an unknown id should fail")
	}
	if err := Repoint(p.ID, ""); err == nil {
		t.Error("Repoint to an empty source should fail")
	}
}
