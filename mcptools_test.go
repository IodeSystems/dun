package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// fakeMCPTools is a Streamable HTTP MCP server serving the named tools.
func fakeMCPTools(t *testing.T, names ...string) *httptest.Server {
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
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			var tools []map[string]any
			for _, n := range names {
				tools = append(tools, map[string]any{"name": n, "description": "does " + n + "\nin detail",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}})
			}
			result = map[string]any{"tools": tools}
		}
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		fmt.Fprint(w, string(out))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func modelTools(h *Harness) []string {
	var out []string
	for _, td := range h.Session.Tools {
		out = append(out, td.Function.Name)
	}
	return out
}

// A Disable entry is a name or a glob.
func TestFilterTools_Globs(t *testing.T) {
	tools := []mcpmgr.MCPTool{{Name: "tts_say", ServerID: "el"}, {Name: "tts_list", ServerID: "el"},
		{Name: "voices", ServerID: "el"}, {Name: "[odd", ServerID: "el"}, {Name: "tts_say", ServerID: "other"}}
	got := filterTools(tools, []Server{{ID: "el", Disable: []string{"tts_*", "[odd"}}})
	var names []string
	for _, t := range got {
		names = append(names, t.ServerID+"/"+t.Name)
	}
	if !slices.Equal(names, []string{"el/voices", "other/tts_say"}) {
		t.Errorf("got %v", names)
	}
}

// Every discovered tool is listed with its cost, hidden ones included; a
// toggle writes the user server list and the model's tool set follows; a
// hidden tool turns back on; one a glob hides says so instead.
func TestSetToolOn_GlobalWritesDisableAndRebuilds(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCPTools(t, "tts_say", "tts_list", "voices")
	h := newServerHarness(t)
	ctx := context.Background()
	if _, err := h.AddServer(ctx, ServerSpec{ID: "el", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	st := h.Servers()[0]
	if st.Tools != 3 || st.Cost <= 0 || st.Hidden != 0 {
		t.Fatalf("state %+v", st)
	}
	full := st.Cost

	off, on := false, true
	path, _, err := h.SetToolOn("el", "tts_list", ScopeGlobal, &off)
	if err != nil || path != UserServersFile() {
		t.Fatalf("off: %q %v", path, err)
	}
	if f, _ := readServersFile(UserServersFile()); !slices.Equal(f.Servers[0].Disable, []string{"tts_list"}) {
		t.Errorf("saved %+v", f.Servers[0])
	}
	if slices.Contains(modelTools(h), "el__tts_list") || !slices.Contains(modelTools(h), "el__tts_say") {
		t.Errorf("model tools %v", modelTools(h))
	}
	st = h.Servers()[0]
	if st.Tools != 2 || st.Hidden != 1 || st.Cost >= full {
		t.Errorf("after: %+v (full %d)", st, full)
	}
	tools, _ := h.ServerTools("el")
	i := slices.IndexFunc(tools, func(x ToolInfo) bool { return x.Name == "tts_list" })
	if len(tools) != 3 || i < 0 || tools[i].Enabled || tools[i].HiddenBy != "tts_list" || tools[i].Tokens <= 0 ||
		!strings.Contains(tools[i].Schema, `"properties"`) {
		t.Fatalf("the hidden tool must still be listed: %+v", tools)
	}

	if _, _, err := h.SetToolOn("el", "tts_list", ScopeGlobal, &on); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(modelTools(h), "el__tts_list") {
		t.Errorf("back on: %v", modelTools(h))
	}
	// Emptied, the list is written as [] — it must still override.
	if f, _ := readServersFile(UserServersFile()); f.Servers[0].Disable == nil || len(f.Servers[0].Disable) != 0 {
		t.Errorf("saved %#v", f.Servers[0].Disable)
	}
	if p, changed, err := h.SetToolOn("el", "voices", ScopeGlobal, &on); p != "" || changed || err != nil {
		t.Errorf("already on: %q %v", p, err)
	}

	// A glob hides more than one; turning one back on cannot shrink it.
	if _, err := SetUserServerDisable("el", []string{"tts_*"}); err != nil {
		t.Fatal(err)
	}
	if err := h.reloadServers(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.SetToolOn("el", "tts_say", ScopeGlobal, &on); err == nil || !strings.Contains(err.Error(), `"tts_*"`) {
		t.Errorf("glob: %v", err)
	}
	// A session switch turns it on over the glob, for this session only.
	if _, _, err := h.SetToolOn("el", "tts_say", ScopeSession, &on); err != nil || !slices.Contains(modelTools(h), "el__tts_say") {
		t.Errorf("session over glob: %v %v", err, modelTools(h))
	}
	if _, _, err := h.SetToolOn("el", "nope", ScopeGlobal, &off); err == nil {
		t.Error("an unknown tool must be refused")
	}
}

// A project switch for a tool goes to the project's machine state, never
// to the repo.
func TestSetToolOn_ProjectGoesToStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	ts := fakeMCPTools(t, "a", "b")
	h := newServerHarness(t, Server{ID: "proj", URL: ts.URL})
	writeJSON(t, h.cfg.Workspace, ProjectServersFile, `{"servers":[{"id":"proj","url":"`+ts.URL+`"}]}`)
	if err := h.StartServer(context.Background(), "proj"); err != nil {
		t.Fatal(err)
	}
	off := false
	path, _, err := h.SetToolOn("proj", "a", ScopeProject, &off)
	if err != nil || !strings.HasPrefix(path, home) || filepath.Base(path) != LocalServersFile {
		t.Fatalf("%q %v", path, err)
	}
	got, err := LoadServers(h.cfg.Workspace, h.cfg.Workspace, "")
	if p := byID(got)["proj"]; err != nil || p.ProjectTools["a"] != false || len(p.ProjectTools) != 1 {
		t.Errorf("reload: %+v %v", p, err)
	}
	if slices.Contains(modelTools(h), "proj__a") || !slices.Contains(modelTools(h), "proj__b") {
		t.Errorf("model tools %v", modelTools(h))
	}
}

// The empty list survives a write; nil stays absent.
func TestServerSpec_EmptyDisableIsWritten(t *testing.T) {
	b, _ := json.Marshal(ServerSpec{ID: "x", Disable: []string{}})
	if !strings.Contains(string(b), `"disable":[]`) {
		t.Errorf("%s", b)
	}
	b, _ = json.Marshal(ServerSpec{ID: "x"})
	if strings.Contains(string(b), "disable") {
		t.Errorf("%s", b)
	}
}
