package dun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	if ResolveProject(dir) != nil {
		t.Fatal("a directory dun has never seen has no project")
	}
	if _, err := os.Stat(registryFile()); !os.IsNotExist(err) {
		t.Fatal("ResolveProject must not create the registry")
	}
	p, err := EnsureProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.ID, "my-repo-") || len(p.ID) != len("my-repo-")+6 {
		t.Errorf("id = %q, want my-repo-<6 hex>", p.ID)
	}
	if st, err := os.Stat(p.StateDir); err != nil || !st.IsDir() {
		t.Fatalf("state dir not created: %v", err)
	}
	again, err := EnsureProject(dir)
	if err != nil || again.ID != p.ID {
		t.Fatalf("second sight got %q (%v), want %q", again.ID, err, p.ID)
	}
	if r := ResolveProject(dir); r == nil || r.ID != p.ID {
		t.Fatalf("ResolveProject = %+v, want id %q", r, p.ID)
	}
	var reg registry
	b, _ := os.ReadFile(registryFile())
	if err := json.Unmarshal(b, &reg); err != nil || reg.Projects[realPath(dir)] != p.ID {
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
			p, err := EnsureProject(dir)
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
		if got := reg.Projects[realPath(filepath.Join(base, fmt.Sprintf("p%d", i)))]; got != ids[i] {
			t.Errorf("p%d: registry %q, minted %q", i, got, ids[i])
		}
	}
	left, _ := filepath.Glob(filepath.Join(DunHome(), ".projects.json.tmp-*"))
	if len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}
