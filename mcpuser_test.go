package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// TestMain keeps every test off the real $DUN_HOME and ~/.claude: LoadServers reads the
// user server list from it, and a server someone added with /mcp add must
// not change what a test sees. It keeps them off the user's shared tool
// daemons too (isolateTools).
func TestMain(m *testing.M) {
	if persona := os.Getenv(fakeBinEnv); persona != "" {
		fakeBinMain(persona) // a fake server or CLI (fakebin_test.go), not a test run
		return
	}
	home, err := os.MkdirTemp("", "dun-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("DUN_HOME", home)
	userHomeDir = func() (string, error) { return home, nil } // and ~/.claude/skills
	isolateTools(home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// The user list is the lowest file layer; a remote server's bearer becomes
// an Authorization header, read from the environment when it says $NAME.
func TestLoadServers_UserListAndBearer(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("CORP_TOKEN", "s3cret")
	if _, err := PutUserServer(ServerSpec{ID: "corp", URL: "https://corp/mcp", Bearer: "$CORP_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	if _, err := PutUserServer(ServerSpec{ID: "gone", URL: "https://x/mcp"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(UserServersFile()); st.Mode().Perm() != 0o600 {
		t.Errorf("the user list holds tokens: perms %v", st.Mode().Perm())
	}
	dir := t.TempDir()
	writeJSON(t, dir, ProjectServersFile, `{"servers":[{"id":"gone","disabled":true}]}`)
	got, err := LoadServers(dir, "/ws", "/raglit")
	if err != nil {
		t.Fatal(err)
	}
	corp, ok := byID(got)["corp"]
	if !ok || corp.URL != "https://corp/mcp" || corp.Headers["Authorization"] != "Bearer s3cret" {
		t.Fatalf("corp = %+v", corp)
	}
	if _, ok := byID(got)["gone"]; ok {
		t.Error("a project file must be able to disable a user server")
	}
	if ok, _ := RemoveUserServer("corp"); !ok {
		t.Error("remove")
	}
}

// fakeMCP is a Streamable HTTP MCP server with one tool named like raglit's.
func fakeMCP(t *testing.T, token string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &msg)
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "search", "description": "corp search"}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "corp result"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		fmt.Fprint(w, string(out))
	}))
}

func TestAddServer_RemoteToolsAreNamespacedAndRemovable(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCP(t, "tok")
	defer ts.Close()
	h := newNoteHarness(t)
	h.mgr, h.lastErr, h.lastStart = mcpmgr.NewManager(), map[string]string{}, map[string]time.Duration{}
	defer h.mgr.Close()
	h.Session = &agent.Session{}
	h.cfg.Servers = []Server{}
	ctx := context.Background()

	if _, err := h.AddServer(ctx, ServerSpec{ID: "docs", URL: ts.URL}); err == nil {
		t.Error("a built-in id must be refused")
	}
	path, err := h.AddServer(ctx, ServerSpec{ID: "corp", URL: ts.URL, Bearer: "wrong"})
	if path == "" || err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a bad token saves but does not start: %q %v", path, err)
	}
	if _, err := h.AddServer(ctx, ServerSpec{ID: "corp", URL: ts.URL, Bearer: "tok"}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, td := range h.Session.Tools {
		names = append(names, td.Function.Name)
	}
	if !slices.Contains(names, "corp__search") || slices.Contains(names, "search") {
		t.Fatalf("tools = %v; an added server's tools carry its id", names)
	}
	if len(searchExts(BundledExtensions(), h.mgr.GetTools())) != 0 {
		t.Error("an added server's search must not be taken for raglit's")
	}
	tc := llm.ToolCall{}
	tc.Function.Name, tc.Function.Arguments = "corp__search", `{"q":"x"}`
	if out, _ := h.Session.Dispatch(ctx, tc); !strings.HasPrefix(out, "corp result") || !strings.Contains(out, "available: corp__search") {
		t.Errorf("dispatch through the namespace: %q", out)
	}
	if _, err := h.RemoveServer("corp"); err != nil {
		t.Fatal(err)
	}
	names = nil
	for _, td := range h.Session.Tools {
		names = append(names, td.Function.Name)
	}
	if h.mgr.ServerStarted("corp") || slices.Contains(names, "corp__search") {
		t.Error("remove must stop it and drop its tools")
	}
	if f, _ := readServersFile(UserServersFile()); f == nil || len(f.Servers) != 0 {
		t.Errorf("remove must delete it from the user list: %+v", f)
	}
}

// /mcp remove takes the server's entries in this project's state and this
// session's sidecar with it, and leaves other servers and other projects.
func TestRemoveServer_ClearsProjectAndSessionEntries(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCP(t, "tok")
	defer ts.Close()
	repo, other := pruneRepo(t), pruneRepo(t)
	h := newNoteHarness(t)
	h.mgr, h.lastErr, h.lastStart = mcpmgr.NewManager(), map[string]string{}, map[string]time.Duration{}
	defer h.mgr.Close()
	h.Session = &agent.Session{}
	h.cfg.Servers = []Server{}
	h.cfg.Workspace = repo
	h.cfg.SessionFile = filepath.Join(t.TempDir(), "s.jsonl")
	ctx := context.Background()

	if _, err := h.AddServer(ctx, ServerSpec{ID: "corp", URL: ts.URL, Bearer: "tok"}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{repo, other} {
		if _, err := updateLocalServer(d, "corp", func(s *ServerSpec) { s.Tools = map[string]bool{"search": false} }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := setAutostart(repo, "keep", true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetServerOn(ctx, "corp", ScopeSession, boolp(true)); err != nil {
		t.Fatal(err)
	}
	cleared, err := h.RemoveServer("corp")
	if err != nil || len(cleared) != 2 {
		t.Fatalf("cleared %v %v", cleared, err)
	}
	f, _ := readServersFile(filepath.Join(ResolveProject(repo).StateDir, LocalServersFile))
	if f == nil || len(f.Servers) != 1 || f.Servers[0].ID != "keep" {
		t.Errorf("this project's file: %+v", f)
	}
	f, _ = readServersFile(filepath.Join(ResolveProject(other).StateDir, LocalServersFile))
	if f == nil || len(f.Servers) != 1 || f.Servers[0].ID != "corp" {
		t.Errorf("another project's file must stay: %+v", f)
	}
	if sw := LoadSessionServers(h.cfg.SessionFile); len(sw) != 0 {
		t.Errorf("session entries: %+v", sw)
	}
}

// isolateTools points the tools a test may start at dir instead of the user's
// shared daemons: a real poly-lsp-mcp (1be97d0 on) proxies to the per-user
// daemon under ~/.poly-lsp and $XDG_RUNTIME_DIR unless told not to, and
// raglit's config root is ~/.raglit. Without this, a test that reached a real
// binary would use — and could upgrade or restart — the user's own.
func isolateTools(dir string) {
	os.Setenv("POLY_LSP_NO_DAEMON", "1")
	os.Setenv("POLY_LSP_HOME", dir+"/poly-lsp") // the daemon's own dir, once poly-lsp reads it
	os.Setenv("XDG_RUNTIME_DIR", dir)
	os.Setenv("RAGLIT_ROOT", dir+"/raglit")
}
