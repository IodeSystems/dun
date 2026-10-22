package dun

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A remote MCP server behind a LOGIN GATE — Google IAP, Cloudflare Access,
// oauth2-proxy, an SSO portal — does not answer like an MCP server. An
// unauthenticated request gets a redirect to a login page (or the page
// itself), and the HTTP client follows it to HTML: what the user then saw
// was a JSON decode error, or a discovery timeout. That is not the MCP OAuth
// flow (no 401, no resource metadata), and nothing in dun can complete a
// browser login for a gate; the fix is configuration. probeMCP recognises
// the gate and says which.

// mcpProbe is how a server answered an unauthenticated initialize.
type mcpProbe struct {
	status int
	www    string // WWW-Authenticate, on a 401
	gate   string // the login-gate diagnosis, or ""
}

// probeMCP POSTs an initialize with no credentials and does NOT follow
// redirects: the redirect is the evidence.
func probeMCP(ctx context.Context, serverURL string) (mcpProbe, error) {
	hc := &http.Client{Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	body := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"dun","version":"probe"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, strings.NewReader(body))
	if err != nil {
		return mcpProbe{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return mcpProbe{}, fmt.Errorf("probe %s: %w", serverURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	p := mcpProbe{status: resp.StatusCode, www: resp.Header.Get("WWW-Authenticate")}
	p.gate = gateDiagnosis(serverURL, resp)
	return p, nil
}

// gateDiagnosis names the gate in front of serverURL, or "" when the answer
// looks like an MCP server's (JSON, an event stream, or a 401 that is not a
// gate's own).
func gateDiagnosis(serverURL string, resp *http.Response) string {
	su, _ := url.Parse(serverURL)
	iapHeader := strings.EqualFold(resp.Header.Get("X-Goog-IAP-Generated-Response"), "true")
	switch {
	case resp.StatusCode/100 == 3:
		loc, err := su.Parse(resp.Header.Get("Location"))
		if err != nil || loc.Host == "" {
			return ""
		}
		// /mcp → /mcp/ on the same host is a router's slash fix, not a gate.
		if strings.EqualFold(loc.Host, su.Host) && !looksLikeLogin(loc.Path) {
			return ""
		}
		return gateMessage(serverURL, loc, iapHeader)
	case iapHeader:
		return gateMessage(serverURL, nil, true)
	case resp.StatusCode/100 == 2:
		ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if ct == "text/html" {
			return gateMessage(serverURL, su, false)
		}
	}
	return ""
}

func looksLikeLogin(path string) bool {
	p := strings.ToLower(path)
	for _, w := range []string{"login", "signin", "sign_in", "sso", "oauth", "auth"} {
		if strings.Contains(p, w) {
			return true
		}
	}
	return false
}

// gateMessage says what the gate is and what to configure instead. target is
// where it sends the browser (nil when unknown).
func gateMessage(serverURL string, target *url.URL, iap bool) string {
	host := "a web login page"
	if target != nil {
		host = target.Host
	}
	msg := "behind a login gate: it redirects to " + host + " — this is not the MCP OAuth flow; " +
		"configure oauth with that provider's issuer, or a static bearer / token"
	switch {
	case iap || (target != nil && (strings.EqualFold(target.Host, "accounts.google.com") || strings.Contains(target.RawQuery, "_gcp_iap") || strings.Contains(target.Path, "_gcp_iap"))):
		msg += ". Google IAP: it takes a Google ID token whose aud is the IAP's OAuth client id (or a client allowlisted for programmatic access) — " +
			`oauth {"issuer": "https://accounts.google.com", "client_id": "<that client>", "scopes": ["openid", "email"], "send": "id_token", "auth_params": {"access_type": "offline"}}`
	case target != nil && (strings.HasSuffix(strings.ToLower(target.Host), ".cloudflareaccess.com") || strings.Contains(target.Path, "/cdn-cgi/access/")):
		msg += ". Cloudflare Access: its token comes from `cloudflared access token -app=" + serverURL + "` and goes in a cf-access-token header — " +
			`"headers": {"cf-access-token": "$CF_ACCESS_TOKEN"} with the variable set from that command. ` +
			"dun cannot run a token command itself yet, so the token is refreshed by hand when it expires"
	}
	return msg
}
