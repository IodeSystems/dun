package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"slices"
	"strings"

	"github.com/iodesystems/dun/internal/mcpserver"
)

// A session's side of the three switch scopes (internal/mcpserver/scope.go):
// setting a switch, and following a global or project change at a turn.

// baseServers is what the files layer over: the caller's Go-level servers,
// or the built-ins.
func (h *Harness) baseServers() []mcpserver.Server {
	base := h.cfg.Servers
	if base == nil {
		base = mcpserver.DefaultServers(h.cfg.Workspace, h.cfg.RaglitHome)
	}
	return mcpserver.ExtServers(base, h.exts)
}

// resolveSpecs re-reads every layer.
func (h *Harness) resolveSpecs() ([]mcpserver.Server, error) {
	h.srvMu.Lock()
	sess := make(map[string]mcpserver.SessionServer, len(h.sessSw))
	for id, s := range h.sessSw {
		sess[id] = s
	}
	h.srvMu.Unlock()
	servers, err := mcpserver.Resolve(h.configDir(), h.cfg.Workspace, h.cfg.RaglitHome, h.baseServers(), sess, h.cfg.AutostartOverride)
	return withoutDirServers(servers, h.cfg), err
}

// followServers is the turn boundary's re-read: a global or project change
// made elsewhere (another session, a hand edit) reaches this session. What
// starts, starts in the background; the tools join at the next rebuild.
func (h *Harness) followServers(ctx context.Context) {
	if err := h.reloadServers(ctx, false, nil); err != nil {
		log.Printf("dun: server config not reloaded: %v", err)
	}
}

// FollowServersIfIdle re-reads the server layers now when no turn is running,
// and reports whether it did. A running turn is left alone: reconciling can
// restart the shell, which kills an eval in flight, and the next turn re-reads
// at its start (followServers) anyway.
func (h *Harness) FollowServersIfIdle(ctx context.Context) bool {
	if !h.turnMu.TryLock() {
		return false
	}
	h.turnMu.Unlock()
	h.followServers(ctx)
	return true
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
	h.reloads.Add(1)
	defer h.reloads.Add(-1)
	changed, starts, firstErr, err := h.reconcileServers(ctx, restart)
	if err != nil || !changed {
		return err
	}
	// The starts run outside reconMu: a start can take its whole timeout,
	// and a switch changed meanwhile (off, while it starts) must not wait
	// for it. Each is marked starting under the lock, so no other reconcile
	// starts it again.
	for _, n := range starts {
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

// reconcileServers, under reconMu, takes the switches' new server set: it
// stops what is no longer on (and stops the starts of what was switched off
// while starting), restarts the shell when what it hosts changed, and says
// which servers to start, marked starting. changed is false when the set is
// what it was and nothing is to restart: there is nothing to rebuild.
func (h *Harness) reconcileServers(ctx context.Context, restart map[string]bool) (changed bool, starts []mcpserver.Server, shellErr, err error) {
	h.reconMu.Lock()
	defer h.reconMu.Unlock()
	next, err := h.resolveSpecs()
	if err != nil {
		return false, nil, nil, err
	}
	h.srvMu.Lock()
	prev := h.specs
	h.specs = next
	h.srvMu.Unlock()
	if reflect.DeepEqual(prev, next) && len(restart) == 0 {
		return false, nil, nil, nil
	}
	old := map[string]mcpserver.Server{}
	for _, s := range prev {
		old[s.ID] = s
	}
	now := map[string]mcpserver.Server{}
	for _, s := range next {
		now[s.ID] = s
	}
	direct := func(s mcpserver.Server, ok bool) bool { return ok && s.Autostart && s.Via != mcpserver.ViaShell }
	hostedKey := func(list []mcpserver.Server) string {
		var b strings.Builder
		for _, s := range list {
			if s.Via == mcpserver.ViaShell && s.Autostart {
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
		// A changed instance is another connection: the shared one, or this
		// session's own.
		if h.mgr.ServerStarted(id) && direct(o, true) && (!direct(n, ok) || restart[id] || o.Instance != n.Instance) {
			h.mgr.StopServer(id)
			h.noteServer(id, "stopped")
		}
		if direct(o, true) && !direct(n, ok) {
			h.cancelStart(id, errSwitchedOff)
		}
	}
	var firstErr error
	if hostedKey(prev) != hostedKey(next) || restart[mcpserver.ServerShell] {
		if _, err := h.restartShell(ctx); err != nil {
			firstErr = fmt.Errorf("the shell did not come back: %w", err)
		}
	}
	for _, n := range next {
		o, had := old[n.ID]
		if !direct(n, true) || h.mgr.ServerStarted(n.ID) || h.isStarting(n.ID) || (direct(o, had) && !restart[n.ID] && o.Instance == n.Instance) {
			continue
		}
		h.markStarting(n.ID)
		starts = append(starts, n)
	}
	return true, starts, firstErr, nil
}

// knownServer finds id among the resolved servers.
func (h *Harness) knownServer(id string) (mcpserver.Server, error) {
	h.srvMu.Lock()
	s, ok := h.spec(id)
	ids := mcpserver.IDs(h.specs)
	h.srvMu.Unlock()
	if !ok {
		return mcpserver.Server{}, fmt.Errorf("no server %q (have %s)", id, strings.Join(ids, ", "))
	}
	return s, nil
}

// editSession changes this session's switches for id and saves them. It
// returns the sidecar written, "" for an in-memory session.
func (h *Harness) editSession(id string, edit func(*mcpserver.SessionServer)) (string, error) {
	h.srvMu.Lock()
	if h.sessSw == nil {
		h.sessSw = map[string]mcpserver.SessionServer{}
	}
	s := h.sessSw[id]
	edit(&s)
	if s.Empty() {
		delete(h.sessSw, id)
	} else {
		h.sessSw[id] = s
	}
	cp := make(map[string]mcpserver.SessionServer, len(h.sessSw))
	for k, v := range h.sessSw {
		cp[k] = v
	}
	h.srvMu.Unlock()
	return mcpserver.SaveSessionServers(h.cfg.SessionFile, cp)
}

// writeScope applies edit to id's entry at scope and returns the file written.
func (h *Harness) writeScope(id, scope string, global, project func(*mcpserver.Spec), session func(*mcpserver.SessionServer)) (string, error) {
	switch scope {
	case mcpserver.ScopeGlobal:
		return mcpserver.UpsertUserServer(id, global)
	case mcpserver.ScopeProject:
		return mcpserver.UpdateLocalServer(h.configDir(), id, project)
	case mcpserver.ScopeSession:
		return h.editSession(id, session)
	}
	return "", fmt.Errorf("scope %q: want global, project or session", scope)
}

// SetServerOn sets id's on/off switch at scope; nil unsets it (not at
// global, which is on or off). The change applies now: the server starts
// (and is waited for) or stops. It returns the file written.
func (h *Harness) SetServerOn(ctx context.Context, id, scope string, on *bool) (string, error) {
	if g, ok := builtinID(id); ok { // a group of dun's own tools (builtins.go)
		if on == nil && scope == mcpserver.ScopeGlobal {
			return "", fmt.Errorf("the global switch is on or off — it has no unset")
		}
		return h.setBuiltin(g, "", scope, on)
	}
	if _, err := h.knownServer(id); err != nil {
		return "", err
	}
	if on == nil && scope == mcpserver.ScopeGlobal {
		return "", fmt.Errorf("the global switch is on or off — it has no unset")
	}
	path, err := h.writeScope(id, scope,
		func(s *mcpserver.Spec) { s.Autostart = on },
		func(s *mcpserver.Spec) { s.Autostart = on },
		func(s *mcpserver.SessionServer) { s.On = on })
	if err != nil {
		return "", err
	}
	if err := h.reloadServers(ctx, true, nil); err != nil {
		return path, err
	}
	// Asked for by name: a server that is on but not up (it failed before)
	// is tried again.
	if s, err := h.knownServer(id); err == nil && s.Autostart && s.Via != mcpserver.ViaShell && !h.mgr.ServerStarted(id) && !h.isStarting(id) {
		return path, h.startServer(ctx, s)
	}
	return path, nil
}

// SetServerVia places id at scope: ViaShell, ViaDirect, or "" to unset (not
// at global, where unset follows the default mode, SetDefaultMode). It returns the file written.
func (h *Harness) SetServerVia(ctx context.Context, id, scope, via string) (string, error) {
	if _, ok := builtinID(id); ok {
		return "", fmt.Errorf("%s is dun's own: it has no mode", id)
	}
	if _, err := h.knownServer(id); err != nil {
		return "", err
	}
	if mcpserver.IsBuiltin(id) {
		return "", fmt.Errorf("%q is a built-in server: it is always standalone", id)
	}
	switch via {
	case "": // unset: the next scope down, at global the default mode
	case mcpserver.ViaDirect, mcpserver.ViaShell:
	default:
		return "", fmt.Errorf("mode %q: want standalone or scripted", via)
	}
	if s, _ := h.knownServer(id); via == mcpserver.ViaShell && s.Instance == mcpserver.InstanceAgent {
		return "", fmt.Errorf("%s has instance agent: a scripted server runs in the session's shell, which a sub-agent shares — set its instance to session or shared first", id)
	}
	// Global standalone is stored as "direct", not as absence: with a
	// scripted default (SetDefaultVia) absence means scripted.
	path, err := h.writeScope(id, scope,
		func(s *mcpserver.Spec) { s.Via = via },
		func(s *mcpserver.Spec) { s.Via = via },
		func(s *mcpserver.SessionServer) { s.Via = via })
	if err != nil {
		return "", err
	}
	return path, h.reloadServers(ctx, true, nil)
}

// SetDefaultMode sets the mode of a server no scope sets one for: ViaShell or
// ViaDirect. It returns the file written (mcp.json, every workspace).
func (h *Harness) SetDefaultMode(ctx context.Context, via string) (string, error) {
	path, err := mcpserver.SetDefaultVia(via)
	if err != nil {
		return "", err
	}
	return path, h.reloadServers(ctx, true, nil)
}

// SetServerInstance sets id's instance at scope: InstanceShared,
// InstanceSession, InstanceAgent, or "" to unset (not at global, where unset
// is shared). It returns the file written.
func (h *Harness) SetServerInstance(ctx context.Context, id, scope, instance string) (string, error) {
	if _, ok := builtinID(id); ok {
		return "", fmt.Errorf("%s is dun's own: it has no instance", id)
	}
	if fixed, why, ok := mcpserver.BuiltinInstance(id); ok {
		return "", fmt.Errorf("%q is a built-in server: its instance is fixed, %s (%s)", id, fixed, why)
	}
	s, err := h.knownServer(id)
	if err != nil {
		return "", err
	}
	switch instance {
	case "":
		if scope == mcpserver.ScopeGlobal {
			return "", fmt.Errorf("the global instance is shared, session or agent — it has no unset")
		}
	case mcpserver.InstanceShared, mcpserver.InstanceSession:
	case mcpserver.InstanceAgent:
		// Scripted by the default alone gives way: the server stays
		// standalone (ViaFromDefaultAgent). A mode set at a scope refuses.
		if s.Via == mcpserver.ViaShell && s.Scopes.ViaFrom != mcpserver.ViaFromDefault {
			return "", fmt.Errorf("%s is scripted: it runs in the session's shell, which a sub-agent shares — agent is for a standalone server", id)
		}
	default:
		return "", fmt.Errorf("instance %q: want %s, %s or %s", instance, mcpserver.InstanceShared, mcpserver.InstanceSession, mcpserver.InstanceAgent)
	}
	stored := instance
	if scope == mcpserver.ScopeGlobal && instance == mcpserver.InstanceShared {
		stored = "" // the global layer's shared is its absence
	}
	path, err := h.writeScope(id, scope,
		func(s *mcpserver.Spec) { s.Instance = stored },
		func(s *mcpserver.Spec) { s.Instance = stored },
		func(s *mcpserver.SessionServer) { s.Instance = stored })
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
	if g, ok := builtinID(id); ok {
		tools, err := h.builtinTools(g)
		if err != nil {
			return "", false, err
		}
		i := slices.IndexFunc(tools, func(t ToolInfo) bool { return t.Name == tool })
		if i < 0 {
			return "", false, fmt.Errorf("%s has no tool %q", id, tool)
		}
		want := ""
		if on != nil {
			want = mcpserver.OnOffWord(*on)
		}
		cur := map[string]string{mcpserver.ScopeGlobal: tools[i].Global, mcpserver.ScopeProject: tools[i].Project, mcpserver.ScopeSession: tools[i].Session}[scope]
		if cur == want {
			return "", false, nil
		}
		if g == builtinCore {
			return "", false, fmt.Errorf("dun:core is always on")
		}
		path, err := h.setBuiltin(g, tool, scope, on)
		return path, err == nil, err
	}
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
	g, p, ss := s.ToolScopes(tool)
	want := ""
	if on != nil {
		want = mcpserver.OnOffWord(*on)
	}
	var path string
	switch scope {
	case mcpserver.ScopeGlobal:
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
			if by := mcpserver.HiddenBy(tool, next); by != "" {
				return "", false, fmt.Errorf("%s is hidden by the pattern %q — narrow or remove it in the server's \"disable\" in %s", tool, by, mcpserver.UserServersFile())
			}
		} else {
			next = append(append(next, s.Disable...), tool)
		}
		path, err = mcpserver.UpsertUserServer(id, func(sp *mcpserver.Spec) { sp.Disable = next })
	case mcpserver.ScopeProject, mcpserver.ScopeSession:
		cur := p
		if scope == mcpserver.ScopeSession {
			cur = ss
		}
		if _, set := s.ProjectTools[tool]; scope == mcpserver.ScopeProject && !set && s.ProjectDisable != nil && on == nil {
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
			func(sp *mcpserver.Spec) { sp.Tools = edit(sp.Tools) },
			func(sp *mcpserver.SessionServer) { sp.Tools = edit(sp.Tools) })
	default:
		return "", false, fmt.Errorf("scope %q: want global, project or session", scope)
	}
	if err != nil {
		return "", false, err
	}
	return path, true, h.reloadServers(context.Background(), true, nil)
}
