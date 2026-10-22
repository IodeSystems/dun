package dun

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/agentkit/mcpmgr/mcpauth"
)

// The harness side of OAuth servers: one oauthClient per server, the
// /mcp login and /mcp logout commands, and the auth column of /mcp.

// oauthFor is s's login, or nil when s does not log in with OAuth. The client
// is kept across restarts; a changed oauth block replaces it.
func (h *Harness) oauthFor(s Server) *oauthClient {
	if s.OAuth == nil || s.URL == "" {
		return nil
	}
	h.oauthMu.Lock()
	defer h.oauthMu.Unlock()
	if oc := h.oauth[s.ID]; oc != nil && oc.url == s.URL && reflect.DeepEqual(oc.spec, *s.OAuth) {
		return oc
	}
	if h.oauth == nil {
		h.oauth = map[string]*oauthClient{}
	}
	oc := newOAuthClient(s, h.cfg.OnLogin, h.cfg.OpenBrowser)
	h.oauth[s.ID] = oc
	return oc
}

// HasOAuth reports whether server id logs in with OAuth — whose start may
// wait on a person in a browser.
func (h *Harness) HasOAuth(id string) bool {
	h.srvMu.Lock()
	s, ok := h.spec(id)
	h.srvMu.Unlock()
	return ok && s.OAuth != nil
}

// authState is the /mcp listing's auth column for s.
func (h *Harness) authState(s Server) string {
	switch {
	case s.URL == "":
		return ""
	case s.OAuth != nil:
		return h.oauthFor(s).status().String()
	case s.Bearer != "":
		return "static bearer"
	}
	for k := range s.Headers {
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "cf-access-token") {
			return "static header"
		}
	}
	return "none"
}

// LoginServer runs the interactive login for an OAuth server now, whatever
// its stored token, and starts the server if it is not running. The URL to
// open goes out through Config.OnLogin while this waits.
func (h *Harness) LoginServer(ctx context.Context, id string) (string, error) {
	h.srvMu.Lock()
	s, ok := h.spec(id)
	h.srvMu.Unlock()
	if !ok {
		return "", fmt.Errorf("no server %q", id)
	}
	oc := h.oauthFor(s)
	if oc == nil {
		return "", fmt.Errorf("%s has no oauth block — add one (client_id, …) to its entry in %s", id, UserServersFile())
	}
	if err := oc.Login(ctx, ""); err != nil {
		return "", err
	}
	msg := oc.status().String()
	if h.mgr.ServerStarted(id) {
		return msg, nil // its next request sends the new token
	}
	if err := h.startServer(ctx, s); err != nil {
		return msg, fmt.Errorf("logged in, but it did not start: %w", err)
	}
	return msg, nil
}

// LogoutServer deletes an OAuth server's stored token, revoking it when the
// server can, and stops the server: left running, its next request would
// open a login again.
func (h *Harness) LogoutServer(ctx context.Context, id string) (string, error) {
	h.srvMu.Lock()
	s, ok := h.spec(id)
	h.srvMu.Unlock()
	if !ok {
		return "", fmt.Errorf("no server %q", id)
	}
	oc := h.oauthFor(s)
	if oc == nil {
		return "", fmt.Errorf("%s does not log in with oauth", id)
	}
	revoked, err := oc.Logout(ctx)
	if err != nil {
		return "", err
	}
	msg := "logged out (token deleted"
	if revoked {
		msg += " and revoked"
	}
	msg += ")"
	if h.mgr.ServerStarted(id) {
		h.mgr.StopServer(id)
		h.applyTools()
		msg += "; stopped — /mcp login " + id + " to log in again"
	}
	return msg, nil
}

// explainStartErr turns a remote server's failed start into what to do about
// it: a login gate in front of it (mcpgate.go), or a server that wants an
// OAuth login and has none configured.
func explainStartErr(ctx context.Context, s Server, err error) error {
	if s.URL == "" {
		return err
	}
	var uerr *mcpmgr.UnauthorizedError
	if errors.As(err, &uerr) {
		if s.OAuth != nil {
			return err
		}
		if ch, ok := mcpauth.BearerChallenge(uerr.WWWAuthenticate); ok && ch.Params["resource_metadata"] != "" {
			return fmt.Errorf("%w\n%s wants an OAuth login: add \"oauth\": {\"client_id\": \"…\"} to its entry (its metadata: %s)",
				err, s.ID, ch.Params["resource_metadata"])
		}
		return err
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if p, perr := probeMCP(pctx, s.URL); perr == nil && p.gate != "" {
		return fmt.Errorf("%s is %s", s.ID, p.gate)
	}
	return err
}
