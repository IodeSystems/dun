package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
)

// accessAsker is an Ask that answers with ans and keeps what it was asked.
type accessAsker struct {
	ans  string
	asks []string
	opts [][]string
}

func (a *accessAsker) ask(_ context.Context, q string, opts []string, _ bool, _ []string) (string, error) {
	a.asks = append(a.asks, q)
	a.opts = append(a.opts, opts)
	return a.ans, nil
}

// accessHarness is a session in ws, with a file outside it, whose Ask is a.
func accessHarness(t *testing.T, a *accessAsker) (h *Harness, ws, outside string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	old := tmpRoots
	tmpRoots = func() []string { return nil } // these dirs are in /tmp, which reads may see
	t.Cleanup(func() { tmpRoots = old })
	ws, other := t.TempDir(), t.TempDir()
	outside = filepath.Join(other, "notes.txt")
	if err := os.WriteFile(outside, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Workspace: ws, SessionFile: filepath.Join(t.TempDir(), "s.jsonl"), Client: &scriptRunner{}}
	if a != nil {
		cfg.Ask = a.ask
	}
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h, ws, outside
}

func editAlpha(t *testing.T, h *Harness, path string) string {
	return fileCall(t, h, "file_edit", map[string]any{"path": path, "edits": []map[string]any{{"old": "alpha", "new": "gamma"}}})
}

func entryFor(h *Harness, p string) *AddedPath {
	for _, a := range h.AddedEntries() {
		if a.Path == p {
			return &a
		}
	}
	return nil
}

// A yes at each scope opens the file there, for writing (the call writes)
// and unindexed, and the edit that asked goes through in the same call.
func TestFileAccess_YesAtEachScopeRetries(t *testing.T) {
	for ans, scope := range map[string]string{accessSession: mcpserver.ScopeSession, accessProject: mcpserver.ScopeProject, accessGlobal: mcpserver.ScopeGlobal} {
		t.Run(scope, func(t *testing.T) {
			a := &accessAsker{ans: ans}
			h, _, outside := accessHarness(t, a)
			out := editAlpha(t, h, outside)
			if strings.Contains(out, "ERROR") {
				t.Fatalf("edit after yes: %s", out)
			}
			if len(a.asks) != 1 || !strings.Contains(a.asks[0], outside) || !strings.Contains(a.asks[0], "read and write") {
				t.Fatalf("asks: %q", a.asks)
			}
			if want := []string{accessSession, accessProject, accessGlobal, accessNo}; !slices.Equal(a.opts[0], want) {
				t.Errorf("options %q, want %q", a.opts[0], want)
			}
			if b, _ := os.ReadFile(outside); !strings.Contains(string(b), "gamma") {
				t.Errorf("not edited: %s", b)
			}
			e := entryFor(h, outside)
			if e == nil || !e.On || !e.Write || e.Scopes[scope] != "on" || e.From != scope {
				t.Fatalf("entry: %+v", e)
			}
			// Open now: the next call asks nothing.
			if out := fileCall(t, h, "file_read", map[string]any{"path": outside}); !strings.Contains(out, "gamma") || len(a.asks) != 1 {
				t.Errorf("read after: %s (%d asks)", out, len(a.asks))
			}
		})
	}
}

// A no in the person's own words is the model's error, and nothing opens.
func TestFileAccess_NoWithReason(t *testing.T) {
	a := &accessAsker{ans: "use the copy in docs/ instead"}
	h, _, outside := accessHarness(t, a)
	out := editAlpha(t, h, outside)
	if !strings.HasPrefix(out, "ERROR") || !strings.Contains(out, "They said: use the copy in docs/ instead") {
		t.Fatalf("no: %s", out)
	}
	if e := entryFor(h, outside); e != nil {
		t.Errorf("opened on a no: %+v", e)
	}
	if b, _ := os.ReadFile(outside); strings.Contains(string(b), "gamma") {
		t.Error("edited on a no")
	}
	a.ans = accessNo
	if out := editAlpha(t, h, outside); !strings.HasPrefix(out, "ERROR: the user did not open") || strings.Contains(out, "They said") {
		t.Errorf("a bare no: %s", out)
	}
}

// A path that climbs out with `..` is asked about by its real path, and a
// read opens it read only.
func TestFileAccess_RelativeEscapeAsks(t *testing.T) {
	a := &accessAsker{ans: accessSession}
	h, ws, outside := accessHarness(t, a)
	rel, err := filepath.Rel(ws, outside)
	if err != nil || !strings.HasPrefix(rel, "..") {
		t.Fatalf("rel %q: %v", rel, err)
	}
	out := fileCall(t, h, "file_read", map[string]any{"path": rel})
	if !strings.Contains(out, "alpha") {
		t.Fatalf("read after yes: %s", out)
	}
	if len(a.asks) != 1 || !strings.Contains(a.asks[0], outside) || !strings.Contains(a.asks[0], "read only") {
		t.Fatalf("asks: %q", a.asks)
	}
	if e := entryFor(h, outside); e == nil || e.Write {
		t.Errorf("entry: %+v", e)
	}
	// A symlink in the tree to outside it is asked about the same way.
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(ws, "out")); err != nil {
		t.Skip(err)
	}
	if out := editAlpha(t, h, "out/notes.txt"); strings.Contains(out, "ERROR") || len(a.asks) != 2 || !strings.Contains(a.asks[1], "reading only") {
		t.Errorf("through a link: %s, asks %q", out, a.asks)
	}
}

// Open for reading only and the call writes: the person is asked, and a yes
// sets the path's mode to write at that scope.
func TestFileAccess_ReadOnlyGrantAsksForWrite(t *testing.T) {
	a := &accessAsker{ans: accessSession}
	h, _, outside := accessHarness(t, a)
	dir := filepath.Dir(outside)
	if _, err := h.AddPath(dir, mcpserver.ScopeGlobal, PathOptions{NoIndex: true}); err != nil {
		t.Fatal(err)
	}
	if out := editAlpha(t, h, outside); strings.Contains(out, "ERROR") {
		t.Fatalf("edit after yes: %s", out)
	}
	if len(a.asks) != 1 || !strings.Contains(a.asks[0], "reading only ("+dir+")") || !strings.Contains(a.asks[0], "the directory") {
		t.Fatalf("asks: %q", a.asks)
	}
	if e := entryFor(h, dir); e == nil || !e.Write || e.From != mcpserver.ScopeGlobal {
		t.Errorf("entry: %+v", e)
	}
}

// In a round dun added the person is not asked: the refusal names the
// command and says to block the ask with it, as ask_user's does.
func TestFileAccess_AutomaticRoundRefusedWithBlockedAsk(t *testing.T) {
	a := &accessAsker{ans: accessSession}
	h, _, outside := accessHarness(t, a)
	if !h.asksOn() {
		t.Fatal("this session keeps no asks")
	}
	h.turnBegin(originAuto)
	out := editAlpha(t, h, outside)
	if len(a.asks) != 0 {
		t.Fatalf("asked in an automatic round: %q", a.asks)
	}
	for _, w := range []string{"ERROR", "/file add " + outside + " --write", askBlockHow} {
		if !strings.Contains(out, w) {
			t.Errorf("refusal lacks %q: %s", w, out)
		}
	}
	h.turnBegin(originPerson)
	if out := editAlpha(t, h, outside); strings.Contains(out, "ERROR") || len(a.asks) != 1 {
		t.Errorf("the person's turn: %s (%d asks)", out, len(a.asks))
	}
}

// A sub-agent, and a session with no Ask (--non-interactive), refuse as
// before, with the command.
func TestFileAccess_NobodyToAskRefuses(t *testing.T) {
	a := &accessAsker{ans: accessSession}
	h, _, outside := accessHarness(t, a)
	h.parent = &Harness{}
	out := editAlpha(t, h, outside)
	h.parent = nil
	if len(a.asks) != 0 || !strings.Contains(out, "Ask the user to add it: /file add "+outside+" --write") {
		t.Errorf("sub-agent: %s (%d asks)", out, len(a.asks))
	}
	h2, _, outside2 := accessHarness(t, nil)
	if out := editAlpha(t, h2, outside2); !strings.Contains(out, "Ask the user to add it: /file add "+outside2+" --write") {
		t.Errorf("no Ask: %s", out)
	}
}

// A read-only workspace keeps its edit tools, and they refuse with the
// command; several edits go in one call.
func TestFileTools_ReadOnlyRefusesAndMultiEdit(t *testing.T) {
	h, ws, _ := accessHarness(t, nil)
	p := filepath.Join(ws, "a.txt")
	os.WriteFile(p, []byte("one\ntwo\nthree\n"), 0o644)
	out := fileCall(t, h, "file_edit", map[string]any{"path": "a.txt", "edits": []map[string]any{
		{"old": "one", "new": "1"}, {"old": "three", "new": "3"}}})
	if strings.Contains(out, "ERROR") {
		t.Fatalf("multi-edit: %s", out)
	}
	if b, _ := os.ReadFile(p); string(b) != "1\ntwo\n3\n" {
		t.Errorf("after multi-edit: %q", b)
	}
	if !strings.Contains(h.Session.System, "Several changes to one file go in one file_edit call") {
		t.Error("the prompt line does not say to batch edits")
	}
	if _, err := h.SetWorkspaceMode(mcpserver.ScopeSession, "read"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fileToolNames(h), "file_edit") {
		t.Fatalf("read only took file_edit off the list: %v", fileToolNames(h))
	}
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"file_edit", map[string]any{"path": "a.txt", "edits": []map[string]any{{"old": "two", "new": "2"}}}},
		{"file_write", map[string]any{"path": "b.txt", "content": "x\n"}},
	} {
		if out := fileCall(t, h, c.name, c.args); !strings.Contains(out, "reading only") || !strings.Contains(out, "/file mode . write") {
			t.Errorf("%s in a read-only workspace: %s", c.name, out)
		}
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "2") {
		t.Error("edited a read-only workspace")
	}
}
