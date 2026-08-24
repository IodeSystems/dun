package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildInput(t *testing.T) {
	for _, n := range []string{"main.go", "serve.html", "xterm.css", "addon.js", "go.mod", "go.sum"} {
		if !buildInput(n) {
			t.Errorf("buildInput(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"README.md", "dun", ".gitignore", "notes.txt", "plan.md"} {
		if buildInput(n) {
			t.Errorf("buildInput(%q) = true, want false", n)
		}
	}
}

func TestSourceNewerThan(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !sourceNewerThan(dir, time.Unix(0, 0)) {
		t.Fatal("a .go file must count as newer than the epoch")
	}
	if sourceNewerThan(dir, time.Now().Add(time.Hour)) {
		t.Fatal("nothing should be newer than an hour from now")
	}

	// A tree with only non-build files never triggers a rebuild.
	docs := t.TempDir()
	if err := os.WriteFile(filepath.Join(docs, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sourceNewerThan(docs, time.Unix(0, 0)) {
		t.Fatal("non-build files must not count as source changes")
	}
}

func TestSourceNewerThanFollowsLocalReplace(t *testing.T) {
	root, dep := t.TempDir(), t.TempDir()
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(root, "go.mod", "module m\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => "+dep+"\n")
	write(root, "main.go", "package main")
	write(dep, "go.mod", "module example.com/dep")
	write(dep, "dep.go", "package dep")

	// Binary newer than everything → nothing to do.
	future := time.Now().Add(time.Hour)
	if sourceNewerThan(root, future) {
		t.Fatal("nothing is newer than an hour from now")
	}
	// An edit in the REPLACED module alone must trigger a rebuild — the case
	// that kept a stale agentkit running for two weeks.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dep, "dep.go"), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "main.go"), past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "go.mod"), past, past); err != nil {
		t.Fatal(err)
	}
	if !sourceNewerThan(root, past.Add(time.Minute)) {
		t.Fatal("an edit in a local replace target must count as a source change")
	}
}

func TestLocalReplaceDirs(t *testing.T) {
	root, dep := t.TempDir(), t.TempDir()
	rel := filepath.Join(root, "sub")
	if err := os.Mkdir(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module m\n" +
		"replace example.com/abs => " + dep + "\n" +
		"replace example.com/rel => ./sub\n" +
		"replace example.com/dup => " + dep + "\n" +
		"replace example.com/gone => ../nope-does-not-exist\n" +
		"replace example.com/mod => example.com/other v1.2.3\n" +
		"// replace example.com/cmt => " + dep + "\n" +
		"replace (\n\texample.com/blk v1.0.0 => ./sub\n)\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	got := localReplaceDirs(root)
	want := []string{dep, rel}
	if len(got) != len(want) {
		t.Fatalf("localReplaceDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("localReplaceDirs = %v, want %v", got, want)
		}
	}
}

func TestTreeNewerThanSkipsNestedModules(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "tools", "ttydrive")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ path, body string }{
		{filepath.Join(nested, "go.mod"), "module t"},
		{filepath.Join(nested, "main.go"), "package main"},
	} {
		if err := os.WriteFile(f.path, []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if treeNewerThan(root, time.Unix(0, 0)) {
		t.Fatal("a nested module is a separate build and must not force a rebuild")
	}
	// ...but the root's own go.mod still counts.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module m"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !treeNewerThan(root, time.Unix(0, 0)) {
		t.Fatal("the root module's own go.mod is a build input")
	}
}
