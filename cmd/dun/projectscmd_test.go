package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/project"
)

// list shows every registered project with its id and path, and marks
// missing paths as moved.
func TestProjects_List(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	// Register two projects.
	ws1 := t.TempDir()
	ws2 := t.TempDir()
	if _, err := project.Ensure(ws1); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Ensure(ws2); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if rc := projectsCmd(nil, &out, &bytes.Buffer{}); rc != 0 {
		t.Fatalf("projectsCmd: %d", rc)
	}
	got := out.String()
	entries, _ := project.List()
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	for _, e := range entries {
		if !strings.Contains(got, e.ID) {
			t.Errorf("output missing id %q:\n%s", e.ID, got)
		}
		if !strings.Contains(got, e.Source) {
			t.Errorf("output missing path %q:\n%s", e.Source, got)
		}
	}
}

// list marks a project whose path no longer exists.
func TestProjects_List_Moved(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	p, err := project.Ensure(ws)
	if err != nil {
		t.Fatal(err)
	}
	// Remove the directory to simulate a move.
	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if rc := projectsCmd(nil, &out, &bytes.Buffer{}); rc != 0 {
		t.Fatalf("projectsCmd: %d", rc)
	}
	got := out.String()
	if !strings.Contains(got, p.ID) {
		t.Fatalf("output missing id %q:\n%s", p.ID, got)
	}
	if !strings.Contains(got, "(moved)") {
		t.Errorf("expected (moved) marker:\n%s", got)
	}
}

// forget removes the registry entry; sessions and memory are left in place.
func TestProjects_Forget(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	p, err := project.Ensure(ws)
	if err != nil {
		t.Fatal(err)
	}
	// Create a session file and a memory file to verify they survive.
	sessDir := project.SessionDir(p.ID)
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "s1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Memory lives under $DUN_HOME/projects/<id>/
	memDir := filepath.Join(t.TempDir(), "projects", p.ID)
	_ = memDir // project state dir already created by Ensure

	var out, errb bytes.Buffer
	if rc := projectsCmd([]string{"forget", p.ID}, &out, &errb); rc != 0 {
		t.Fatalf("forget: %d %s", rc, errb.String())
	}
	// Registry entry is gone.
	if e := project.Find(p.ID); e != nil {
		t.Fatalf("entry still in registry after forget: %+v", e)
	}
	// Session file is still there.
	if _, err := os.Stat(filepath.Join(sessDir, "s1.jsonl")); err != nil {
		t.Errorf("session file removed by forget: %v", err)
	}
	// State directory is still there.
	if _, err := os.Stat(p.StateDir); err != nil {
		t.Errorf("state dir removed by forget: %v", err)
	}
}

// forget with an unknown id is an error.
func TestProjects_Forget_Unknown(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if rc := projectsCmd([]string{"forget", "no-such-id"}, &out, &errb); rc == 0 {
		t.Fatal("want non-zero exit for unknown id")
	}
}

// re-point moves an entry to a new path; sessions and memory follow.
func TestProjects_Repoint(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws1 := t.TempDir()
	p, err := project.Ensure(ws1)
	if err != nil {
		t.Fatal(err)
	}
	// Create a session file.
	sessDir := project.SessionDir(p.ID)
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "s1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Move to a new path.
	ws2 := t.TempDir()
	var out, errb bytes.Buffer
	if rc := projectsCmd([]string{"re-point", p.ID, ws2}, &out, &errb); rc != 0 {
		t.Fatalf("re-point: %d %s", rc, errb.String())
	}
	// The entry now points to ws2.
	e := project.Find(p.ID)
	if e == nil {
		t.Fatal("entry gone after re-point")
	}
	if e.Source != project.RealPath(ws2) {
		t.Fatalf("source = %q, want %q", e.Source, project.RealPath(ws2))
	}
	// Session file is still there (keyed by id, not path).
	if _, err := os.Stat(filepath.Join(sessDir, "s1.jsonl")); err != nil {
		t.Errorf("session file lost after re-point: %v", err)
	}
}

// re-point to a path already registered to a different id is refused.
func TestProjects_Repoint_Conflict(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws1 := t.TempDir()
	ws2 := t.TempDir()
	p1, err := project.Ensure(ws1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := project.Ensure(ws2); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if rc := projectsCmd([]string{"re-point", p1.ID, ws2}, &out, &errb); rc == 0 {
		t.Fatal("want non-zero exit for path conflict")
	}
	if !strings.Contains(errb.String(), "already registered") {
		t.Errorf("want 'already registered' in %q", errb.String())
	}
}

// A moved repo is recovered: re-point the old id to the new path and its
// sessions and memory are the ones it had before the move.
func TestProjects_MovedRepoRecovered(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws1 := t.TempDir()
	p, err := project.Ensure(ws1)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate sessions and memory.
	sessDir := project.SessionDir(p.ID)
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessFile := filepath.Join(sessDir, "old-session.jsonl")
	if err := os.WriteFile(sessFile, []byte(`{"msg":"hello"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	memFile := filepath.Join(p.StateDir, "dun.local.json")
	if err := os.WriteFile(memFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The checkout moves (old path disappears).
	if err := os.RemoveAll(ws1); err != nil {
		t.Fatal(err)
	}
	// List should mark it moved.
	var out bytes.Buffer
	if rc := projectsCmd(nil, &out, &bytes.Buffer{}); rc != 0 {
		t.Fatalf("list: %d", rc)
	}
	if !strings.Contains(out.String(), "(moved)") {
		t.Fatalf("expected (moved) in:\n%s", out.String())
	}
	// Re-point to the new path.
	ws2 := t.TempDir()
	errb := &bytes.Buffer{}
	if rc := projectsCmd([]string{"re-point", p.ID, ws2}, &out, errb); rc != 0 {
		t.Fatalf("re-point: %d %s", rc, errb.String())
	}
	// Sessions are the same ones.
	data, err := os.ReadFile(sessFile)
	if err != nil {
		t.Fatalf("session file lost: %v", err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Errorf("session content changed: %q", data)
	}
	// Memory is the same.
	if _, err := os.Stat(memFile); err != nil {
		t.Errorf("memory file lost: %v", err)
	}
	// The registry now points to ws2.
	e := project.Find(p.ID)
	if e == nil {
		t.Fatal("entry gone")
	}
	if e.Source != project.RealPath(ws2) {
		t.Fatalf("source = %q, want %q", e.Source, project.RealPath(ws2))
	}
}

// help text is printed.
func TestProjects_Help(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	var out bytes.Buffer
	if rc := projectsCmd([]string{"help"}, &out, &bytes.Buffer{}); rc != 0 {
		t.Fatalf("help: %d", rc)
	}
	if !strings.Contains(out.String(), "forget") || !strings.Contains(out.String(), "re-point") {
		t.Errorf("help missing subcommands:\n%s", out.String())
	}
}
