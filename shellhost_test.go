package dun

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// Via survives the user file and the layered load; "direct" reads as the
// default; anything else, and a built-in in the shell, is refused by name.
func TestServerSpec_ViaRoundTrip(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if _, err := PutUserServer(ServerSpec{ID: "corp", Command: "corp-mcp", Via: ViaShell}); err != nil {
		t.Fatal(err)
	}
	if _, err := PutUserServer(ServerSpec{ID: "db", Command: "db-mcp", Via: ViaDirect}); err != nil {
		t.Fatal(err)
	}
	f, _ := readServersFile(UserServersFile())
	if f.Servers[0].Via != ViaShell {
		t.Errorf("file: %+v", f.Servers)
	}
	dir := t.TempDir()
	got, err := LoadServers(dir, "/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if byID(got)["corp"].Via != ViaShell || byID(got)["db"].Via != "" {
		t.Errorf("corp %+v db %+v", byID(got)["corp"], byID(got)["db"])
	}
	// A project file can move it back with an explicit "direct".
	writeJSON(t, dir, ProjectServersFile, `{"servers":[{"id":"corp","via":"direct"}]}`)
	if got, _ := LoadServers(dir, "/ws", ""); byID(got)["corp"].Via != "" {
		t.Errorf("direct over shell: %+v", byID(got)["corp"])
	}
	writeJSON(t, dir, ProjectServersFile, `{"servers":[{"id":"corp","via":"sideways"}]}`)
	if _, err := LoadServers(dir, "/ws", ""); err == nil || !strings.Contains(err.Error(), "sideways") {
		t.Errorf("bad via: %v", err)
	}
	writeJSON(t, dir, ProjectServersFile, `{"servers":[{"id":"code","via":"shell"}]}`)
	if _, err := LoadServers(dir, "/ws", ""); err == nil || !strings.Contains(err.Error(), "built in") {
		t.Errorf("a built-in in the shell: %v", err)
	}
}

// The generated file is mcpshell's {"mcpServers": …}: a stdio server keeps
// its command, args and env; a URL server's $NAME bearer becomes auth_cmd
// (the token is never written), a literal one an Authorization header.
func TestMcpshellConfig_StdioAndURL(t *testing.T) {
	t.Setenv("CORP_TOKEN", "s3cret")
	b, err := mcpshellConfig([]Server{
		{ID: "corp-docs", Command: "corp-mcp", Args: []string{"--stdio"}, Env: []string{"DSN=x=y"}},
		{ID: "el", URL: "https://el/mcp", Bearer: "$CORP_TOKEN", Headers: map[string]string{"Authorization": "Bearer s3cret", "X-A": "1"}},
		{ID: "lit", URL: "https://lit/mcp", Bearer: "abc", Headers: map[string]string{"Authorization": "Bearer abc"}},
		{ID: "unset", URL: "https://u/mcp", Bearer: "${NOPE_NOT_SET}"},
		{ID: "9x", Command: "n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "s3cret") {
		t.Errorf("an env-referenced token was written out:\n%s", b)
	}
	var root struct {
		McpServers map[string]mcpshellServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	s := root.McpServers
	if c := s["corp_docs"]; c.Command != "corp-mcp" || !slices.Equal(c.Args, []string{"--stdio"}) || c.Env["DSN"] != "x=y" {
		t.Errorf("stdio: %+v", c)
	}
	if e := s["el"]; e.URL != "https://el/mcp" || e.AuthCmd != `printf '%s' "$CORP_TOKEN"` || e.Headers["Authorization"] != "" || e.Headers["X-A"] != "1" {
		t.Errorf("env bearer: %+v", e)
	}
	if l := s["lit"]; l.AuthCmd != "" || l.Headers["Authorization"] != "Bearer abc" {
		t.Errorf("literal bearer: %+v", l)
	}
	if u := s["unset"]; u.AuthCmd != "" || u.Headers != nil {
		t.Errorf("an unset variable sends no Authorization, as direct does: %+v", u)
	}
	if _, ok := s["_9x"]; !ok {
		t.Errorf("namespaces must be identifiers: %v", s)
	}
	if _, err := mcpshellConfig([]Server{{ID: "a-b", Command: "x"}, {ID: "a_b", Command: "y"}}); err == nil {
		t.Error("two servers on one namespace must be refused")
	}
}

func newServerHarness(t *testing.T, specs ...Server) *Harness {
	h := newNoteHarness(t)
	h.mgr, h.lastErr, h.lastStart = mcpmgr.NewManager(), map[string]string{}, map[string]time.Duration{}
	t.Cleanup(h.mgr.Close)
	h.Session = &agent.Session{}
	h.specs = specs
	h.cfg.Servers = append([]Server{}, specs...) // the base the files layer over; not the built-ins
	h.cfg.Workspace = t.TempDir()
	return h
}

// --mcp is on the shell's argv only while something is placed in it, and the
// file it names is 0600 under $DUN_HOME, never in the workspace.
func TestShellArgs_McpOnlyWhenHosted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	shell := Server{ID: ServerShell, Command: "mcpshell", Args: []string{"mcp", "--files-dir", "/ws"}}
	h := newServerHarness(t, shell, Server{ID: "corp", Command: "corp-mcp", Autostart: true})
	if got := h.shellArgs(shell); !slices.Equal(got, shell.Args) {
		t.Errorf("nothing hosted: %v", got)
	}
	h.specs[1].Via = ViaShell
	got := h.shellArgs(shell)
	if len(got) != 5 || got[3] != "--mcp" || !strings.HasPrefix(got[4], home) {
		t.Fatalf("hosted: %v", got)
	}
	st, err := os.Stat(got[4])
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file %v %v", st, err)
	}
	if !slices.Equal(shell.Args, []string{"mcp", "--files-dir", "/ws"}) {
		t.Error("the spec's args were modified")
	}
	// Moved back: no --mcp, and the file is gone.
	h.specs[1].Via = ""
	if args := h.shellArgs(shell); slices.Contains(args, "--mcp") {
		t.Errorf("unhosted: %v", args)
	}
	if _, err := os.Stat(got[4]); !os.IsNotExist(err) {
		t.Error("the stale file must go")
	}
	// A shell that is not mcpshell cannot host.
	h.specs[1].Via = ViaShell
	other := Server{ID: ServerShell, Command: "/opt/othershell", Args: []string{"x"}}
	if args := h.shellArgs(other); !slices.Equal(args, other.Args) {
		t.Errorf("not mcpshell: %v", args)
	}
}

// A shell-placed server is never started by dun, at add or on request; /mcp
// move takes it out and back; a built-in cannot be moved; /mcp refresh on it
// says why there is nothing to index.
func TestMoveServer_BothWays(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCP(t, "tok")
	defer ts.Close()
	h := newServerHarness(t)
	ctx := context.Background()

	if _, err := h.AddServer(ctx, ServerSpec{ID: "corp", URL: ts.URL, Bearer: "tok", Via: ViaShell}); err != nil {
		t.Fatal(err)
	}
	if h.mgr.ServerStarted("corp") {
		t.Fatal("a shell-placed server must not be started directly")
	}
	if err := h.StartServer(ctx, "corp"); err == nil || !strings.Contains(err.Error(), "is scripted") {
		t.Errorf("start on request: %v", err)
	}
	if _, err := h.SyncMCPDocs(ctx, "corp"); err == nil || !strings.Contains(err.Error(), "is scripted") {
		t.Errorf("refresh: %v", err)
	}
	st := h.Servers()[0]
	if st.Via != ViaShell || st.Namespace != "corp" || st.Running {
		t.Errorf("state %+v", st)
	}

	if _, err := h.MoveServer(ctx, "corp", ScopeGlobal, ViaDirect); err != nil {
		t.Fatal(err)
	}
	if !h.mgr.ServerStarted("corp") || !slices.Contains(h.ToolNames(), "corp__search") {
		t.Fatalf("direct: started %v tools %v", h.mgr.ServerStarted("corp"), h.ToolNames())
	}
	if f, _ := readServersFile(UserServersFile()); f.Servers[0].Via != "" {
		t.Errorf("saved %+v", f.Servers[0])
	}

	if _, err := h.MoveServer(ctx, "corp", ScopeGlobal, ViaShell); err != nil {
		t.Fatal(err)
	}
	if h.mgr.ServerStarted("corp") || slices.Contains(h.ToolNames(), "corp__search") {
		t.Errorf("shell: started %v tools %v", h.mgr.ServerStarted("corp"), h.ToolNames())
	}
	if f, _ := readServersFile(UserServersFile()); f.Servers[0].Via != ViaShell {
		t.Errorf("saved %+v", f.Servers[0])
	}

	h.cfg.Servers = append(h.cfg.Servers, Server{ID: ServerCode, Command: "poly-lsp-mcp"})
	_ = h.reloadServers(ctx, true, nil)
	if _, err := h.MoveServer(ctx, ServerCode, ScopeGlobal, ViaShell); err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Errorf("built-in: %v", err)
	}
	if _, err := h.MoveServer(ctx, "corp", ScopeGlobal, "sideways"); err == nil {
		t.Error("a bad placement must be refused")
	}
}

// mcpshell's stderr says per upstream whether it connected; the lines are
// mcpshell's own (mcp/toolkit.go InstallClients).
func TestParseHosted(t *testing.T) {
	tail := `mcpshell mcp: file toolkit enabled (read-write: /ws)
mcpshell mcp: upstream "corp_docs" connected (corp) — 7 tool(s)
mcpshell mcp: failed to connect upstream "el": dial: refused
mcpshell mcp: skipping upstream "me" — it identifies as mcpshell; wiring it in would let eval recurse into itself
mcpshell mcp: serving on stdio`
	got := parseHosted(tail, []Server{{ID: "corp-docs"}, {ID: "el"}, {ID: "me"}, {ID: "quiet"}})
	if c := got["corp-docs"]; !c.Connected || c.Tools != 7 {
		t.Errorf("connected: %+v", c)
	}
	if e := got["el"]; e.Connected || e.Err != "dial: refused" {
		t.Errorf("failed: %+v", e)
	}
	if m := got["me"]; m.Connected || !strings.Contains(m.Err, "identifies as mcpshell") {
		t.Errorf("skipped: %+v", m)
	}
	if _, ok := got["quiet"]; ok {
		t.Error("no line is no state")
	}
}

// The model learns a hosted namespace from the system prompt — mcpshell's
// eval description names none — and a move reads as a move.
func TestShellHosted_ModelView(t *testing.T) {
	sys := systemFor([]mcpmgr.MCPTool{{Name: "eval", ServerID: ServerShell}}, nil, nil, "", "corp_docs")
	if !strings.Contains(sys, "corp_docs") || !strings.Contains(sys, `help("corp_docs")`) {
		t.Errorf("system:\n%s", sys)
	}
	if strings.Contains(systemFor([]mcpmgr.MCPTool{{Name: "eval", ServerID: ServerShell}}, nil, nil, ""), "hosted") {
		t.Error("no hosted line without hosted servers")
	}
	before := []mcpmgr.MCPTool{{Name: "search", ServerID: "corp-docs"}}
	note := toolSetDelta(before, nil, map[string]string{"corp-docs": "corp_docs"})
	if !strings.Contains(note, "moved into the shell") || strings.Contains(note, "turned off") {
		t.Errorf("delta: %q", note)
	}
}

// mcpshell always gets a file toolkit rooted at the session's tree: added
// when a configured shell's args left it out, never doubled, and an explicit
// one is kept.
func TestWithFilesDir(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{[]string{"mcp", "--web"}, []string{"mcp", "--web", "--files-dir", "/ws"}},
		{[]string{"mcp", "--files-dir", "/elsewhere"}, []string{"mcp", "--files-dir", "/elsewhere"}},
		{[]string{"mcp", "--files-dir=/elsewhere"}, []string{"mcp", "--files-dir=/elsewhere"}},
		{[]string{"mcp", "-files-dir", "/x"}, []string{"mcp", "-files-dir", "/x"}},
	} {
		if got := withFilesDir(c.in, "/ws"); !slices.Equal(got, c.want) {
			t.Errorf("withFilesDir(%v) = %v", c.in, got)
		}
	}
	if got := withFilesDir([]string{"mcp"}, ""); !slices.Equal(got, []string{"mcp"}) {
		t.Errorf("no root: %v", got)
	}
}

// mcpshell saves over-limit eval output to a per-session dir: beside the
// session file, keyed by its id, so a restart or resume finds the same files.
func TestShellEnv_OverflowDir(t *testing.T) {
	shell := Server{ID: ServerShell, Command: "/usr/local/bin/mcpshell", Env: []string{"A=1"}}
	h := newServerHarness(t, shell)
	h.cfg.SessionFile = "/home/u/.dun/sessions/repo/20261023-101500.jsonl"
	want := "MCPSHELL_TMP_OVERFLOW_DIR=/home/u/.dun/sessions/repo/overflow/20261023-101500"
	if got := h.shellEnv(shell, slices.Clone(shell.Env)); !slices.Equal(got, []string{"A=1", want}) {
		t.Errorf("shellEnv = %v", got)
	}
	// The config's own setting wins.
	own := []string{"MCPSHELL_TMP_OVERFLOW_DIR=/mine"}
	if got := h.shellEnv(shell, own); !slices.Equal(got, own) {
		t.Errorf("configured dir overridden: %v", got)
	}
	// Not mcpshell: untouched.
	other := Server{ID: ServerShell, Command: "/opt/othershell"}
	if got := h.shellEnv(other, nil); len(got) != 0 {
		t.Errorf("not mcpshell: %v", got)
	}
	// No session file: a temp dir per process.
	h.cfg.SessionFile = ""
	if got := h.shellEnv(shell, nil); len(got) != 1 || !strings.Contains(got[0], "dun-overflow") {
		t.Errorf("in-memory session: %v", got)
	}
}
