package dun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	if _, err := h.AddPath(agents, ScopeSession, PathOptions{Write: true}); err != nil {
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
	if _, err := h2.AddPath(other, ScopeSession, PathOptions{Write: true, NoIndex: true}); err != nil {
		t.Fatal(err)
	}
	if out := fileCall(t, h2, "file_read", map[string]any{"path": filepath.Join(other, "secret.txt")}); !strings.Contains(out, "no") || strings.Contains(out, "ERROR") {
		t.Errorf("directory: %s", out)
	}
	if _, err := h2.RemovePath(other, ScopeSession); err != nil {
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
	if _, err := h.AddPath(link, ScopeSession, PathOptions{Write: true, NoIndex: true}); err != nil {
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

// /file at scopes (USER 2026-10-28): added globally, a path opens read only;
// the session can turn it off, and unsetting the session's hands it back;
// write mode lets the edit through.
func TestFilePaths_ScopesModeAndPrecedence(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no raglit: a directory is opened, not indexed
	old := tmpRoots
	tmpRoots = func() []string { return nil }
	t.Cleanup(func() { tmpRoots = old })
	ws, other := t.TempDir(), t.TempDir()
	f := filepath.Join(other, "notes.txt")
	os.WriteFile(f, []byte("first\n"), 0o644)
	h, err := Start(context.Background(), Config{Workspace: ws, SessionFile: filepath.Join(t.TempDir(), "s.jsonl"), Client: &scriptRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	msg, err := h.AddPath(other, ScopeGlobal, PathOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "read only") || !strings.Contains(msg, "not indexed") {
		t.Errorf("add: %s", msg)
	}
	if out := fileCall(t, h, "file_read", map[string]any{"path": f}); !strings.Contains(out, "first") {
		t.Errorf("read: %s", out)
	}
	edit := map[string]any{"path": f, "edits": []map[string]any{{"old": "first", "new": "second"}}}
	if out := fileCall(t, h, "file_edit", edit); !strings.Contains(out, "for reading only") || !strings.Contains(out, "/file mode") {
		t.Errorf("a read-only edit: %s", out)
	}
	if _, err := h.SetPathOn(other, ScopeSession, false); err != nil {
		t.Fatal(err)
	}
	if out := fileCall(t, h, "file_read", map[string]any{"path": f}); !strings.Contains(out, "outside this workspace") {
		t.Errorf("off at the session: %s", out)
	}
	if _, err := h.RemovePath(other, ScopeSession); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetPathMode(other, ScopeProject, "write"); err != nil {
		t.Fatal(err)
	}
	if out := fileCall(t, h, "file_edit", edit); strings.Contains(out, "ERROR") {
		t.Errorf("write mode at the project: %s", out)
	}
	if b, _ := os.ReadFile(f); string(b) != "second\n" {
		t.Errorf("not edited: %q", b)
	}
	es := h.AddedEntries()
	if len(es) != 1 || !es[0].On || !es[0].Write || es[0].From != ScopeGlobal || es[0].Scopes[ScopeGlobal] != "on" {
		t.Errorf("entries: %+v", es)
	}
	if v, ok := h.SwitchView(pathsKind); !ok || len(v.Items) != 2 || !strings.Contains(v.Items[1].Source, "read and write") { // [0]: the workspace
		t.Errorf("table: %+v", v.Items)
	}
}

// A session's grants from before scopes (<session>.files.json, a list, all
// read and write) are read into its paths.json once.
func TestFilePaths_OldSessionListMigrates(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	other := t.TempDir()
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	old := strings.TrimSuffix(sf, ".jsonl") + ".files.json"
	b, _ := json.Marshal([]string{other})
	os.WriteFile(old, b, 0o600)
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), SessionFile: sf, Client: &scriptRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	es := h.AddedEntries()
	if len(es) != 1 || es[0].Path != other || !es[0].On || !es[0].Write || es[0].Index {
		t.Fatalf("migrated: %+v", es)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("the old list should be gone once read")
	}
}

// An added directory is indexed by default: raglit watches it into an index
// of its own, which /rag then has on at the same scope; index off switches
// it off there and keeps it.
func TestFilePaths_IndexedDirectoryIsOnInRag(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	bin := t.TempDir()
	argv := filepath.Join(bin, "argv")
	script := "#!/bin/sh\necho \"$@\" >> " + argv + "\necho watching\n"
	if err := os.WriteFile(filepath.Join(bin, "raglit"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), SessionFile: filepath.Join(t.TempDir(), "s.jsonl"),
		Servers: []Server{{ID: ServerDocs, Command: filepath.Join(bin, "raglit"), Args: []string{"serve", "--project", "p", "--index", "w"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	msg, err := h.AddPath(other, ScopeProject, PathOptions{})
	if err != nil {
		t.Fatal(err)
	}
	name := "p:" + pathIndexName(other)
	if !strings.Contains(msg, "indexed as "+name) || !strings.Contains(msg, "on in /rag for this project") {
		t.Fatalf("add: %s", msg)
	}
	if b, _ := os.ReadFile(argv); !strings.Contains(string(b), "watch "+other+" --index "+pathIndexName(other)) {
		t.Errorf("raglit was not asked to watch it: %q", b)
	}
	s, _ := h.knownServer(ServerDocs)
	if !slices.Contains(s.Attach, name) {
		t.Errorf("not on in /rag: %v", s.Attach)
	}
	if _, err := h.SetPathIndex(other, ScopeProject, false); err != nil {
		t.Fatal(err)
	}
	s, _ = h.knownServer(ServerDocs)
	if slices.Contains(s.Attach, name) || !slices.Contains(s.Detach, name) {
		t.Errorf("index off should switch it off in /rag: on %v off %v", s.Attach, s.Detach)
	}
}

// The /file table's first row is the workspace, always open; its mode is
// switchable per scope, and a read-only workspace refuses edits (USER
// 2026-10-28).
func TestFilePaths_WorkspaceRow(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "a.txt"), []byte("one\n"), 0o644)
	h, err := Start(context.Background(), Config{Workspace: ws, SessionFile: filepath.Join(t.TempDir(), "s.jsonl"), Client: &scriptRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	v := h.pathsView()
	if len(v.Items) == 0 || v.Items[0].Name != filepath.Clean(ws) || !v.Items[0].On || !strings.HasPrefix(v.Items[0].Source, "read and write · this workspace") {
		t.Fatalf("first row: %+v", v.Items)
	}
	if _, err := h.setPathSwitch(ws, ScopeSession, new(bool)); err == nil {
		t.Error("the workspace was switched off")
	}
	if _, err := h.SetPathMode(".", ScopeProject, "read"); err != nil {
		t.Fatal(err)
	}
	if v := h.pathsView(); !strings.HasPrefix(v.Items[0].Source, "read · this workspace") || v.Items[0].From != ScopeProject {
		t.Errorf("after read at project: %+v", v.Items[0])
	}
	if out := fileCall(t, h, "file_edit", map[string]any{"path": "a.txt", "edits": []map[string]any{{"old": "one", "new": "two"}}}); !strings.Contains(out, "ERROR") {
		t.Errorf("a read-only workspace took an edit: %s", out)
	}
	if _, err := h.SetPathMode(ws, ScopeSession, "write"); err != nil { // the nearer scope wins
		t.Fatal(err)
	}
	if out := fileCall(t, h, "file_edit", map[string]any{"path": "a.txt", "edits": []map[string]any{{"old": "one", "new": "two"}}}); strings.Contains(out, "ERROR") {
		t.Errorf("write at session: %s", out)
	}
}

// A path added while a turn runs is open to that turn's next call: the
// model asks, the person adds, the model tries again in the same turn
// (USER 2026-11-06: "adding a file after it has tried to write doesn't seem
// to work"). The tool set waits for the turn's end; the grant does not, and
// the same call a third time is not a loop: the person changed its answer.
func TestFileAdd_DuringATurn(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	old := tmpRoots
	tmpRoots = func() []string { return nil }
	t.Cleanup(func() { tmpRoots = old })
	ws, other := t.TempDir(), t.TempDir()
	notes := filepath.Join(other, "notes.md")
	os.WriteFile(notes, []byte("old\n"), 0o644)
	h, err := Start(context.Background(), Config{Workspace: ws, SessionFile: filepath.Join(t.TempDir(), "s.jsonl"), Client: &scriptRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.turnMu.Lock() // a turn is running
	defer h.turnMu.Unlock()
	if out := fileCall(t, h, "file_write", map[string]any{"path": notes, "content": "new\n", "overwrite": true}); !strings.Contains(out, "/file add "+notes+" --write") {
		t.Fatalf("outside: want the command with --write: %s", out)
	}
	if _, err := h.AddPath(notes, ScopeSession, PathOptions{}); err != nil {
		t.Fatal(err)
	}
	if out := fileCall(t, h, "file_write", map[string]any{"path": notes, "content": "new\n", "overwrite": true}); !strings.Contains(out, "reading only") {
		t.Errorf("added read only, in the turn: want the read-only refusal: %s", out)
	}
	if _, err := h.SetPathMode(notes, ScopeSession, "write"); err != nil {
		t.Fatal(err)
	}
	if out := fileCall(t, h, "file_write", map[string]any{"path": notes, "content": "new\n", "overwrite": true}); strings.Contains(out, "ERROR") {
		t.Errorf("made writable, in the turn: %s", out)
	}
	if b, _ := os.ReadFile(notes); string(b) != "new\n" {
		t.Errorf("not written: %q", b)
	}
}

// A file the model means to create can be added before it exists, when its
// directory does: that file alone opens, and the write makes it.
func TestFileAdd_ANewFile(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	old := tmpRoots
	tmpRoots = func() []string { return nil }
	t.Cleanup(func() { tmpRoots = old })
	ws, other := t.TempDir(), t.TempDir()
	fresh := filepath.Join(other, "plan.md")
	h, err := Start(context.Background(), Config{Workspace: ws, SessionFile: filepath.Join(t.TempDir(), "s.jsonl"), Client: &scriptRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	out := fileCall(t, h, "file_write", map[string]any{"path": fresh, "content": "x\n"})
	if !strings.Contains(out, "/file add "+fresh+" --write") || !strings.Contains(out, "/file add "+other+" --write") {
		t.Fatalf("outside, not there yet: want the file and its directory: %s", out)
	}
	if _, err := h.AddPath(fresh, ScopeSession, PathOptions{Write: true}); err != nil {
		t.Fatalf("a file not there yet, in a directory that is: %v", err)
	}
	if out := fileCall(t, h, "file_write", map[string]any{"path": fresh, "content": "x\n"}); strings.Contains(out, "ERROR") {
		t.Errorf("write: %s", out)
	}
	if out := fileCall(t, h, "file_write", map[string]any{"path": filepath.Join(other, "else.md"), "content": "x\n"}); !strings.Contains(out, "only the files added") {
		t.Errorf("its neighbour: %s", out)
	}
	if _, err := h.AddPath(filepath.Join(t.TempDir(), "no", "such", "dir.md"), ScopeSession, PathOptions{}); err == nil {
		t.Error("a file in a directory that is not there: want an error")
	}
}

// After a /file change the model is told every open path and its mode, so it
// can tell read from write, not just what changed.
func TestFilesAside_ListsEveryPathWithItsMode(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws, ro, rw := t.TempDir(), t.TempDir(), t.TempDir()
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	h, err := Start(context.Background(), Config{Workspace: ws, SessionFile: sf, Servers: []Server{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.AddPath(ro, ScopeSession, PathOptions{NoIndex: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddPath(rw, ScopeSession, PathOptions{Write: true, NoIndex: true}); err != nil {
		t.Fatal(err)
	}
	a := h.FilesAside()
	for _, want := range []string{ws + " (read and write)", ro + " (read only)", rw + " (read and write)", "Nothing else"} {
		if !strings.Contains(a, want) {
			t.Errorf("aside lacks %q:\n%s", want, a)
		}
	}
}

// The system prompt names no open path and no mode (USER 2026-11-07): the
// file tools refuse what is not open, and filesystem isolation is what holds
// exec to it. Adding a path changes nothing in it.
func TestSystemPrompt_NamesNoOpenPaths(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws, ro := t.TempDir(), t.TempDir()
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	h, err := Start(context.Background(), Config{Workspace: ws, SessionFile: sf, Servers: []Server{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	before := h.Session.System
	for _, no := range []string{"(read and write)", "(read only)", "also open"} {
		if strings.Contains(before, no) {
			t.Errorf("the system prompt names modes (%q):\n%s", no, before)
		}
	}
	if _, err := h.AddPath(ro, ScopeSession, PathOptions{NoIndex: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.Session.System, ro) {
		t.Errorf("the added path is in the system prompt:\n%s", h.Session.System)
	}
}
