package dun

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// dun's own tools are rows in /mcp, a group each: switched off at a scope,
// a group's tools leave the definitions and a call is refused; one tool can
// be switched alone; core cannot be switched off (USER 2026-10-28).
func TestBuiltinGroups_SwitchAGroupAndATool(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), SessionFile: filepath.Join(t.TempDir(), "s.jsonl"), Client: &scriptRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	rows := map[string]ServerState{}
	for _, r := range h.BuiltinGroups() {
		rows[r.ID] = r
	}
	if r := rows["dun:files"]; !r.Running || r.Tools == 0 || r.Global != "on" || r.Proto != "dun" {
		t.Fatalf("dun:files: %+v (rows %v)", r, rows)
	}
	if _, ok := rows["dun:core"]; !ok {
		t.Fatalf("no dun:core: %v", rows)
	}
	if _, err := h.SetServerOn(context.Background(), "dun:core", ScopeSession, ptr(false)); err == nil {
		t.Error("core was switched off")
	}
	has := func(name string) bool { return slices.Contains(h.ToolNames(), name) }
	if !has("recap") || !has("file_read") {
		t.Fatalf("tools: %v", h.ToolNames())
	}
	if _, err := h.SetServerOn(context.Background(), "dun:recap", ScopeProject, ptr(false)); err != nil {
		t.Fatal(err)
	}
	if has("recap") {
		t.Errorf("recap still listed: %v", h.ToolNames())
	}
	if out := fileCall(t, h, "recap", map[string]any{"summary": "x"}); !strings.Contains(out, "switched off") {
		t.Errorf("a call to it: %s", out)
	}
	// The session's on beats the project's off.
	if _, err := h.SetServerOn(context.Background(), "dun:recap", ScopeSession, ptr(true)); err != nil {
		t.Fatal(err)
	}
	if !has("recap") {
		t.Error("session on did not win")
	}
	// One tool alone.
	if _, changed, err := h.SetToolOn("dun:files", "file_write", ScopeSession, ptr(false)); err != nil || !changed {
		t.Fatalf("tool off: %v %v", changed, err)
	}
	if has("file_write") || !has("file_read") {
		t.Errorf("after file_write off: %v", h.ToolNames())
	}
	tools, _ := h.ServerTools("dun:files")
	for _, ti := range tools {
		if ti.Name == "file_write" && (ti.Enabled || ti.Session != "off") {
			t.Errorf("file_write row: %+v", ti)
		}
	}
	for _, r := range h.BuiltinGroups() {
		if r.ID == "dun:files" && r.Hidden != 1 {
			t.Errorf("dun:files hidden %d", r.Hidden)
		}
	}
}
