package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// ServerScopes is one server's switches at each scope, and which one
// decided. nil and "" are unset.
type ServerScopes struct {
	Global, Project, Session *bool
	OnFrom                   string // the scope that decided Autostart
	ViaGlobal, ViaProject    string // "", ViaDirect, ViaShell
	ViaSession               string
	ViaFrom                  string // the scope that decided Via; "" for a built-in
}

// GlobalOn is the global switch as shown: unset is off, except for a server
// placed in the shell, which the shell always hosted.
func (s ServerScopes) GlobalOn(via string) bool {
	if s.Global != nil {
		return *s.Global
	}
	return via == ViaShell
}

// SessionServer is one server's switches in a session's sidecar.
type SessionServer struct {
	On    *bool           `json:"on,omitempty"`
	Via   string          `json:"via,omitempty"`
	Tools map[string]bool `json:"tools,omitempty"`
}

func (s SessionServer) empty() bool { return s.On == nil && s.Via == "" && len(s.Tools) == 0 }

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
		if !s.empty() {
			out[id] = s
		}
	}
	return out
}

func saveSessionServers(sessionFile string, sw map[string]SessionServer) (string, error) {
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
	return p, writeFileAtomic(p, append(b, '\n'), 0o600)
}

// catalogEntry is one server across the layers: def is every layer merged
// (what the server IS), global and project the switch values of those
// layers alone.
type catalogEntry struct {
	def, global, project ServerSpec
}

// specOf is a Go-level Server as a global-layer spec.
func specOf(s Server) ServerSpec {
	auto := s.Autostart
	return ServerSpec{ID: s.ID, Command: s.Command, Args: s.Args, URL: s.URL, Headers: s.Headers, Bearer: s.Bearer,
		Env: s.Env, Timeout: s.Timeout, Autostart: &auto, Disable: s.Disable, Via: s.Via, OAuth: s.OAuth}
}

// loadCatalog merges base (the built-ins, or a caller's Go-level servers),
// $DUN_HOME/mcp.json and dir's project files, keeping the global and project
// switches apart.
func loadCatalog(dir string, base []Server) ([]catalogEntry, error) {
	idx := map[string]int{}
	var out []catalogEntry
	put := func(spec ServerSpec, project bool) {
		i, ok := idx[spec.ID]
		if !ok {
			i = len(out)
			idx[spec.ID] = i
			out = append(out, catalogEntry{def: ServerSpec{ID: spec.ID}, global: ServerSpec{ID: spec.ID}, project: ServerSpec{ID: spec.ID}})
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
		f, err := readServersFile(path)
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
func switchesOnly(s ServerSpec) bool {
	return s.Command == "" && s.URL == "" && s.Args == nil && s.Env == nil && s.Headers == nil &&
		s.Bearer == "" && s.OAuth == nil && s.Timeout == 0 &&
		(s.Autostart != nil || s.Via != "" || s.Disable != nil || s.Tools != nil)
}

// resolve turns a catalog entry and this session's switches into the Server
// it runs as. ok=false: the server is not in this workspace (disabled, or
// only switches name it).
func (e catalogEntry) resolve(sess SessionServer, flag *bool, workspace, raglitHome string) (Server, bool, error) {
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
	sc := ServerScopes{Global: e.global.Autostart, Project: e.project.Autostart, Session: sess.On}
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
	default:
		sc.ViaFrom = ScopeGlobal
	}
	if builtinServer(s.ID) {
		sc.ViaFrom = ""
		if via == ViaShell {
			return Server{}, false, fmt.Errorf("dun: server %q is built in and cannot be placed in the shell (remove its \"via\")", s.ID)
		}
	}
	if via == ViaDirect {
		via = ""
	}
	if err := checkOAuth(s, via); err != nil {
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
		Env:            expandPlaceholders(s.Env, workspace, raglitHome),
		Timeout:        s.Timeout,
		Autostart:      on,
		Disable:        e.global.Disable,
		ProjectDisable: e.project.Disable,
		ProjectTools:   e.project.Tools,
		SessionTools:   sess.Tools,
		Via:            via,
		Bearer:         s.Bearer,
		OAuth:          s.OAuth,
		Scopes:         sc,
	}, true, nil
}

// resolveServers is the server list for a workspace, with a session's
// switches and this run's flags on top.
func resolveServers(dir, workspace, raglitHome string, base []Server, sess map[string]SessionServer, flags map[string]bool) ([]Server, error) {
	cat, err := loadCatalog(dir, base)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(cat))
	for _, e := range cat {
		var flag *bool
		if v, ok := flags[e.def.ID]; ok {
			flag = &v
		}
		s, ok, err := e.resolve(sess[e.def.ID], flag, workspace, raglitHome)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// toolState is whether a tool is on, the scope that decided, and the
// deny-list entry hiding it when a pattern did: a single-tool switch at
// session, then project scope; then a project deny-list, which sets every
// tool; then the global one.
func (s Server) toolState(name string) (on bool, from, by string) {
	if v, ok := s.SessionTools[name]; ok {
		return v, ScopeSession, ""
	}
	if v, ok := s.ProjectTools[name]; ok {
		return v, ScopeProject, ""
	}
	if s.ProjectDisable != nil {
		by = hiddenBy(name, s.ProjectDisable)
		return by == "", ScopeProject, by
	}
	by = hiddenBy(name, s.Disable)
	return by == "", ScopeGlobal, by
}

// toolScopes is a tool's switch at each scope: "on", "off" or "" (unset).
// Global is never unset.
func (s Server) toolScopes(name string) (global, project, session string) {
	global = onOffWord(hiddenBy(name, s.Disable) == "")
	if v, ok := s.ProjectTools[name]; ok {
		project = onOffWord(v)
	} else if s.ProjectDisable != nil {
		project = onOffWord(hiddenBy(name, s.ProjectDisable) == "")
	}
	if v, ok := s.SessionTools[name]; ok {
		session = onOffWord(v)
	}
	return
}

func onOffWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// baseServers is what the files layer over: the caller's Go-level servers,
// or the built-ins.
func (h *Harness) baseServers() []Server {
	if h.cfg.Servers != nil {
		return h.cfg.Servers
	}
	return DefaultServers(h.cfg.Workspace, h.cfg.RaglitHome)
}

// resolveSpecs re-reads every layer.
func (h *Harness) resolveSpecs() ([]Server, error) {
	h.srvMu.Lock()
	sess := make(map[string]SessionServer, len(h.sessSw))
	for id, s := range h.sessSw {
		sess[id] = s
	}
	h.srvMu.Unlock()
	return resolveServers(h.configDir(), h.cfg.Workspace, h.cfg.RaglitHome, h.baseServers(), sess, h.cfg.AutostartOverride)
}

// followServers is the turn boundary's re-read: a global or project change
// made elsewhere (another session, a hand edit) reaches this session. What
// starts, starts in the background; the tools join at the next rebuild.
func (h *Harness) followServers(ctx context.Context) {
	if err := h.reloadServers(ctx, false, nil); err != nil {
		log.Printf("dun: server config not reloaded: %v", err)
	}
}

// reloadServers re-resolves the servers and applies the difference: a server
// whose resolved state turned off is stopped, one that turned on is started
// (waiting for it when wait), a change to what the shell hosts restarts the
// shell, and a changed tool switch rebuilds the tool set. restart names
// servers to bounce though their state did not change (a replaced
// definition). A child shares its parent's servers and does none of this.
func (h *Harness) reloadServers(ctx context.Context, wait bool, restart map[string]bool) error {
	if h.parent != nil || h.mgr == nil {
		return nil
	}
	h.reconMu.Lock()
	defer h.reconMu.Unlock()
	next, err := h.resolveSpecs()
	if err != nil {
		return err
	}
	h.srvMu.Lock()
	prev := h.specs
	h.specs = next
	h.srvMu.Unlock()
	if reflect.DeepEqual(prev, next) && len(restart) == 0 {
		return nil
	}
	old := map[string]Server{}
	for _, s := range prev {
		old[s.ID] = s
	}
	now := map[string]Server{}
	for _, s := range next {
		now[s.ID] = s
	}
	direct := func(s Server, ok bool) bool { return ok && s.Autostart && s.Via != ViaShell }
	hostedKey := func(list []Server) string {
		var b strings.Builder
		for _, s := range list {
			if s.Via == ViaShell && s.Autostart {
				j, _ := json.Marshal(s)
				b.Write(j)
				if restart[s.ID] {
					b.WriteString("!")
				}
			}
		}
		return b.String()
	}
	// Stops first, so the shell restart and the starts see the new set.
	for id, o := range old {
		n, ok := now[id]
		if h.mgr.ServerStarted(id) && direct(o, true) && (!direct(n, ok) || restart[id]) {
			h.mgr.StopServer(id)
		}
	}
	var firstErr error
	if hostedKey(prev) != hostedKey(next) || restart[ServerShell] {
		if _, err := h.restartShell(ctx); err != nil {
			firstErr = fmt.Errorf("the shell did not come back: %w", err)
		}
	}
	for _, n := range next {
		o, had := old[n.ID]
		if !direct(n, true) || h.mgr.ServerStarted(n.ID) || (direct(o, had) && !restart[n.ID]) {
			continue
		}
		if wait {
			if err := h.startServer(ctx, n); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		go func() {
			if err := h.startServer(ctx, n); err != nil {
				log.Printf("dun: %s did not start: %v", n.ID, err)
			}
		}()
	}
	h.applyTools()
	return firstErr
}

// knownServer finds id among the resolved servers.
func (h *Harness) knownServer(id string) (Server, error) {
	h.srvMu.Lock()
	s, ok := h.spec(id)
	ids := ServerIDs(h.specs)
	h.srvMu.Unlock()
	if !ok {
		return Server{}, fmt.Errorf("no server %q (have %s)", id, strings.Join(ids, ", "))
	}
	return s, nil
}

// editSession changes this session's switches for id and saves them. It
// returns the sidecar written, "" for an in-memory session.
func (h *Harness) editSession(id string, edit func(*SessionServer)) (string, error) {
	h.srvMu.Lock()
	if h.sessSw == nil {
		h.sessSw = map[string]SessionServer{}
	}
	s := h.sessSw[id]
	edit(&s)
	if s.empty() {
		delete(h.sessSw, id)
	} else {
		h.sessSw[id] = s
	}
	cp := make(map[string]SessionServer, len(h.sessSw))
	for k, v := range h.sessSw {
		cp[k] = v
	}
	h.srvMu.Unlock()
	return saveSessionServers(h.cfg.SessionFile, cp)
}

// writeScope applies edit to id's entry at scope and returns the file written.
func (h *Harness) writeScope(id, scope string, global, project func(*ServerSpec), session func(*SessionServer)) (string, error) {
	switch scope {
	case ScopeGlobal:
		return UpsertUserServer(id, global)
	case ScopeProject:
		return updateLocalServer(h.configDir(), id, project)
	case ScopeSession:
		return h.editSession(id, session)
	}
	return "", fmt.Errorf("scope %q: want global, project or session", scope)
}

// SetServerOn sets id's on/off switch at scope; nil unsets it (not at
// global, which is on or off). The change applies now: the server starts
// (and is waited for) or stops. It returns the file written.
func (h *Harness) SetServerOn(ctx context.Context, id, scope string, on *bool) (string, error) {
	if _, err := h.knownServer(id); err != nil {
		return "", err
	}
	if on == nil && scope == ScopeGlobal {
		return "", fmt.Errorf("the global switch is on or off — it has no unset")
	}
	path, err := h.writeScope(id, scope,
		func(s *ServerSpec) { s.Autostart = on },
		func(s *ServerSpec) { s.Autostart = on },
		func(s *SessionServer) { s.On = on })
	if err != nil {
		return "", err
	}
	if err := h.reloadServers(ctx, true, nil); err != nil {
		return path, err
	}
	// Asked for by name: a server that is on but not up (it failed before)
	// is tried again.
	if s, err := h.knownServer(id); err == nil && s.Autostart && s.Via != ViaShell && !h.mgr.ServerStarted(id) {
		return path, h.startServer(ctx, s)
	}
	return path, nil
}

// SetServerVia places id at scope: ViaShell, ViaDirect, or "" to unset (not
// at global, where unset is direct). It returns the file written.
func (h *Harness) SetServerVia(ctx context.Context, id, scope, via string) (string, error) {
	s, err := h.knownServer(id)
	if err != nil {
		return "", err
	}
	if builtinServer(id) {
		return "", fmt.Errorf("%q is a built-in server and cannot be placed in the shell", id)
	}
	switch via {
	case "":
		if scope == ScopeGlobal {
			return "", fmt.Errorf("the global placement is shell or direct — it has no unset")
		}
	case ViaDirect, ViaShell:
	default:
		return "", fmt.Errorf("via %q: want %q or %q", via, ViaDirect, ViaShell)
	}
	if via == ViaShell && s.OAuth != nil {
		return "", fmt.Errorf("%q logs in with oauth, and dun holds its token: it cannot run in the shell", id)
	}
	stored := via
	if scope == ScopeGlobal && via == ViaDirect {
		stored = "" // the global layer's direct is its absence
	}
	path, err := h.writeScope(id, scope,
		func(s *ServerSpec) { s.Via = stored },
		func(s *ServerSpec) { s.Via = stored },
		func(s *SessionServer) { s.Via = stored })
	if err != nil {
		return "", err
	}
	return path, h.reloadServers(ctx, true, nil)
}

// SetToolOn turns one tool of a running, direct server on or off at scope;
// nil unsets it (project or session). The global switch edits the server's
// deny-list in mcp.json. It reports the file written and whether the switch
// changed; the model sees the change from its next turn.
func (h *Harness) SetToolOn(id, tool, scope string, on *bool) (string, bool, error) {
	tools, err := h.ServerTools(id)
	if err != nil {
		return "", false, err
	}
	i := -1
	for j, t := range tools {
		if t.Name == tool {
			i = j
		}
	}
	if i < 0 {
		return "", false, fmt.Errorf("%s has no tool %q", id, tool)
	}
	s, _ := h.knownServer(id)
	g, p, ss := s.toolScopes(tool)
	want := ""
	if on != nil {
		want = onOffWord(*on)
	}
	var path string
	switch scope {
	case ScopeGlobal:
		if on == nil {
			return "", false, fmt.Errorf("the global switch is on or off — it has no unset")
		}
		if g == want {
			return "", false, nil
		}
		next := []string{} // non-nil: an emptied list must still override (ServerSpec.MarshalJSON)
		if *on {
			for _, pat := range s.Disable {
				if pat != tool {
					next = append(next, pat)
				}
			}
			if by := hiddenBy(tool, next); by != "" {
				return "", false, fmt.Errorf("%s is hidden by the pattern %q — narrow or remove it in the server's \"disable\" in %s", tool, by, UserServersFile())
			}
		} else {
			next = append(append(next, s.Disable...), tool)
		}
		path, err = UpsertUserServer(id, func(sp *ServerSpec) { sp.Disable = next })
	case ScopeProject, ScopeSession:
		cur := p
		if scope == ScopeSession {
			cur = ss
		}
		if _, set := s.ProjectTools[tool]; scope == ScopeProject && !set && s.ProjectDisable != nil && on == nil {
			return "", false, fmt.Errorf("%s is set by the project's \"disable\" list, which unset does not edit", tool)
		}
		if cur == want {
			return "", false, nil
		}
		edit := func(m map[string]bool) map[string]bool {
			out := map[string]bool{}
			for k, v := range m {
				out[k] = v
			}
			if on == nil {
				delete(out, tool)
			} else {
				out[tool] = *on
			}
			if len(out) == 0 {
				return nil
			}
			return out
		}
		path, err = h.writeScope(id, scope, nil,
			func(sp *ServerSpec) { sp.Tools = edit(sp.Tools) },
			func(sp *SessionServer) { sp.Tools = edit(sp.Tools) })
	default:
		return "", false, fmt.Errorf("scope %q: want global, project or session", scope)
	}
	if err != nil {
		return "", false, err
	}
	return path, true, h.reloadServers(context.Background(), true, nil)
}
