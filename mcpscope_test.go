package dun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func boolp(b bool) *bool { return &b }

// The session's switch wins, then the project's, then the global one; unset
// falls through.
func TestResolve_SessionOverProjectOverGlobal(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := PutUserServer(ServerSpec{ID: "x", Command: "x-mcp", Autostart: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	get := func(sess map[string]SessionServer) Server {
		t.Helper()
		got, err := resolveServers(dir, dir, "", DefaultServers(dir, ""), sess, nil)
		if err != nil {
			t.Fatal(err)
		}
		return byID(got)["x"]
	}
	if s := get(nil); !s.Autostart || s.Scopes.OnFrom != ScopeGlobal {
		t.Fatalf("global: %+v", s.Scopes)
	}
	if _, err := updateLocalServer(dir, "x", func(s *ServerSpec) { s.Autostart = boolp(false) }); err != nil {
		t.Fatal(err)
	}
	if s := get(nil); s.Autostart || s.Scopes.OnFrom != ScopeProject {
		t.Fatalf("project: %+v", s.Scopes)
	}
	on := map[string]SessionServer{"x": {On: boolp(true)}}
	if s := get(on); !s.Autostart || s.Scopes.OnFrom != ScopeSession || *s.Scopes.Project {
		t.Fatalf("session: %+v", s.Scopes)
	}
	// Via resolves the same way, independently.
	via := map[string]SessionServer{"x": {Via: ViaShell}}
	if s := get(via); s.Via != ViaShell || s.Scopes.ViaFrom != ScopeSession || s.Scopes.OnFrom != ScopeProject {
		t.Fatalf("via: %+v %q", s.Scopes, s.Via)
	}
	// Unset at project: the global value again, and the stub entry is gone.
	if _, err := updateLocalServer(dir, "x", func(s *ServerSpec) { s.Autostart = nil }); err != nil {
		t.Fatal(err)
	}
	if s := get(nil); !s.Autostart || s.Scopes.OnFrom != ScopeGlobal || s.Scopes.Project != nil {
		t.Fatalf("unset: %+v", s.Scopes)
	}
	// A repo's dun.json is the project value while the project state is unset.
	writeJSON(t, dir, ProjectServersFile, `{"servers":[{"id":"x","autostart":false}]}`)
	if s := get(nil); s.Autostart || s.Scopes.OnFrom != ScopeProject {
		t.Fatalf("dun.json: %+v", s.Scopes)
	}
}

// A tool's switch: session, then project (single tool, then a deny-list),
// then global.
func TestToolState_Scopes(t *testing.T) {
	s := Server{Disable: []string{"g_*"}}
	if on, from, by := s.toolState("g_a"); on || from != ScopeGlobal || by != "g_*" {
		t.Errorf("global: %v %s %s", on, from, by)
	}
	s.ProjectDisable = []string{}
	if on, from, _ := s.toolState("g_a"); !on || from != ScopeProject {
		t.Errorf("project list replaces the global one: %v %s", on, from)
	}
	s.ProjectTools = map[string]bool{"g_a": false}
	if on, from, _ := s.toolState("g_a"); on || from != ScopeProject {
		t.Errorf("project tool: %v %s", on, from)
	}
	s.SessionTools = map[string]bool{"g_a": true}
	if on, from, _ := s.toolState("g_a"); !on || from != ScopeSession {
		t.Errorf("session tool: %v %s", on, from)
	}
	if g, p, ss := s.toolScopes("g_a"); g != "off" || p != "off" || ss != "on" {
		t.Errorf("scopes %s %s %s", g, p, ss)
	}
}

// Nothing is migrated, so existing files must mean what they meant: for
// every server, with no session switch, autostart, placement and which tools
// show are what the old one-merge-over-every-layer gave.
func TestResolve_ExistingFilesKeepTheirMeaning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	dir := t.TempDir()
	writeJSON(t, filepath.Dir(UserServersFile()), "mcp.json", `{"servers":[
		{"id":"a","command":"a-mcp","autostart":true,"via":"shell"},
		{"id":"b","command":"b-mcp","autostart":true,"disable":["t1"]},
		{"id":"c","command":"c-mcp"},
		{"id":"e","command":"e-mcp","autostart":true,"disable":["t*"]}]}`)
	writeJSON(t, dir, ProjectServersFile, `{"servers":[{"id":"d","command":"d-mcp","autostart":true},{"id":"e","disable":["t2"]}]}`)
	for _, edit := range []struct {
		id string
		f  func(*ServerSpec)
	}{
		{ServerDocs, func(s *ServerSpec) { s.Autostart = boolp(true) }},
		{"b", func(s *ServerSpec) { s.Autostart, s.Disable = boolp(false), []string{} }},
		{"a", func(s *ServerSpec) { s.Via = ViaDirect }},
		{"c", func(s *ServerSpec) { s.Disable = []string{"t2"} }},
	} {
		if _, err := updateLocalServer(dir, edit.id, edit.f); err != nil {
			t.Fatal(err)
		}
	}

	// The old resolution: every layer merged in order, last one wins.
	old := map[string]ServerSpec{}
	var order []string
	for _, s := range DefaultServers(dir, "") {
		old[s.ID] = specOf(s)
		order = append(order, s.ID)
	}
	for _, path := range append([]string{UserServersFile()}, serverConfigPaths(dir)...) {
		f, err := readServersFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if f == nil {
			continue
		}
		for _, s := range f.Servers {
			if _, ok := old[s.ID]; !ok {
				order = append(order, s.ID)
			}
			old[s.ID] = mergeSpec(old[s.ID], s)
		}
	}

	got, err := LoadServers(dir, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	now := byID(got)
	if len(now) != len(order) {
		t.Fatalf("servers %v, want %v", ServerIDs(got), order)
	}
	for _, id := range order {
		o, n := old[id], now[id]
		via, _ := NormalizeVia(o.Via)
		if wantOn := o.Autostart != nil && *o.Autostart; n.Autostart != wantOn || n.Via != via {
			t.Errorf("%s: on %v via %q, was on %v via %q", id, n.Autostart, n.Via, wantOn, via)
		}
		for _, tool := range []string{"t1", "t2", "t3", "search_figures", "search"} {
			on, _, _ := n.toolState(tool)
			if was := hiddenBy(tool, o.Disable) == ""; on != was {
				t.Errorf("%s.%s: on %v, was %v", id, tool, on, was)
			}
		}
	}
}

// A session's switches survive the process: a fresh Start on the same
// session file reads them back.
func TestSessionSwitches_PersistAcrossResume(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCPTools(t, "a")
	file := filepath.Join(t.TempDir(), "s.jsonl")
	cfg := Config{Workspace: t.TempDir(), SessionFile: file, Servers: []Server{{ID: "el", URL: ts.URL, Autostart: true}}}
	ctx := context.Background()
	h, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	path, err := h.SetServerOn(ctx, "el", ScopeSession, boolp(false))
	if err != nil || path != SessionServersFile(file) {
		t.Fatalf("%q %v", path, err)
	}
	if h.mgr.ServerStarted("el") {
		t.Error("off for the session, still running")
	}
	h.Close()
	if _, err := os.Stat(SessionServersFile(file)); err != nil {
		t.Fatal(err)
	}

	h, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	st, _ := serverState(h, "el")
	if st.Auto || st.Session != "off" || st.From != ScopeSession || st.Global != "on" {
		t.Fatalf("resumed: %+v", st)
	}
	if _, err := h.SetServerOn(ctx, "el", ScopeSession, nil); err != nil {
		t.Fatal(err)
	}
	if !h.mgr.ServerStarted("el") {
		t.Error("unset at session: the global on applies and it starts")
	}
}

// A running session follows a global or project change at its next turn,
// unless its own switch is set.
func TestFollowServers_GlobalChangeReachesARunningSession(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCPTools(t, "a")
	h := newServerHarness(t)
	ctx := context.Background()
	if _, err := h.AddServer(ctx, ServerSpec{ID: "el", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	if !h.mgr.ServerStarted("el") {
		t.Fatal("added: not running")
	}
	// Another session turns it off globally: this one's file is all it sees.
	if _, err := UpsertUserServer("el", func(s *ServerSpec) { s.Autostart = boolp(false) }); err != nil {
		t.Fatal(err)
	}
	h.followServers(ctx)
	if h.mgr.ServerStarted("el") || slices.Contains(modelTools(h), "el__a") {
		t.Fatalf("global off not followed: %v", modelTools(h))
	}
	// Its own switch on: a global change no longer moves it.
	if _, err := h.SetServerOn(ctx, "el", ScopeSession, boolp(true)); err != nil || !h.mgr.ServerStarted("el") {
		t.Fatalf("session on: %v", err)
	}
	if _, err := UpsertUserServer("el", func(s *ServerSpec) { s.Autostart = boolp(false) }); err != nil {
		t.Fatal(err)
	}
	h.followServers(ctx)
	if !h.mgr.ServerStarted("el") {
		t.Error("the session switch must hold over the global one")
	}
	// Placement too: a session switch to the shell stops the direct one.
	if _, err := h.SetServerVia(ctx, "el", ScopeSession, ViaShell); err != nil {
		t.Fatal(err)
	}
	if h.mgr.ServerStarted("el") {
		t.Error("in the shell, but still started directly")
	}
	if _, err := h.SetServerVia(ctx, "el", ScopeSession, ""); err != nil || !h.mgr.ServerStarted("el") {
		t.Errorf("via unset: back to direct and started: %v", err)
	}
	if _, err := h.SetServerVia(ctx, ServerCode, ScopeSession, ViaShell); err == nil {
		t.Error("a built-in has no placement")
	}
	if _, err := h.SetServerOn(ctx, "el", ScopeGlobal, nil); err == nil {
		t.Error("global has no unset")
	}
}
