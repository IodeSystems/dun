package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/agentkit/mcpmgr/mcpauth"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/oauth"
)

// The harness side of OAuth servers: one oauth.Client per server, the
// /mcp login and /mcp logout commands, and the auth column of /mcp.

// oauthFor is s's login, or nil when s does not log in with OAuth: dund's
// (Config.OAuthBroker, internal/oauth/oauthbroker.go) when the engine runs under it, else
// the engine's own client. It is kept across restarts; a changed oauth
// block replaces it.
func (h *Harness) oauthFor(s mcpserver.Server) oauth.Source {
	if s.OAuth == nil || s.URL == "" {
		return nil
	}
	h.oauthMu.Lock()
	defer h.oauthMu.Unlock()
	if o := h.oauth[s.ID]; o.src != nil && o.url == s.URL && reflect.DeepEqual(o.spec, *s.OAuth) {
		return o.src
	}
	if h.oauth == nil {
		h.oauth = map[string]oauthEntry{}
	}
	var src oauth.Source
	if h.cfg.OAuthBroker != nil {
		spec := *s.OAuth
		spec.ClientSecret = oauth.ClientSecret(spec) // resolved here, where the env is
		src = oauth.NewBrokered(s.ID, s.URL, spec, h.cfg.OAuthBroker)
	} else {
		oc := oauth.NewClient(s.ID, s.URL, *s.OAuth, h.cfg.OnLogin, h.cfg.OpenBrowser)
		oc.Ended = h.cfg.OnLoginEnd
		src = oc
	}
	h.oauth[s.ID] = oauthEntry{url: s.URL, spec: *s.OAuth, src: src}
	return src
}

// oauthEntry is a server's login and the config it was made for.
type oauthEntry struct {
	url  string
	spec oauth.Spec
	src  oauth.Source
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
func (h *Harness) authState(s mcpserver.Server) string {
	if _, keyed := raglitKeyed(s); keyed { // raglitauth.go
		if loadRaglitToken() == "" {
			return "needs /rag login"
		}
		return "dun token"
	}
	switch {
	case s.URL == "":
		return ""
	case s.OAuth != nil:
		return h.oauthFor(s).Status().String()
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
	var msg string
	if _, keyed := raglitKeyed(s); keyed {
		m, err := h.raglitLogin(ctx, s)
		if err != nil {
			return "", err
		}
		msg = m
	} else {
		oc := h.oauthFor(s)
		if oc == nil {
			return "", fmt.Errorf("%s has no oauth block — add one (client_id, …) to its entry in %s", id, mcpserver.UserServersFile())
		}
		if err := oc.Login(ctx, ""); err != nil {
			return "", err
		}
		msg = oc.Status().String()
	}
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
	var msg string
	if _, keyed := raglitKeyed(s); keyed {
		m, err := h.raglitLogout(ctx, s)
		if err != nil {
			return "", err
		}
		msg = m
	} else {
		oc := h.oauthFor(s)
		if oc == nil {
			return "", fmt.Errorf("%s does not log in with oauth", id)
		}
		revoked, err := oc.Logout(ctx)
		if err != nil {
			return "", err
		}
		msg = "logged out (token deleted"
		if revoked {
			msg += " and revoked"
		}
		msg += ")"
	}
	if h.mgr.ServerStarted(id) {
		h.mgr.StopServer(id)
		h.applyTools()
		msg += "; stopped — /mcp login " + id + " to log in again"
	}
	return msg, nil
}

// explainStartErr turns a remote server's failed start into what to do about
// it: a login gate in front of it (internal/oauth/mcpgate.go), or a server that wants an
// OAuth login and has none configured.
func explainStartErr(ctx context.Context, s mcpserver.Server, err error) error {
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
	if p, perr := oauth.Probe(pctx, s.URL); perr == nil && p.Gate != "" {
		return fmt.Errorf("%s is %s", s.ID, p.Gate)
	}
	return err
}

// wantsOAuth reports whether a remote server with no credentials of its
// own answers an unauthenticated initialize with a 401 Bearer challenge
// naming its protected-resource metadata (RFC 9728): the MCP OAuth flow.
func wantsOAuth(ctx context.Context, s mcpserver.Spec) bool {
	if s.URL == "" || s.Bearer != "" {
		return false
	}
	for k := range s.Headers {
		if strings.EqualFold(k, "Authorization") {
			return false
		}
	}
	p, err := oauth.Probe(ctx, s.URL)
	if err != nil || p.Status != http.StatusUnauthorized {
		return false
	}
	ch, ok := mcpauth.BearerChallenge(p.WWW)
	return ok && ch.Params["resource_metadata"] != ""
}
