package harness

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/dun/internal/mcpserver"
)

func TestClientRootsVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.15.0": true,
		"v0.15.1": true,
		"v1.0.0":  true,
		"v0.15.1-0.20261119000000-abcdef123456+dirty": true, // a checkout after v0.15.0
		"v0.14.1":                            false,
		"v0.15.0-rc.1":                       false,
		"v0.0.0-20261119000000-abcdef123456": false, // unstamped
		"(devel)":                            false,
		"":                                   false,
	} {
		if got := clientRootsVersion(v); got != want {
			t.Errorf("clientRootsVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestWithClientRoots(t *testing.T) {
	old := clientRootsBinary
	t.Cleanup(func() { clientRootsBinary = old })
	shell := mcpserver.Server{ID: mcpserver.ServerShell, Command: "mcpshell"}
	base := []string{"mcp", "--files-dir", "/ws"}

	clientRootsBinary = func(string) bool { return true }
	got, ok := withClientRoots(shell, "/bin/mcpshell", base)
	if !ok || !slices.Equal(got, append(slices.Clone(base), clientRootsFlag)) {
		t.Fatalf("new shell: %v %v", got, ok)
	}
	if got, ok := withClientRoots(shell, "/bin/mcpshell", got); !ok || len(got) != len(base)+1 {
		t.Errorf("flag given twice: %v %v", got, ok)
	}
	if got, ok := withClientRoots(shell, "/bin/mcpshell", []string{"mcp"}); ok || len(got) != 1 {
		t.Errorf("no --files-dir: %v %v", got, ok)
	}
	if _, ok := withClientRoots(mcpserver.Server{ID: mcpserver.ServerShell, Command: "othershell"}, "/bin/othershell", base); ok {
		t.Error("a shell that is not mcpshell got the flag")
	}

	clientRootsBinary = func(string) bool { return false }
	if got, ok := withClientRoots(shell, "/bin/mcpshell", base); ok || !slices.Equal(got, base) {
		t.Errorf("old shell: %v %v", got, ok)
	}
}

func TestBuildShellRoots(t *testing.T) {
	grants := []AddedPath{
		{Path: "/a/notes", On: true, Write: true, Dir: true},
		{Path: "/b/ref", On: true, Dir: true},
		{Path: "/c/off", On: false, Write: true, Dir: true},
		{Path: "/d/one file.txt", On: true},
	}
	got := buildShellRoots("/ws/", grants, []string{"/tmp/dun-1", "/b/ref", "/ws"})
	want := []shellRoot{
		{URI: "file:///a/notes", Name: "notes", Meta: map[string]any{"mcpshell/access": "write"}},
		{URI: "file:///b/ref", Name: "ref"},
		{URI: "file:///d/one%20file.txt", Name: "one file.txt"},
		{URI: "file:///tmp/dun-1", Name: "dun-1"},
	}
	if rootsKey(got) != rootsKey(want) {
		t.Fatalf("roots\n got %s\nwant %s", rootsKey(got), rootsKey(want))
	}
	if w := buildShellRoots("/ws", nil, nil); len(w) != 0 {
		t.Errorf("the workspace is --files-dir's root, not a client root: %+v", w)
	}
}

// The harness answers the shell's roots/list from its grants, and nothing
// else.
func TestAnswerServerRequestRoots(t *testing.T) {
	h, ws, outside := accessHarness(t, nil)
	dir := filepath.Dir(outside)
	if _, err := h.AddPath(dir, mcpserver.ScopeSession, PathOptions{Write: true, NoIndex: true}); err != nil {
		t.Fatal(err)
	}
	res, err := h.answerServerRequest(context.Background(), mcpserver.ServerShell, rootsListMethod, nil)
	if err != nil {
		t.Fatal(err)
	}
	roots := res.(map[string]any)["roots"].([]shellRoot)
	realDir, _ := filepath.EvalSymlinks(dir)
	var sawWS, sawDir bool
	for _, r := range roots {
		switch r.URI {
		case fileURI(filepath.Clean(ws)):
			sawWS = true // --files-dir's root already
		case fileURI(realDir):
			sawDir = r.Meta[rootsAccessMeta] == "write"
		}
	}
	if sawWS || !sawDir {
		t.Errorf("roots %s: want %s with write, and not the workspace", rootsKey(roots), realDir)
	}
	if _, err := h.answerServerRequest(context.Background(), mcpserver.ServerShell, "sampling/createMessage", nil); !errors.Is(err, mcpmgr.ErrMethodNotFound) {
		t.Errorf("other method: %v", err)
	}
	if _, err := h.answerServerRequest(context.Background(), "other", rootsListMethod, nil); !errors.Is(err, mcpmgr.ErrMethodNotFound) {
		t.Errorf("other server: %v", err)
	}
}
