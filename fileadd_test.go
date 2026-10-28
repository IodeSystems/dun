package dun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

func fileCall(t *testing.T, h *Harness, name string, args map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(args)
	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = name, string(b)
	out, err := h.Session.Dispatch(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A path outside the workspace: the model is told to ask for /file add;
// added, a file is readable and editable and its neighbour is not; a
// directory opens what is under it; the grants survive a resume.
func TestFileAdd(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	old := tmpRoots
	tmpRoots = func() []string { return nil } // these dirs are in /tmp, which reads may see
	t.Cleanup(func() { tmpRoots = old })
	ws, other := t.TempDir(), t.TempDir()
	agents := filepath.Join(other, "AGENTS.md")
	os.WriteFile(agents, []byte("# rules\nkeep tests green\n"), 0o644)
	os.WriteFile(filepath.Join(other, "secret.txt"), []byte("no\n"), 0o644)
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	h, err := Start(context.Background(), Config{Workspace: ws, SessionFile: sf, Client: &scriptRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	out := fileCall(t, h, "file_read", map[string]any{"path": agents})
	if !strings.Contains(out, "Ask the user to add it: /file add "+agents) {
		t.Fatalf("outside: %s", out)
	}
	if _, err := h.AddPath(agents); err != nil {
		t.Fatal(err)
	}
	if out := fileCall(t, h, "file_read", map[string]any{"path": agents}); !strings.Contains(out, "keep tests green") {
		t.Errorf("added file: %s", out)
	}
	if out := fileCall(t, h, "file_read", map[string]any{"path": filepath.Join(other, "secret.txt")}); strings.Contains(out, "no\n") || !strings.Contains(out, "only the files added") {
		t.Errorf("its neighbour: %s", out)
	}
	if out := fileCall(t, h, "file_edit", map[string]any{"path": agents, "edits": []map[string]any{{"old": "keep tests green", "new": "keep tests fast"}}}); strings.Contains(out, "ERROR") {
		t.Errorf("edit: %s", out)
	}
	if b, _ := os.ReadFile(agents); !strings.Contains(string(b), "keep tests fast") {
		t.Errorf("not edited: %s", b)
	}
	h.Close()

	// Resumed: the grant is kept; a directory opens its files.
	h2, err := Start(context.Background(), Config{Workspace: ws, SessionFile: sf, Client: &scriptRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if ps := h2.AddedPaths(); len(ps) != 1 {
		t.Fatalf("resumed: %v", ps)
	}
	if _, err := h2.AddPath(other); err != nil {
		t.Fatal(err)
	}
	if out := fileCall(t, h2, "file_read", map[string]any{"path": filepath.Join(other, "secret.txt")}); !strings.Contains(out, "no") || strings.Contains(out, "ERROR") {
		t.Errorf("directory: %s", out)
	}
	if _, err := h2.RemovePath(other); err != nil {
		t.Fatal(err)
	}
	if out := fileCall(t, h2, "file_read", map[string]any{"path": filepath.Join(other, "secret.txt")}); !strings.Contains(out, "only the files added") {
		t.Errorf("removed: %s", out)
	}
}

// A directory added through a symlink is kept by its real path; the model
// writes the path the person typed, and everything under it — a
// subdirectory's file read, edited, a new one written — goes through
// (USER 2026-10-28: v0.1.8 refused the subdirectory's files).
func TestFileAdd_ThroughASymlink(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	old := tmpRoots
	tmpRoots = func() []string { return nil }
	t.Cleanup(func() { tmpRoots = old })
	ws, other := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(other, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(other, "a", "b", "deep.txt"), []byte("deep line\n"), 0o644)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(other, link); err != nil {
		t.Skip(err)
	}
	h, err := Start(context.Background(), Config{Workspace: ws, SessionFile: filepath.Join(t.TempDir(), "s.jsonl"), Client: &scriptRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.AddPath(link); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(link, "a", "b", "deep.txt")
	if out := fileCall(t, h, "file_read", map[string]any{"path": deep}); !strings.Contains(out, "deep line") {
		t.Errorf("read through the link: %s", out)
	}
	if out := fileCall(t, h, "file_edit", map[string]any{"path": deep, "edits": []map[string]any{{"old": "deep line", "new": "deeper"}}}); strings.Contains(out, "ERROR") {
		t.Errorf("edit through the link: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(other, "a", "b", "deep.txt")); !strings.Contains(string(b), "deeper") {
		t.Errorf("not edited: %s", b)
	}
	fresh := filepath.Join(link, "a", "new.txt")
	if out := fileCall(t, h, "file_write", map[string]any{"path": fresh, "content": "made\n"}); strings.Contains(out, "ERROR") {
		t.Errorf("write through the link: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(other, "a", "new.txt")); string(b) != "made\n" {
		t.Errorf("not written: %q", b)
	}
}
