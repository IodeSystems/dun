package mcpserver

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/iodesystems/dun/internal/fsutil"
)

// Three scopes of server switches.
//
// Every server has three switches for whether it is on, three for where it
// runs (direct or in the shell), and three per tool:
//
//	global   $DUN_HOME/mcp.json — every workspace. On or off; a built-in's
//	         default (shell on, code and docs off) is its global value.
//	project  the project's machine state, $DUN_HOME/projects/<id>/dun.local.json
//	         (or the worktree's overlay). Unset, on or off.
//	session  this session's .mcp.json sidecar, beside its .jsonl, so it holds
//	         across a resume and a dund restart. Unset, on or off.
//
// The value used is the session's if set, else the project's, else the
// global one. A repo's committed dun.json is not a fourth scope: what it says
// is the project value until the project's state says otherwise (it is a
// lower layer of the same merge).
//
// This is the old layering with a session layer on top: mcp.json was
// already below the project files, and the project files already replaced
// what mcp.json said. So existing files keep their meaning and nothing is
// migrated. One difference: a project deny-list ("disable") still sets
// every tool at project scope, as it used to, but a single tool switched at
// project or session scope ("tools": {"name": false}) overrides it.
//
// A running session follows a global or project change at its next turn
// (followServers): it re-reads the files, and starts, stops or re-hosts what
// changed. A server whose resolved value did not change is left alone, so a
// server that failed to start is not retried every turn.

// Scope names, as typed (--global) and as shown.
const (
	ScopeGlobal  = "global"
	ScopeProject = "project"
	ScopeSession = "session"
	// scopeFlag is a --rag/--lsp flag: this run only, under the session.
	scopeFlag = "flag"
)

// ParseScope reads "global", "--global", "-g" and the like.
func ParseScope(s string) (string, bool) {
	switch strings.TrimLeft(strings.ToLower(strings.TrimSpace(s)), "-") {
	case "global", "g":
		return ScopeGlobal, true
	case "project", "p":
		return ScopeProject, true
	case "session", "s":
		return ScopeSession, true
	}
	return "", false
}

// ViaFrom values beside the scope names: no scope set a mode, and the global
// default (LoadDefaultVia) is scripted. The second is that default overruled:
// the server's instance is agent, which a scripted server cannot be.
const (
	ViaFromDefault      = "default"
	ViaFromDefaultAgent = "default, instance agent"
)

// Scopes is one server's switches at each scope, and which one
// decided. nil and "" are unset.
type Scopes struct {
	Global, Project, Session *bool
	OnFrom                   string // the scope that decided Autostart
	ViaGlobal, ViaProject    string // "", ViaDirect, ViaShell
	ViaSession               string
	ViaFrom                  string // the scope that decided Via, or ViaFromDefault*; "" for a built-in
	// The instance switch at each scope ("", InstanceShared, InstanceSession,
	// InstanceAgent) and the scope that decided Server.Instance; "" for a
	// built-in, whose instance is fixed (builtinInstances).
	InstanceGlobal, InstanceProject, InstanceSession string
	InstanceFrom                                     string
	// Indexes switched on (attached) and off (detached) at each scope
	// (harness/attach.go); Server.Attach and Server.Detach are the result.
	AttachGlobal, AttachProject, AttachSession []string
	DetachGlobal, DetachProject, DetachSession []string
}

// GlobalOn is the global switch as shown: unset is off, except for a server
// placed in the shell, which the shell always hosted.
func (s Scopes) GlobalOn(via string) bool {
	if s.Global != nil {
		return *s.Global
	}
	return via == ViaShell
}

// SessionServer is one server's switches in a session's sidecar.
type SessionServer struct {
	On  *bool  `json:"on,omitempty"`
	Via string `json:"via,omitempty"`
	// Instance is Spec.Instance at this scope.
	Instance string          `json:"instance,omitempty"`
	Tools    map[string]bool `json:"tools,omitempty"`
	Attach   []string        `json:"attach,omitempty"`
	Detach   []string        `json:"detach,omitempty"`
}

func (s SessionServer) Empty() bool {
	return s.On == nil && s.Via == "" && s.Instance == "" && len(s.Tools) == 0 && len(s.Attach) == 0 && len(s.Detach) == 0
}

// SessionServersFile is the session's switch sidecar, "" for an in-memory
// session (mirrors StateFile).
func SessionServersFile(sessionFile string) string {
	if sessionFile == "" {
		return ""
	}
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".mcp.json"
}

type sessionServersFile struct {
	Servers map[string]SessionServer `json:"servers"`
}

// LoadSessionServers reads a session's switches. Missing or unreadable is
// none: a bad sidecar must not be why a resume fails.
func LoadSessionServers(sessionFile string) map[string]SessionServer {
	out := map[string]SessionServer{}
	p := SessionServersFile(sessionFile)
	if p == "" {
		return out
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return out
	}
	var f sessionServersFile
	if json.Unmarshal(b, &f) != nil {
		log.Printf("dun: %s is not readable; this session's server switches start unset", p)
		return out
	}
	for id, s := range f.Servers {
		if _, current := f.Servers[CanonicalID(id)]; s.Empty() || (id != CanonicalID(id) && current) {
			continue // a pre-rename key loses to the current one
		}
		out[CanonicalID(id)] = s
	}
	return out
}

// SaveSessionServers writes a session's switches to its sidecar and returns
// the file written ("" for an in-memory session).
func SaveSessionServers(sessionFile string, sw map[string]SessionServer) (string, error) {
	p := SessionServersFile(sessionFile)
	if p == "" {
		return "", nil
	}
	b, err := json.MarshalIndent(sessionServersFile{Servers: sw}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	return p, fsutil.WriteAtomic(p, append(b, '\n'), 0o600)
}

// catalogEntry is one server across the layers: def is every layer merged
// (what the server IS), global and project the switch values of those
// layers alone.
type catalogEntry struct {
	def, global, project Spec
}

// specOf is a Go-level Server as a global-layer spec.
func specOf(s Server) Spec {
	auto := s.Autostart
	return Spec{ID: s.ID, Command: s.Command, Args: s.Args, URL: s.URL, Headers: s.Headers, Bearer: s.Bearer,
		TokenCmd: s.TokenCmd, Env: s.Env, Timeout: s.Timeout, Autostart: &auto, Disable: s.Disable, Via: s.Via, Instance: s.Instance, Shell: s.Shell, OAuth: s.OAuth}
}

// loadCatalog merges base (the built-ins, or a caller's Go-level servers),
// $DUN_HOME/mcp.json and dir's project files, keeping the global and project
// switches apart.
func loadCatalog(dir string, base []Server) ([]catalogEntry, error) {
	idx := map[string]int{}
	var out []catalogEntry
	put := func(spec Spec, project bool) {
		i, ok := idx[spec.ID]
		if !ok {
			i = len(out)
			idx[spec.ID] = i
			out = append(out, catalogEntry{def: Spec{ID: spec.ID}, global: Spec{ID: spec.ID}, project: Spec{ID: spec.ID}})
		}
		e := &out[i]
		e.def = mergeSpec(e.def, spec)
		if project {
			e.project = mergeSpec(e.project, spec)
		} else {
			e.global = mergeSpec(e.global, spec)
		}
	}
	for _, s := range base {
		put(specOf(s), false)
	}
	paths := []string{UserServersFile()}
	if dir != "" { // no project: a relative path would read the process's cwd
		paths = append(paths, serverConfigPaths(dir)...)
	}
	for n, path := range paths {
		f, err := ReadConfig(path)
		if err != nil {
			return nil, err
		}
		if f == nil {
			continue
		}
		for _, spec := range f.Servers {
			if spec.ID == "" {
				return nil, fmt.Errorf("dun: %s: a server entry has no id", path)
			}
			put(spec, n > 0)
		}
	}
	return out, nil
}

// switchesOnly reports a spec that sets switches and nothing else: an entry
// a scope wrote for a server some other workspace defines. A bare id is not
// one — that is a server someone forgot to define.
func switchesOnly(s Spec) bool {
	return s.Command == "" && s.URL == "" && s.Args == nil && s.Env == nil && s.Headers == nil &&
		s.Bearer == "" && s.TokenCmd == "" && s.OAuth == nil && s.Timeout == 0 &&
		(s.Wake || s.Autostart != nil || s.Via != "" || s.Instance != "" || s.Disable != nil || s.Tools != nil || s.Attach != nil || s.Detach != nil || s.Exclude != nil || s.IndexBudget != "" || s.IndexAnyway ||
			s.LSPRoots != "")
}

// resolve turns a catalog entry and this session's switches into the Server
// it runs as. ok=false: the server is not in this workspace (disabled, or
// only switches name it).
func (e catalogEntry) resolve(sess SessionServer, flag *bool, workspace, raglitHome, defVia string) (Server, bool, error) {
	s := e.def
	if s.Disabled {
		return Server{}, false, nil
	}
	if s.Command == "" && s.URL == "" {
		if switchesOnly(s) {
			return Server{}, false, nil
		}
		return Server{}, false, fmt.Errorf("dun: server %q has no command or url (declare one, or remove the entry)", s.ID)
	}
	sc := Scopes{Global: e.global.Autostart, Project: e.project.Autostart, Session: sess.On,
		AttachGlobal: e.global.Attach, AttachProject: e.project.Attach, AttachSession: sess.Attach,
		DetachGlobal: e.global.Detach, DetachProject: e.project.Detach, DetachSession: sess.Detach}
	var err error
	for _, v := range []struct {
		in  string
		out *string
	}{{e.global.Via, &sc.ViaGlobal}, {e.project.Via, &sc.ViaProject}, {sess.Via, &sc.ViaSession}} {
		if v.in == "" {
			continue
		}
		if *v.out, err = NormalizeVia(v.in); err != nil {
			return Server{}, false, fmt.Errorf("dun: server %q: %w", s.ID, err)
		}
		if *v.out == "" {
			*v.out = ViaDirect
		}
	}
	via := ViaDirect
	switch {
	case sc.ViaSession != "":
		via, sc.ViaFrom = sc.ViaSession, ScopeSession
	case sc.ViaProject != "":
		via, sc.ViaFrom = sc.ViaProject, ScopeProject
	case sc.ViaGlobal != "":
		via, sc.ViaFrom = sc.ViaGlobal, ScopeGlobal
	case defVia == ViaShell && !IsBuiltin(s.ID):
		// The global default places it. A server whose instance is agent
		// cannot be scripted (a sub-agent has no shell of its own): it stays
		// standalone, and the cell says why.
		via, sc.ViaFrom = ViaShell, ViaFromDefault
		if agentInstance(e.global.Instance, e.project.Instance, sess.Instance) {
			via, sc.ViaFrom = ViaDirect, ViaFromDefaultAgent
		}
	default:
		sc.ViaFrom = ScopeGlobal
	}
	if IsBuiltin(s.ID) {
		sc.ViaFrom = ""
		if via == ViaShell {
			return Server{}, false, fmt.Errorf("dun: server %q is built in and cannot be scripted (remove its \"via\")", s.ID)
		}
	}
	if via == ViaDirect {
		via = ""
	}
	instance, err := resolveInstance(s.ID, &sc, e.global.Instance, e.project.Instance, sess.Instance, via)
	if err != nil {
		return Server{}, false, err
	}
	if err := CheckOAuth(s, via); err != nil {
		return Server{}, false, fmt.Errorf("dun: server %q: %w", s.ID, err)
	}
	if err := CheckTokenCmd(s, via); err != nil {
		return Server{}, false, fmt.Errorf("dun: server %q: %w", s.ID, err)
	}
	var on bool
	switch {
	case sess.On != nil:
		on, sc.OnFrom = *sess.On, ScopeSession
	case flag != nil:
		on, sc.OnFrom = *flag, scopeFlag
	case e.project.Autostart != nil:
		on, sc.OnFrom = *e.project.Autostart, ScopeProject
	default:
		on, sc.OnFrom = sc.GlobalOn(via), ScopeGlobal
	}
	return Server{
		ID:             s.ID,
		URL:            s.URL,
		Headers:        requestHeaders(s),
		Command:        s.Command,
		Args:           expandPlaceholders(s.Args, workspace, raglitHome),
		Env:            envRefs(expandPlaceholders(s.Env, workspace, raglitHome)),
		Timeout:        s.Timeout,
		Autostart:      on,
		Disable:        e.global.Disable,
		ProjectDisable: e.project.Disable,
		ProjectTools:   e.project.Tools,
		SessionTools:   sess.Tools,
		Via:            via,
		Instance:       instance,
		PerWorkspace:   workspaceKeyed(s),
		Shell:          s.Shell,
		Bearer:         s.Bearer,
		TokenCmd:       s.TokenCmd,
		OAuth:          s.OAuth,
		Scopes:         sc,
		Attach:         effectiveIndexes(sc, true),
		Detach:         effectiveIndexes(sc, false),
		Exclude:        s.Exclude,
		IndexBudget:    s.IndexBudget,
		IndexAnyway:    s.IndexAnyway,
		LSPRoots:       s.LSPRoots,
		Wake:           s.Wake,
	}, true, nil
}

var warned sync.Map

// warnOnce logs msg the first time only: the servers resolve every turn.
func warnOnce(msg string) {
	if _, seen := warned.LoadOrStore(msg, true); !seen {
		log.Printf("%s — using standalone until it is fixed (/mcp default standalone|scripted)", msg)
	}
}

// agentInstance reports whether the nearest scope that sets an instance says
// agent. An unreadable value is left to resolveInstance to refuse.
func agentInstance(global, project, session string) bool {
	for _, v := range []string{session, project, global} {
		if v == "" {
			continue
		}
		inst, err := NormalizeInstance(v)
		return err == nil && inst == InstanceAgent
	}
	return false
}

// workspaceKeyed reports whether s's args or env name {{workspace}}: as
// written, or in its bundled manifest (whose base is expanded before the
// layers see it).
func workspaceKeyed(s Spec) bool {
	raw := slices.Concat(s.Args, s.Env)
	if e, ok := bundledExt(s.ID); ok {
		es := e.server()
		raw = slices.Concat(raw, es.Args, es.Env)
	}
	return slices.ContainsFunc(raw, func(a string) bool { return strings.Contains(a, "{{workspace}}") })
}

// resolveInstance is the nearest scope's instance switch, recorded in sc:
// shared when none sets it; a built-in's fixed one, which no scope may set.
// A scripted server set to agent runs as session: it runs in the session's
// shell, and a child has no shell of its own (the setters refuse that pair;
// a hand-edited file may still hold it).
func resolveInstance(id string, sc *Scopes, global, project, session, via string) (string, error) {
	var err error
	for _, v := range []struct {
		in  string
		out *string
	}{{global, &sc.InstanceGlobal}, {project, &sc.InstanceProject}, {session, &sc.InstanceSession}} {
		if v.in == "" {
			continue
		}
		if *v.out, err = NormalizeInstance(v.in); err != nil {
			return "", fmt.Errorf("dun: server %q: %w", id, err)
		}
		if *v.out == "" {
			*v.out = InstanceShared
		}
	}
	if fixed, _, ok := BuiltinInstance(id); ok {
		if sc.InstanceGlobal != "" || sc.InstanceProject != "" || sc.InstanceSession != "" {
			return "", fmt.Errorf("dun: server %q is built in and its instance is fixed (%s; remove its \"instance\")", id, fixed)
		}
		return fixed, nil
	}
	inst := InstanceShared
	switch {
	case sc.InstanceSession != "":
		inst, sc.InstanceFrom = sc.InstanceSession, ScopeSession
	case sc.InstanceProject != "":
		inst, sc.InstanceFrom = sc.InstanceProject, ScopeProject
	case sc.InstanceGlobal != "":
		inst, sc.InstanceFrom = sc.InstanceGlobal, ScopeGlobal
	default:
		sc.InstanceFrom = ScopeGlobal
	}
	if inst == InstanceAgent && via == ViaShell {
		inst = InstanceSession
	}
	return inst, nil
}

// envRefs reads each "KEY=$NAME" or "KEY=${NAME}" from dun's environment
// when the server starts, as Bearer is, so mcp.json can name a secret
// without holding it. Only a value that is wholly one reference: "a$b" stays
// as written.
func envRefs(env []string) []string {
	if env == nil {
		return nil
	}
	out := make([]string, len(env))
	for i, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if m := EnvRef.FindStringSubmatch(v); ok && m != nil {
			v = os.Getenv(m[1] + m[2])
		}
		out[i] = k + "=" + v
		if !ok {
			out[i] = kv
		}
	}
	return out
}

// Resolve is the server list for a workspace, with a session's
// switches and this run's flags on top.
func Resolve(dir, workspace, raglitHome string, base []Server, sess map[string]SessionServer, flags map[string]bool) ([]Server, error) {
	cat, err := loadCatalog(dir, base)
	if err != nil {
		return nil, err
	}
	defVia, err := LoadDefaultVia()
	if err != nil { // a bad value must not take every server down with it
		warnOnce(err.Error())
		defVia = ViaDirect
	}
	out := make([]Server, 0, len(cat))
	for _, e := range cat {
		var flag *bool
		if v, ok := flags[e.def.ID]; ok {
			flag = &v
		}
		s, ok, err := e.resolve(sess[e.def.ID], flag, workspace, raglitHome, defVia)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// ToolState is whether a tool is on, the scope that decided, and the
// deny-list entry hiding it when a pattern did: a single-tool switch at
// session, then project scope; then a project deny-list, which sets every
// tool; then the global one.
func (s Server) ToolState(name string) (on bool, from, by string) {
	if v, ok := s.SessionTools[name]; ok {
		return v, ScopeSession, ""
	}
	if v, ok := s.ProjectTools[name]; ok {
		return v, ScopeProject, ""
	}
	if s.ProjectDisable != nil {
		by = HiddenBy(name, s.ProjectDisable)
		return by == "", ScopeProject, by
	}
	by = HiddenBy(name, s.Disable)
	return by == "", ScopeGlobal, by
}

// ToolScopes is a tool's switch at each scope: "on", "off" or "" (unset).
// Global is never unset.
func (s Server) ToolScopes(name string) (global, project, session string) {
	global = OnOffWord(HiddenBy(name, s.Disable) == "")
	if v, ok := s.ProjectTools[name]; ok {
		project = OnOffWord(v)
	} else if s.ProjectDisable != nil {
		project = OnOffWord(HiddenBy(name, s.ProjectDisable) == "")
	}
	if v, ok := s.SessionTools[name]; ok {
		session = OnOffWord(v)
	}
	return
}

// OnOffWord is b as a switch reads.
func OnOffWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// UnionAttach is the scopes' attachments, each once, in scope order.
func UnionAttach(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lists {
		for _, a := range l {
			if a != "" && !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// effectiveIndexes is the indexes switched on (on) or off (!on): for each
// index a scope names, the nearest scope decides — session, then project,
// then global.
func effectiveIndexes(sc Scopes, on bool) []string {
	layers := []struct{ attach, detach []string }{
		{sc.AttachSession, sc.DetachSession}, {sc.AttachProject, sc.DetachProject}, {sc.AttachGlobal, sc.DetachGlobal},
	}
	var out []string
	for _, name := range UnionAttach(sc.AttachGlobal, sc.AttachProject, sc.AttachSession,
		sc.DetachGlobal, sc.DetachProject, sc.DetachSession) {
		for _, l := range layers {
			if slices.Contains(l.attach, name) {
				if on {
					out = append(out, name)
				}
				break
			}
			if slices.Contains(l.detach, name) {
				if !on {
					out = append(out, name)
				}
				break
			}
		}
	}
	return out
}

// EnvRef matches a bearer that is only a variable reference: "$NAME" or
// "${NAME}".
var EnvRef = regexp.MustCompile(`^\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))$`)
