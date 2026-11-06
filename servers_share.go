package dun

import (
	"context"
	"log"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// The engine's half of dund's gateway (gateway.go): a shared server is
// started as a remote one at the gateway's endpoint, with this client's
// bearer and no OAuth of the engine's own: the gateway's upstream logs in
// through dund's broker.

// shareable reports whether a standalone server s is reached through dund's
// gateway (sharedInstance).
func (h *Harness) shareable(s Server) bool { return s.Via != ViaShell && h.sharedInstance(s) }

// sharedInstance reports whether s is one connection in dund: under dund,
// not built in, instance shared, and no host hook (an extension's hook
// calls back into the engine that started it). A child's servers are its
// parent's.
//
// A child starts only its own instances (childOwn): an agent one is its own
// key in dund, a shared one re-resolved for its own tree another key. A
// session's own agent instance is the engine's, as session is.
func (h *Harness) sharedInstance(s Server) bool {
	if h.cfg.Gateway == nil || builtinServer(s.ID) {
		return false
	}
	if s.Instance != InstanceShared && (s.Instance != InstanceAgent || h.parent == nil) {
		return false
	}
	if e, ok := h.extension(s.ID); ok && len(e.HostHooks) > 0 {
		return false
	}
	return true
}

// shareSpec is s as this engine resolved it: the key of its upstream.
func shareSpec(s Server, command string, args, env []string, timeout time.Duration) ShareSpec {
	sp := ShareSpec{ID: s.ID, Timeout: int(timeout / time.Second)}
	if s.URL != "" {
		sp.URL, sp.Headers = s.URL, maps.Clone(s.Headers)
		if s.OAuth != nil {
			o := *s.OAuth
			o.ClientSecret = (&oauthClient{spec: o}).clientSecret() // read here, where the env is
			sp.OAuth = &o
		}
		return sp
	}
	sp.Command, sp.Args, sp.Env = command, slices.Clone(args), slices.Clone(env)
	return sp
}

// shareStart asks the gateway for s's endpoint and points mcfg at it.
func (h *Harness) shareStart(ctx context.Context, s Server, mcfg *mcpmgr.MCPConfig, timeout time.Duration) error {
	req := ShareRequest{Spec: shareSpec(s, mcfg.Command, mcfg.Args, mcfg.Env, timeout), Instance: s.Instance}
	if s.Instance == InstanceAgent {
		req.Agent = h.agentPath()
	}
	g, err := h.cfg.Gateway.Share(ctx, req)
	if err != nil {
		return err
	}
	mcfg.Command, mcfg.Args, mcfg.Env = "", nil, nil
	mcfg.URL, mcfg.Headers = g.URL, map[string]string{"Authorization": "Bearer " + g.Bearer}
	mcfg.Authorization, mcfg.OnUnauthorized = nil, nil
	h.srvMu.Lock()
	if h.shared == nil {
		h.shared = map[string]ShareGrant{}
	}
	h.shared[s.ID] = g
	h.srvMu.Unlock()
	h.noteServer(s.ID, "shared: one connection in dund for every session (/mcp instance %s session for this session's own)", s.ID)
	return nil
}

// unshare forgets id's grant, giving its bearer back when the client
// never closed it (a failed start).
func (h *Harness) unshare(id string, release bool) {
	h.srvMu.Lock()
	g, ok := h.shared[id]
	delete(h.shared, id)
	h.srvMu.Unlock()
	if ok && release && h.cfg.Gateway != nil {
		if err := h.cfg.Gateway.Release(context.Background(), g.Bearer); err != nil {
			log.Printf("dun: %s: the gateway's client not released: %v", id, err)
		}
	}
}

// shareHosted asks the gateway for an endpoint for each scripted server
// that is shared, before the shell starts: its shell-mcp.json entry is then
// the endpoint and its bearer, with no auth_cmd. The last shell's grants
// are given back first (that shell is stopped). One whose share fails is
// hosted as before, the shell's own connection.
func (h *Harness) shareHosted(ctx context.Context, shell string, phase *startPhase) {
	h.srvMu.Lock()
	old := h.shellShares
	h.shellShares = nil
	hosted := h.hostedSpecs()
	h.srvMu.Unlock()
	if h.cfg.Gateway == nil {
		return
	}
	for _, g := range old {
		_ = h.cfg.Gateway.Release(ctx, g.Bearer)
	}
	next := map[string]ShareGrant{}
	for _, s := range hosted {
		if !h.sharedInstance(s) {
			continue
		}
		command := s.Command
		if command != "" {
			var err error
			if command, err = FindCommand(command); err != nil {
				continue // the shell says it did not connect
			}
		}
		timeout := time.Duration(s.Timeout) * time.Second
		if timeout <= 0 {
			timeout = 90 * time.Second
		}
		phase.set("asking dund for the shared connection to " + s.ID)
		g, err := h.cfg.Gateway.Share(ctx, ShareRequest{Spec: shareSpec(s, command, s.Args, s.Env, timeout), Instance: s.Instance})
		if err != nil {
			h.noteServer(shell, "%s: not shared, the shell connects it itself: %s", s.ID, oneLine(err.Error(), 300))
			continue
		}
		next[s.ID] = g
		h.noteServer(s.ID, "scripted, shared: the shell reaches dund's one connection")
	}
	h.srvMu.Lock()
	h.shellShares = next
	h.srvMu.Unlock()
}

// GatewayNotify hands an upstream's notification (mcp_notify) to the
// servers this engine reaches through it, as if each had sent it.
func (h *Harness) GatewayNotify(key, method string, params map[string]any) {
	h.srvMu.Lock()
	var ids []string
	for id, g := range h.shared {
		if g.Key == key {
			ids = append(ids, id)
		}
	}
	h.srvMu.Unlock()
	for _, id := range ids {
		h.noteServerNotification(mcpmgr.Notification{ServerID: id, Method: method, Params: params})
	}
	h.agMu.Lock()
	var kids []*Harness
	for _, sa := range h.agents {
		sa.mu.Lock()
		if sa.h != nil {
			kids = append(kids, sa.h)
		}
		sa.mu.Unlock()
	}
	h.agMu.Unlock()
	for _, k := range kids { // a child's own instance through the gateway
		k.GatewayNotify(key, method, params)
	}
}

// agentPath is this harness's place among its session's sub-agents: "" for
// the session, "2" for its second child, "2.1" for that one's first.
func (h *Harness) agentPath() string {
	if h.parent == nil {
		return ""
	}
	if p := h.parent.agentPath(); p != "" {
		return p + "." + strconv.Itoa(h.cfg.AgentID)
	}
	return strconv.Itoa(h.cfg.AgentID)
}

// mcpTools is every tool this harness's servers discovered: a child's own
// instances' in place of its parent's for the same ids.
func (h *Harness) mcpTools() []mcpmgr.MCPTool {
	all := h.mgr.GetTools()
	if h.ownMgr == nil {
		return all
	}
	out := slices.DeleteFunc(all, func(t mcpmgr.MCPTool) bool { return h.own[t.ServerID] })
	return append(out, h.ownMgr.GetTools()...)
}

// mgrFor is the manager server id runs in: a child's own instance in its
// own, everything else in mgr (a child's is its parent's).
func (h *Harness) mgrFor(id string) *mcpmgr.Manager {
	if h.ownMgr != nil && h.own[id] {
		return h.ownMgr
	}
	return h.mgr
}

// childOwn is which of a child's servers are its own instances, as the
// child runs them: each standalone, non-built-in server with instance agent,
// and, for a child isolated in a tree of its own, each server keyed on
// {{workspace}} (poly-lsp included), re-resolved against that tree. Every
// other server stays its parent's. specs is the parent's list; the result
// is the child's, with its own ones replaced.
func childOwn(parent *Harness, cfg Config, specs []Server) ([]Server, map[string]bool) {
	isolated := cfg.Workspace != parent.cfg.Workspace
	var mine map[string]Server
	if isolated {
		root := parent
		for root.parent != nil {
			root = root.parent
		}
		base := root.cfg.Servers
		if base == nil {
			base = DefaultServers(cfg.Workspace, cfg.RaglitHome)
		}
		root.srvMu.Lock()
		sess := maps.Clone(root.sessSw)
		root.srvMu.Unlock()
		re, err := resolveServers(cfg.ConfigDir, cfg.Workspace, cfg.RaglitHome, extServers(base, root.exts), sess, cfg.AutostartOverride)
		if err != nil {
			log.Printf("dun: a child's servers for its own tree: %v; it uses its parent's", err)
		}
		mine = map[string]Server{}
		for _, s := range re {
			mine[s.ID] = s
		}
	}
	out := slices.Clone(specs)
	own := map[string]bool{}
	for i, s := range out {
		if !s.Autostart || s.Via == ViaShell {
			continue
		}
		if r, ok := mine[s.ID]; ok && s.PerWorkspace {
			out[i], own[s.ID] = r, true
			continue
		}
		if s.Instance == InstanceAgent && !builtinServer(s.ID) {
			own[s.ID] = true
		}
	}
	return out, own
}

// regainShared answers a call that failed because the gateway has no such
// server (its upstream exited, or dund dropped this client): the endpoint
// is asked for again and the server restarted, once. It reports whether
// the call is worth repeating.
func (h *Harness) regainShared(ctx context.Context, id string, err error) bool {
	h.srvMu.Lock()
	g, ok := h.shared[id]
	s, known := h.spec(id)
	h.srvMu.Unlock()
	if !ok || !known || err == nil || !strings.Contains(err.Error(), "HTTP 404 from "+g.URL) {
		return false
	}
	h.noteServer(id, "the gateway no longer had it: asking again")
	h.mgrFor(id).StopServer(id)
	h.unshare(id, false)
	return h.startServer(ctx, s) == nil
}
