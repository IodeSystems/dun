package mcpserver

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/project"
)

func boolp(b bool) *bool { return &b }

// poly-lsp is keyed on the workspace, raglit is not.
func TestWorkspaceKeyed_Builtins(t *testing.T) {
	if !workspaceKeyed(Spec{ID: ServerCode}) || workspaceKeyed(Spec{ID: ServerDocs}) || workspaceKeyed(Spec{ID: ServerShell}) {
		t.Error("poly-lsp keyed, raglit and the shell not")
	}
	if !workspaceKeyed(Spec{ID: "x", Env: []string{"ROOT={{workspace}}"}}) {
		t.Error("an env naming the workspace")
	}
}

// The session's switch wins, then the project's, then the global one; unset
// falls through.
func TestResolve_SessionOverProjectOverGlobal(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := PutUserServer(Spec{ID: "x", Command: "x-mcp", Autostart: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	get := func(sess map[string]SessionServer) Server {
		t.Helper()
		got, err := Resolve(dir, dir, "", DefaultServers(dir, ""), sess, nil)
		if err != nil {
			t.Fatal(err)
		}
		return byID(got)["x"]
	}
	if s := get(nil); !s.Autostart || s.Scopes.OnFrom != ScopeGlobal {
		t.Fatalf("global: %+v", s.Scopes)
	}
	if _, err := UpdateLocalServer(dir, "x", func(s *Spec) { s.Autostart = boolp(false) }); err != nil {
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
	if _, err := UpdateLocalServer(dir, "x", func(s *Spec) { s.Autostart = nil }); err != nil {
		t.Fatal(err)
	}
	if s := get(nil); !s.Autostart || s.Scopes.OnFrom != ScopeGlobal || s.Scopes.Project != nil {
		t.Fatalf("unset: %+v", s.Scopes)
	}
	// A repo's dun.json is the project value while the project state is unset.
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"x","autostart":false}]}`)
	if s := get(nil); s.Autostart || s.Scopes.OnFrom != ScopeProject {
		t.Fatalf("dun.json: %+v", s.Scopes)
	}
}

// A tool's switch: session, then project (single tool, then a deny-list),
// then global.
func TestToolState_Scopes(t *testing.T) {
	s := Server{Disable: []string{"g_*"}}
	if on, from, by := s.ToolState("g_a"); on || from != ScopeGlobal || by != "g_*" {
		t.Errorf("global: %v %s %s", on, from, by)
	}
	s.ProjectDisable = []string{}
	if on, from, _ := s.ToolState("g_a"); !on || from != ScopeProject {
		t.Errorf("project list replaces the global one: %v %s", on, from)
	}
	s.ProjectTools = map[string]bool{"g_a": false}
	if on, from, _ := s.ToolState("g_a"); on || from != ScopeProject {
		t.Errorf("project tool: %v %s", on, from)
	}
	s.SessionTools = map[string]bool{"g_a": true}
	if on, from, _ := s.ToolState("g_a"); !on || from != ScopeSession {
		t.Errorf("session tool: %v %s", on, from)
	}
	if g, p, ss := s.ToolScopes("g_a"); g != "off" || p != "off" || ss != "on" {
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
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"d","command":"d-mcp","autostart":true},{"id":"e","disable":["t2"]}]}`)
	for _, edit := range []struct {
		id string
		f  func(*Spec)
	}{
		{ServerDocs, func(s *Spec) { s.Autostart = boolp(true) }},
		{"b", func(s *Spec) { s.Autostart, s.Disable = boolp(false), []string{} }},
		{"a", func(s *Spec) { s.Via = ViaDirect }},
		{"c", func(s *Spec) { s.Disable = []string{"t2"} }},
	} {
		if _, err := UpdateLocalServer(dir, edit.id, edit.f); err != nil {
			t.Fatal(err)
		}
	}

	// The old resolution: every layer merged in order, last one wins.
	old := map[string]Spec{}
	var order []string
	for _, s := range DefaultServers(dir, "") {
		old[s.ID] = specOf(s)
		order = append(order, s.ID)
	}
	for _, path := range append([]string{UserServersFile()}, serverConfigPaths(dir)...) {
		f, err := ReadConfig(path)
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
		t.Fatalf("servers %v, want %v", IDs(got), order)
	}
	for _, id := range order {
		o, n := old[id], now[id]
		via, _ := NormalizeVia(o.Via)
		if wantOn := o.Autostart != nil && *o.Autostart; n.Autostart != wantOn || n.Via != via {
			t.Errorf("%s: on %v via %q, was on %v via %q", id, n.Autostart, n.Via, wantOn, via)
		}
		for _, tool := range []string{"t1", "t2", "t3", "search_figures", "search"} {
			on, _, _ := n.ToolState(tool)
			if was := HiddenBy(tool, o.Disable) == ""; on != was {
				t.Errorf("%s.%s: on %v, was %v", id, tool, on, was)
			}
		}
	}
}

// An env value that is wholly $NAME is read from dun's environment at start;
// anything else stays as written.
func TestEnvRefs(t *testing.T) {
	t.Setenv("DUNTEST_DSN", "pg://x")
	got := envRefs([]string{"A=$DUNTEST_DSN", "B=${DUNTEST_DSN}", "C=a$DUNTEST_DSN", "D=lit", "E"})
	if want := []string{"A=pg://x", "B=pg://x", "C=a$DUNTEST_DSN", "D=lit", "E"}; !slices.Equal(got, want) {
		t.Errorf("%q", got)
	}
}

// The default mode is read from mcp.json; a server no scope sets a mode for
// takes it, and says so. A scope's value, a built-in, and instance agent all
// beat it.
func TestResolve_DefaultVia(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	for _, id := range []string{"a", "b", "c", "d"} {
		if _, err := PutUserServer(Spec{ID: id, Command: id + "-mcp", Autostart: boolp(true)}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(sess map[string]SessionServer) map[string]Server {
		t.Helper()
		got, err := Resolve(dir, dir, "", DefaultServers(dir, ""), sess, nil)
		if err != nil {
			t.Fatal(err)
		}
		return byID(got)
	}
	if v, err := LoadDefaultVia(); err != nil || v != ViaDirect {
		t.Fatalf("unset default: %q %v", v, err)
	}
	if s := get(nil)["a"]; s.Via != "" || s.Scopes.ViaFrom != ScopeGlobal {
		t.Fatalf("standalone default: %q from %q", s.Via, s.Scopes.ViaFrom)
	}
	if _, err := SetDefaultVia("sideways"); err == nil {
		t.Error("a bad default is refused")
	}
	if _, err := SetDefaultVia(ViaShell); err != nil {
		t.Fatal(err)
	}
	// An explicit global standalone (stored as "direct") beats the default;
	// a project's scripted and a session's standalone beat it too.
	if _, err := UpsertUserServer("b", func(s *Spec) { s.Via = ViaDirect }); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateLocalServer(dir, "c", func(s *Spec) { s.Via = ViaShell }); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertUserServer("d", func(s *Spec) { s.Instance = InstanceAgent }); err != nil {
		t.Fatal(err)
	}
	all := get(map[string]SessionServer{"c": {Via: ViaDirect}})
	for _, c := range []struct{ id, via, from string }{
		{"a", ViaShell, ViaFromDefault},
		{"b", "", ScopeGlobal},
		{"c", "", ScopeSession},
		{"d", "", ViaFromDefaultAgent}, // agent cannot be scripted: standalone, and the cell says why
		{ServerShell, "", ""},
		{ServerCode, "", ""},
	} {
		if s := all[c.id]; s.Via != c.via || s.Scopes.ViaFrom != c.from {
			t.Errorf("%s: via %q from %q, want %q from %q", c.id, s.Via, s.Scopes.ViaFrom, c.via, c.from)
		}
	}
	if s := all["d"]; s.Instance != InstanceAgent {
		t.Errorf("d keeps its instance: %q", s.Instance)
	}
	if _, err := SetDefaultVia(ViaDirect); err != nil {
		t.Fatal(err)
	}
	if s := get(nil)["a"]; s.Via != "" || s.Scopes.ViaFrom != ScopeGlobal {
		t.Errorf("back to standalone: %q from %q", s.Via, s.Scopes.ViaFrom)
	}
}

// A bad default_via reads as standalone and takes no server down; setting
// one by command is still refused.
func TestResolve_BadDefaultViaFallsBack(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := PutUserServer(Spec{ID: "a", Command: "a-mcp", Autostart: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	f, _ := ReadConfig(UserServersFile())
	f.DefaultVia = "sideways"
	if err := writeUserServers(UserServersFile(), f); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(dir, dir, "", DefaultServers(dir, ""), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := byID(got)["a"]; s.Via != "" || s.Scopes.ViaFrom != ScopeGlobal {
		t.Errorf("fallback: %q from %q", s.Via, s.Scopes.ViaFrom)
	}
	if _, err := SetDefaultVia("sideways"); err == nil {
		t.Error("a bad default is refused when set")
	}
}

// The nearest scope that names an index decides it.
func TestEffectiveIndexes_NearestScopeDecides(t *testing.T) {
	sc := Scopes{
		AttachGlobal: []string{"a:x", "own:m"}, DetachSession: []string{"a:x"},
		DetachProject: []string{"own:m"}, AttachSession: []string{"own:m", "b:y"},
	}
	if on := effectiveIndexes(sc, true); !slices.Equal(on, []string{"own:m", "b:y"}) {
		t.Errorf("on: %v", on)
	}
	if off := effectiveIndexes(sc, false); !slices.Equal(off, []string{"a:x"}) {
		t.Errorf("off: %v", off)
	}
}

// Attachments at each scope add up; each is listed with where it is set.
// The session's survive a resume (the .mcp.json sidecar).
func TestResolve_AttachIsTheUnionOfTheScopes(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := UpsertUserServer(ServerDocs, func(s *Spec) { s.Attach = []string{"g:one", "both:two"} }); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateLocalServer(dir, ServerDocs, func(s *Spec) { s.Attach = []string{"both:two", "p:three"} }); err != nil {
		t.Fatal(err)
	}
	sessFile := filepath.Join(t.TempDir(), "s.jsonl")
	if _, err := SaveSessionServers(sessFile, map[string]SessionServer{ServerDocs: {Attach: []string{"s:four"}}}); err != nil {
		t.Fatal(err)
	}
	sess := LoadSessionServers(sessFile)
	if !slices.Equal(sess[ServerDocs].Attach, []string{"s:four"}) {
		t.Fatalf("an attach-only session entry is kept: %+v", sess)
	}
	got, err := Resolve(dir, dir, "", DefaultServers(dir, ""), sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := byID(got)[ServerDocs]
	if !slices.Equal(s.Attach, []string{"g:one", "both:two", "p:three", "s:four"}) {
		t.Errorf("union: %v", s.Attach)
	}
	if !slices.Equal(s.Scopes.AttachProject, []string{"both:two", "p:three"}) || !slices.Equal(s.Scopes.AttachSession, []string{"s:four"}) {
		t.Errorf("scopes: %+v", s.Scopes)
	}
	// A server another workspace defines, attached here only, is not one.
	if _, err := UpsertUserServer("elsewhere", func(s *Spec) { s.Attach = []string{"x:y"} }); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(dir, dir, "", DefaultServers(dir, ""), nil, nil); err != nil {
		t.Errorf("an attach-only entry is switches only: %v", err)
	}
}

// The instance switch resolves as the mode does: session, then project,
// then global; unset everywhere is shared. A built-in's is fixed.
func TestResolve_Instance(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := PutUserServer(Spec{ID: "x", Command: "x-mcp", Autostart: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	get := func(sess map[string]SessionServer) map[string]Server {
		t.Helper()
		got, err := Resolve(dir, dir, "", DefaultServers(dir, ""), sess, nil)
		if err != nil {
			t.Fatal(err)
		}
		return byID(got)
	}
	if s := get(nil)["x"]; s.Instance != InstanceShared || s.Scopes.InstanceFrom != ScopeGlobal {
		t.Fatalf("default: %q from %q", s.Instance, s.Scopes.InstanceFrom)
	}
	if _, err := UpdateLocalServer(dir, "x", func(s *Spec) { s.Instance = InstanceAgent }); err != nil {
		t.Fatal(err)
	}
	if s := get(nil)["x"]; s.Instance != InstanceAgent || s.Scopes.InstanceFrom != ScopeProject {
		t.Fatalf("project: %q from %q", s.Instance, s.Scopes.InstanceFrom)
	}
	sess := map[string]SessionServer{"x": {Instance: InstanceSession}}
	if s := get(sess)["x"]; s.Instance != InstanceSession || s.Scopes.InstanceFrom != ScopeSession || s.Scopes.InstanceProject != InstanceAgent {
		t.Fatalf("session: %q %+v", s.Instance, s.Scopes)
	}
	// Scripted with agent runs as session: a child has no shell of its own.
	if s := get(map[string]SessionServer{"x": {Via: ViaShell}})["x"]; s.Instance != InstanceSession {
		t.Fatalf("scripted agent: %q", s.Instance)
	}
	all := get(nil)
	for id, want := range map[string]string{ServerShell: InstanceSession, ServerCode: InstanceWorkspace, ServerDocs: InstanceShared} {
		if s := all[id]; s.Instance != want || s.Scopes.InstanceFrom != "" {
			t.Errorf("%s: %q from %q, want %q fixed", id, s.Instance, s.Scopes.InstanceFrom, want)
		}
	}
	if _, err := Resolve(dir, dir, "", DefaultServers(dir, ""), map[string]SessionServer{ServerShell: {Instance: InstanceAgent}}, nil); err == nil || !strings.Contains(err.Error(), "fixed") {
		t.Errorf("a built-in's instance set: %v", err)
	}
	if _, err := NormalizeInstance("thread"); err == nil {
		t.Error("an unknown instance is refused")
	}
}
