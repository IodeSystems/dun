package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/enginecmd"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/project"
	"github.com/iodesystems/dun/internal/raglit"
)

// fakeToolServer is a Streamable HTTP MCP server with one tool.
func fakeToolServer(t *testing.T, tool string) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		var result any = map[string]any{}
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": tool, "description": tool}}}
		}
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		fmt.Fprint(w, string(out))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// /rag and /poly-lsp: bare reports, on and off switch this session,
// auto and manual the project, unset clears a scope, restart bounces. Each
// line names the server and the next command.
func TestRaglitAndPolyLspCommands_StatusOnOffAutoManualUnsetRestart(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ctx := context.Background()
	ws := t.TempDir()
	h, err := harness.Start(ctx, harness.Config{Workspace: ws, Servers: []mcpserver.Server{
		{ID: mcpserver.ServerDocs, URL: fakeToolServer(t, "search").URL, Timeout: 10},
		{ID: mcpserver.ServerCode, URL: fakeToolServer(t, "node_query").URL, Timeout: 10},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, id := range []string{"raglit", "poly-lsp"} {
		slash := map[string]string{"raglit": "rag", "poly-lsp": "lsp"}[id] // the command; the server keeps its id
		if slash == "" {
			slash = id
		}
		step := func(action string, want ...string) {
			t.Helper()
			out := runServerCmd(ctx, h, id, action)
			for _, w := range want {
				if !strings.Contains(out, w) {
					t.Errorf("/%s %s: lacks %q:\n%s", id, action, w, out)
				}
			}
		}
		running := func(want bool) {
			t.Helper()
			if st, _ := stateOf(h, id); st.Running != want {
				t.Errorf("%s running = %v, want %v", id, st.Running, want)
			}
		}
		step("", id+": stopped", "switches: global off, project unset, session unset", "/"+slash+" on to start it this session")
		step("on", id)
		running(true)
		step("status", id+": running · 1 tool", "session on", "/"+slash+" off to stop it this session")
		step("off", id)
		running(false)
		step("auto", id)
		step("", "project on")
		step("unset --session", id)
		step("", "session unset")
		running(true)
		step("restart", id+": restarted")
		running(true)
		step("manual", id)
		step("", "project off")
		running(false)
		step("unset --project", id)
		step("bounce", "unknown action", "/"+id+" [on|off|unset|restart|auto|manual]")
	}
	// Bare /rag lists its commands with their help.
	if out := runServerCmd(ctx, h, "raglit", ""); !strings.Contains(out, "commands:") || !strings.Contains(out, "/rag include ") ||
		!strings.Contains(out, "/rag excludes ") || !strings.Contains(out, "/rag watch ") {
		t.Errorf("bare /rag lists its commands:\n%s", out)
	}
	// poly-lsp's are its index's excludes (indexcmd.go).
	if out := runServerCmd(ctx, h, "poly-lsp", ""); !strings.Contains(out, "/lsp exclude ") || !strings.Contains(out, "/lsp include ") {
		t.Errorf("bare /lsp lists exclude and include:\n%s", out)
	}
	l := enginecmd.CommandList(h.Servers(), "")
	names := map[string]bool{}
	for _, c := range l.Items {
		names[c.Name] = true
	}
	if !names["rag"] || !names["lsp"] {
		t.Errorf("both are engine commands: %v", names)
	}
}

// raglitSavingsLine is the part of the /rag report a maintainer cares about:
// the per-layer document counts and the daemon's fold verdict, as the CLI
// prints them. The harness test proves RaglitSavings returns the counts from
// the daemon; this proves the CLI formats and SHOWS them, on the registered
// (positive) path, so a regression in the loop, a number, or the id !=
// ServerDocs guard fails here, not in production.
func TestRagStatus_ShowsSavingsAndRecommend(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ctx := context.Background()
	ws := t.TempDir()

	// A fake raglit daemon: the only call this path makes is GET
	// /api/projects/savings, which the client resolves via $RAGLIT_DAEMON.
	sav := raglit.Savings{
		ProjectID:           "abc123",
		DuplicateDocuments:  5,
		DuplicateBytes:      1024,
		DuplicateEmbeddings: 12,
		FanoutNow:           3,
		FanoutWithBase:      1,
		Recommend:           true,
		Reason:              "enough overlap",
		Layers: []raglit.LayerSavings{
			{Name: "private", Documents: 10, Duplicates: 3},
			{Name: "public", Documents: 7, Duplicates: 2},
		},
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/savings" {
			t.Errorf("unexpected daemon path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sav)
	}))
	t.Cleanup(daemon.Close)
	t.Setenv("RAGLIT_DAEMON", daemon.URL)

	// Persist the registration so RaglitProjectState reads a registered
	// project: the raglit state file in the project's state dir.
	p, err := project.Ensure(ws)
	if err != nil {
		t.Fatal(err)
	}
	state := struct {
		Namespace string `json:"namespace"`
		ProjectID string `json:"project_id"`
		Base      string `json:"base"`
		Private   string `json:"private"`
		Public    string `json:"public"`
	}{Namespace: "ns-1234", ProjectID: "abc123", Base: "ns-1234__base", Private: "p", Public: "q"}
	b, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(p.StateDir, "raglit.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := harness.Start(ctx, harness.Config{Workspace: ws, Servers: []mcpserver.Server{
		{ID: mcpserver.ServerDocs, URL: fakeToolServer(t, "search").URL, Timeout: 10},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	out := serverStatusLine(ctx, h, mcpserver.ServerDocs, "rag")
	// The aggregate line: what a maintained base would save.
	for _, want := range []string{
		"project abc123: a maintained base would save 5 duplicate documents (1 KB), 12 duplicate embeddings and 2 of 3 search fan-out",
		"raglit recommends folding",
		"(enough overlap)",
		// The per-layer loop: a number per source, each on its own line.
		"  private: 10 docs (3 duplicates)",
		"  public: 7 docs (2 duplicates)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/rag status lacks %q:\n%s", want, out)
		}
	}
}
