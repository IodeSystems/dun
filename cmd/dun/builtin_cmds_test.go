package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iodesystems/dun"
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

// /raglit and /poly-lsp: bare reports, on and off switch this session,
// auto and manual the project, unset clears a scope, restart bounces. Each
// line names the server and the next command.
func TestRaglitAndPolyLspCommands_StatusOnOffAutoManualUnsetRestart(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ctx := context.Background()
	ws := t.TempDir()
	h, err := dun.Start(ctx, dun.Config{Workspace: ws, Servers: []dun.Server{
		{ID: dun.ServerDocs, URL: fakeToolServer(t, "search").URL, Timeout: 10},
		{ID: dun.ServerCode, URL: fakeToolServer(t, "node_query").URL, Timeout: 10},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, id := range []string{"raglit", "poly-lsp"} {
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
		step("", id+": stopped", "switches: global off, project unset, session unset", "/"+id+" on to start it this session")
		step("on", id)
		running(true)
		step("status", id+": running · 1 tools", "session on", "/"+id+" off to stop it this session")
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
	// Bare /raglit lists its commands with their help.
	if out := runServerCmd(ctx, h, "raglit", ""); !strings.Contains(out, "commands:") || !strings.Contains(out, "/raglit include ") ||
		!strings.Contains(out, "/raglit excludes ") || !strings.Contains(out, "/raglit watch ") {
		t.Errorf("bare /raglit lists its commands:\n%s", out)
	}
	if out := runServerCmd(ctx, h, "poly-lsp", ""); strings.Contains(out, "commands:") {
		t.Errorf("poly-lsp has none:\n%s", out)
	}
	l := commandList(h.Servers(), false)
	names := map[string]bool{}
	for _, c := range l.Items {
		names[c.Name] = true
	}
	if !names["raglit"] || !names["poly-lsp"] {
		t.Errorf("both are engine commands: %v", names)
	}
}
