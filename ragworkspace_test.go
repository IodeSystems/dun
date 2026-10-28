package dun

import (
	"context"
	"testing"
)

// /rag workspace: on by default; a scope's off holds until a nearer
// scope says on; a --no-dir session is always off and cannot be set.
func TestWorkspaceIndexed_Scopes(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), SessionFile: t.TempDir() + "/s.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	check := func(want bool, from string) {
		t.Helper()
		if on, f := h.WorkspaceIndexed(); on != want || f != from {
			t.Errorf("on=%v from=%q, want %v %q", on, f, want, from)
		}
	}
	check(true, "")
	if _, err := h.SetWorkspaceIndexed(false, ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	check(false, ScopeGlobal)
	msg, err := h.SetWorkspaceIndexed(true, ScopeSession)
	if err != nil {
		t.Fatal(err)
	}
	check(true, ScopeSession)
	if msg == "" {
		t.Error("no message")
	}

	nd, err := Start(context.Background(), Config{Workspace: t.TempDir(), NoDir: true})
	if err != nil {
		t.Fatal(err)
	}
	defer nd.Close()
	if on, from := nd.WorkspaceIndexed(); on || from != "--no-dir" {
		t.Errorf("--no-dir: %v %q", on, from)
	}
	if _, err := nd.SetWorkspaceIndexed(true, ScopeProject); err == nil {
		t.Error("--no-dir took a setting")
	}
}
