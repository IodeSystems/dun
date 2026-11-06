package dun

import (
	"context"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// The engine's half of dund's gateway (gateway.go): a shared server is
// started as a remote one at the gateway's endpoint, with this client's
// bearer and no OAuth of the engine's own: the gateway's upstream logs in
// through dund's broker.

// shareable reports whether s is reached through dund's gateway: under
// dund, not built in, standalone, instance shared, and no host hook (an
// extension's hook calls back into the engine that started it). A child's
// servers are its parent's.
func (h *Harness) shareable(s Server) bool {
	if h.cfg.Gateway == nil || h.parent != nil || builtinServer(s.ID) || s.Via == ViaShell || s.Instance != InstanceShared {
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
	g, err := h.cfg.Gateway.Share(ctx, ShareRequest{Spec: shareSpec(s, mcfg.Command, mcfg.Args, mcfg.Env, timeout), Instance: s.Instance})
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
	h.mgr.StopServer(id)
	h.unshare(id, false)
	return h.startServer(ctx, s) == nil
}
