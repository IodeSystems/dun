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
