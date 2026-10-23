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
	"sync"
	"testing"
)

// The bundled manifests parse, and are what DefaultServers runs.
func TestBundledExtensions_AreRaglitAndPolyLsp(t *testing.T) {
	var ids []string
	for _, e := range BundledExtensions() {
		ids = append(ids, e.ID+"/"+e.SlashName()+"/"+e.Source)
	}
	if strings.Join(ids, " ") != "raglit/raglit/bundled poly-lsp/poly-lsp/bundled" {
		t.Fatalf("bundled: %v", ids)
	}
	raglit, _ := bundledExt(ServerDocs)
	for _, h := range []string{HookTurnBefore, HookWorkspaceOpen, HookMemorySave, HookSkillsChanged, HookRulesChanged, HookMCPDocs} {
		if !raglit.Subscribes(h) {
			t.Errorf("raglit does not take %s", h)
		}
	}
	lsp, _ := bundledExt("code") // a pre-rename id still finds it
	if lsp.HostHooks["POLY_LSP_HOOK"] != HostAgentsGuard {
		t.Errorf("poly-lsp host hooks: %v", lsp.HostHooks)
	}
	ws := t.TempDir()
	for _, s := range DefaultServers(ws, "") {
		if strings.Contains(strings.Join(s.Args, " "), "{{") {
			t.Errorf("%s: placeholder left in %v", s.ID, s.Args)
		}
	}
}

// A manifest error starts with the field it is about.
func TestParseExtension_ErrorsNameTheField(t *testing.T) {
	for body, field := range map[string]string{
		`{"id":"Bad","server":{"command":"x"}}`:                                                             "id:",
		`{"id":"a","server":{}}`:                                                                            "server:",
		`{"id":"a","server":{"command":"x","url":"http://y"}}`:                                              "server:",
		`{"id":"a","server":{"id":"b","command":"x"}}`:                                                      "server.id:",
		`{"id":"a","server":{"command":"x","via":"sideways"}}`:                                              "server.via:",
		`{"id":"a","server":{"command":"x"},"slash":{"name":"Hi"}}`:                                         "slash.name:",
		`{"id":"a","server":{"command":"x"},"slash":{"name":"a","commands":{"on":{"tool":"t"}}}}`:           "slash.commands.on:",
		`{"id":"a","server":{"command":"x"},"slash":{"name":"a","commands":{"go":{}}}}`:                     "slash.commands.go:",
		`{"id":"a","server":{"command":"x"},"roles":{"search":{}}}`:                                         "roles.search.tool:",
		`{"id":"a","server":{"command":"x"},"roles":{"index":{}}}`:                                          "roles.index:",
		`{"id":"a","server":{"command":"x"},"roles":{"index":{"tool":"i"}}}`:                                "roles.index.forget:",
		`{"id":"a","server":{"command":"x"},"roles":{"index":{"cli":"other"}}}`:                             "roles.index.cli:",
		`{"id":"a","server":{"command":"x"},"hooks":["turn.before"]}`:                                       "hooks[0]:",
		`{"id":"a","server":{"command":"x"},"roles":{"search":{"tool":"s"}},"hooks":["turn.before","x.y"]}`: "hooks[1]:",
		`{"id":"a","server":{"command":"x"},"hooks":["memory.save"]}`:                                       "hooks[0]:",
		`{"id":"a","server":{"command":"x"},"host_hooks":{"X":"nope"}}`:                                     "host_hooks.X:",
		`{"id":"a","server":{"command":"x"},"env":{"1X":"v"}}`:                                              "env.1X:",
		`{"id":"a","server":{"command":"x"},"skills":"../up"}`:                                              "skills:",
		`{"id":"a","server":{"command":"x"},"hoks":[]}`:                                                     `unknown field "hoks"`,
	} {
		_, err := ParseExtension([]byte(body))
		if err == nil || !strings.HasPrefix(err.Error(), field) && !strings.Contains(err.Error(), field) {
			t.Errorf("%s: want %q, got %v", body, field, err)
		}
	}
}

func writeExt(t *testing.T, root, id, body string) string {
	t.Helper()
	p := filepath.Join(root, id, ExtensionFile)
	writeRule(t, p, body)
	return p
}

// User extensions come from $DUN_HOME/extensions and a project's from its
// .dun/extensions; a later source replaces an earlier one of the same id. A
// manifest that does not load is reported with its path, and a slash
// command already taken is dropped from the later one.
func TestLoadExtensions_SourcesOverridesAndErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	proj := t.TempDir()
	user := filepath.Join(home, "extensions")
	writeExt(t, user, "hello", `{"id":"hello","server":{"command":"hello-mcp"},"slash":{"name":"hello"}}`)
	writeExt(t, user, "twin", `{"id":"twin","server":{"command":"t"},"slash":{"name":"hello"}}`)
	bad := writeExt(t, user, "broken", `{"id":"broken","server":{}}`)
	misnamed := writeExt(t, user, "dir", `{"id":"other","server":{"command":"x"}}`)
	writeExt(t, filepath.Join(proj, DunDir, "extensions"), "hello", `{"id":"hello","description":"the project's","server":{"command":"hello-mcp"},"skills":"skills"}`)

	exts, errs := LoadExtensions(proj)
	by := map[string]Extension{}
	for _, e := range exts {
		by[e.ID] = e
	}
	if by["hello"].Source != ExtProject || by["hello"].Description != "the project's" {
		t.Errorf("the project's hello should replace the user's: %+v", by["hello"])
	}
	if by[ServerDocs].Source != ExtBundled {
		t.Error("the bundled ones load too")
	}
	if want := filepath.Join(proj, DunDir, "extensions", "hello", "skills"); by["hello"].skillsDir() != want {
		t.Errorf("skills dir %q", by["hello"].skillsDir())
	}
	var msgs []string
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{bad + ": server:", misnamed + `: id: "other" is not its directory's name`} {
		if !strings.Contains(all, want) {
			t.Errorf("errors lack %q:\n%s", want, all)
		}
	}
	// The project's hello has no slash, so the user's twin keeps /hello.
	if by["twin"].SlashName() != "hello" {
		t.Errorf("twin: %+v", by["twin"].Slash)
	}
	servers := extServers([]Server{{ID: "shell"}}, exts)
	var ids []string
	for _, s := range servers {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids, []string{"shell", "hello", "twin"}) {
		t.Errorf("extension servers join the base (bundled ones are already in it): %v", ids)
	}
}

// fakeIndexMCP is a server whose index tools take a batch: the MCP side of
// the index seam.
func fakeIndexMCP(t *testing.T, calls *[]string, mu *sync.Mutex) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &msg)
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		paths := map[string]any{"type": "object", "properties": map[string]any{
			"index": map[string]any{"type": "string"}, "paths": map[string]any{"type": "array"}}}
		var result any = map[string]any{}
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{
				{"name": "search", "description": "Search.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"query": map[string]any{"type": "string"}, "index": map[string]any{"type": "string"}}}},
				{"name": "get_document", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"path": map[string]any{"type": "string"}, "index": map[string]any{"type": "string"}}}},
				{"name": "ingest", "inputSchema": paths}, {"name": "forget", "inputSchema": paths}}}
		case "tools/call":
			a, _ := json.Marshal(msg.Params.Arguments)
			mu.Lock()
			*calls = append(*calls, msg.Params.Name+" "+string(a))
			mu.Unlock()
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "{}"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		fmt.Fprint(w, string(out))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// When the index tool takes `paths` and a forget tool exists, documents go
// through MCP; raglit's ingest does not (one url), so it uses the CLI.
func TestIndexSink_MCPWhenTheToolTakesPathsElseTheCLI(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	var calls []string
	var mu sync.Mutex
	ts := fakeIndexMCP(t, &calls, &mu)
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), Servers: []Server{{ID: ServerDocs, URL: ts.URL, Timeout: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	raglit, _ := h.extension(ServerDocs)
	if h.indexSink(raglit) != nil {
		t.Error("not running, and not the raglit binary: no sink")
	}
	if err := h.StartServer(context.Background(), ServerDocs); err != nil {
		t.Fatal(err)
	}
	sink := h.indexSink(raglit)
	if sink == nil {
		t.Fatal("no MCP sink")
	}
	if err := sink.ingest("skills", []string{"/a.md", "/b.md"}); err != nil {
		t.Fatal(err)
	}
	if err := sink.forget("skills", []string{"/a.md"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(calls, "; ")
	mu.Unlock()
	if !strings.Contains(got, `ingest {"index":"skills","paths":["/a.md","/b.md"]}`) || !strings.Contains(got, `forget {"index":"skills","paths":["/a.md"]}`) {
		t.Errorf("calls: %s", got)
	}

	// raglit's real ingest takes a url: the CLI, by the command's name.
	cli := Server{ID: ServerDocs, Command: filepath.Join(t.TempDir(), "raglit"), Args: []string{"serve", "--project", "p"}}
	h.srvMu.Lock()
	h.specs = []Server{cli}
	h.srvMu.Unlock()
	h.mgr.StopServer(ServerDocs)
	if h.indexSink(raglit) == nil {
		t.Error("the raglit binary is the CLI fallback")
	}
	if _, err := os.Stat(cli.Command); err == nil {
		t.Fatal("the fallback is chosen by name, not run here")
	}
}

// installHello copies the test extension (testdata/extensions/hello) to
// $DUN_HOME/extensions, as a user would.
func installHello(t *testing.T, home string) {
	t.Helper()
	for _, f := range []string{ExtensionFile, "server.py"} {
		b, err := os.ReadFile(filepath.Join("testdata", "extensions", "hello", f))
		if err != nil {
			t.Fatal(err)
		}
		writeRule(t, filepath.Join(home, "extensions", "hello", f), string(b))
	}
}

// A user extension's slash command calls its tool, and its turn.before
// search surfaces a hit as an aside, the way raglit's does.
func TestUserExtension_SlashCommandAndTurnBeforeAside(t *testing.T) {
	if !haveBinary("python3") {
		t.Skip("python3 not on PATH")
	}
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	installHello(t, home)
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), Servers: []Server{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if errs := h.ExtensionErrors(); len(errs) != 0 {
		t.Fatalf("load: %v", errs)
	}
	if _, err := h.RunExtCommand(context.Background(), "hello", "greet", "world"); err == nil || !strings.Contains(err.Error(), "/hello on") {
		t.Errorf("not running: %v", err)
	}
	if err := h.StartServer(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if out, err := h.RunExtCommand(context.Background(), "hello", "greet", " world "); err != nil || out != "hello, world" {
		t.Errorf("/hello greet world: %q %v", out, err)
	}
	if !slices.Contains(modelToolNames(h), "hello__find") {
		t.Errorf("its tools are namespaced like any added server's: %v", modelToolNames(h))
	}
	if aside := surfaceOnce(t, h, "the weather"); aside != "" {
		t.Errorf("no hit, no aside: %q", aside)
	}
	aside := surfaceOnce(t, h, "how do I deploy to zebra?")
	if !strings.Contains(aside, "Zebra deploys need the --frostbite flag") || !strings.Contains(aside, "hello://zebra-fact") {
		t.Errorf("the hit should surface as an aside: %q", aside)
	}
	if st, _ := serverState(h, "hello"); st.Slash != "hello" || st.Via != ViaDirect {
		t.Errorf("state: %+v", st)
	}
}
