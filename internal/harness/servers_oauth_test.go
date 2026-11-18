package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/oauth"
	"github.com/iodesystems/dun/internal/oauth/oauthtest"
)

// The whole flow through the harness: the first connect 401s, discovery
// finds the IdP, the browser logs in, the token is stored 0600, and the MCP
// requests carry it.
func TestOAuth_DiscoveryLoginThroughTheHarness(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	b := &oauthtest.Browser{}
	h := newNoteHarness(t)
	h.mgr, h.lastErr, h.lastStart = mcpmgr.NewManager(), map[string]string{}, map[string]time.Duration{}
	defer h.mgr.Close()
	h.Session = &agent.Session{}
	h.cfg.OnLogin = b.Notify
	port := oauthtest.FreePort(t)

	spec := mcpserver.Spec{ID: "snow", URL: w.MCPURL(), OAuth: &oauth.Spec{ClientID: w.ClientID, RedirectPort: port}}
	if _, err := h.AddServer(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if b.Count() != 1 {
		t.Fatalf("logins shown: %d", b.Count())
	}
	if !strings.HasPrefix(b.URLs()[0], w.IdP.URL+"/authorize?") {
		t.Errorf("login URL %s", b.URLs()[0])
	}
	q := w.AuthorizeQ[0]
	if q.Get("resource") != w.MCPURL() || q.Get("scope") != "openid email" || q.Get("nonce") == "" ||
		q.Get("redirect_uri") != fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", port) {
		t.Errorf("authorize request: %v", q)
	}
	if w.TokenForms[0].Get("resource") != w.MCPURL() || w.ClientAuth[0] != "none" || w.TokenForms[0].Get("client_id") != w.ClientID {
		t.Errorf("token request: %v by %v", w.TokenForms[0], w.ClientAuth)
	}
	path := filepath.Join(oauth.Dir(), "snow.json")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", st, err)
	}
	var tok struct { // the token file's fields this test reads (oauth.oauthToken)
		AccessToken   string `json:"access_token"`
		RefreshToken  string `json:"refresh_token"`
		Issuer        string `json:"issuer"`
		TokenEndpoint string `json:"token_endpoint"`
	}
	b2, _ := os.ReadFile(path)
	_ = json.Unmarshal(b2, &tok)
	if tok.TokenEndpoint != w.IdP.URL+"/token" || tok.Issuer != w.IdP.URL || tok.RefreshToken == "" {
		t.Errorf("stored: %+v", tok)
	}
	if !slices.Contains(w.MCPAuth, "Bearer "+tok.AccessToken) {
		t.Errorf("no MCP request carried the token: %v", w.MCPAuth)
	}
	if !slices.Contains(h.ToolNames(), "snow__query") {
		t.Errorf("tools %v", h.ToolNames())
	}
	var auth string
	for _, s := range h.Servers() {
		if s.ID == "snow" {
			auth = s.Auth
		}
	}
	if !strings.Contains(auth, "logged in as ann@example.com until") {
		t.Errorf("listing auth: %q", auth)
	}
	// Saved as configured: the oauth block, no token.
	if f, _ := mcpserver.ReadConfig(mcpserver.UserServersFile()); f == nil || f.Servers[0].OAuth == nil || f.Servers[0].OAuth.ClientID != w.ClientID {
		t.Errorf("saved %+v", f)
	}

	// Logout: deleted, revoked, stopped.
	msg, err := h.LogoutServer(context.Background(), "snow")
	if err != nil || !strings.Contains(msg, "revoked") || !strings.Contains(msg, "stopped") {
		t.Fatalf("logout: %q %v", msg, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("logout must delete the token file")
	}
	if !slices.Contains(w.Revoked, tok.RefreshToken) || h.mgr.ServerStarted("snow") {
		t.Errorf("revoked %v, running %v", w.Revoked, h.mgr.ServerStarted("snow"))
	}
	for _, s := range h.Servers() {
		if s.ID == "snow" && !strings.Contains(s.Auth, "needs login") {
			t.Errorf("after logout: %q", s.Auth)
		}
	}
}

// A server behind a gate fails to start with the diagnosis, not a JSON
// decode error.
func TestStartServer_GateExplained(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, "https://corp.cloudflareaccess.com/cdn-cgi/access/login/x", http.StatusFound)
	}))
	defer ts.Close()
	h := newNoteHarness(t)
	h.mgr, h.lastErr, h.lastStart = mcpmgr.NewManager(), map[string]string{}, map[string]time.Duration{}
	defer h.mgr.Close()
	h.Session = &agent.Session{}
	_, err := h.AddServer(context.Background(), mcpserver.Spec{ID: "gated", URL: ts.URL + "/mcp", Timeout: 5})
	if err == nil || !strings.Contains(err.Error(), "gated is behind a login gate") || !strings.Contains(err.Error(), "Cloudflare Access") {
		t.Errorf("got %v", err)
	}
}

// brokerHarness is an engine whose OAuth is the broker's, as under dund.
func brokerHarness(t *testing.T, b *oauth.Broker, br *oauthtest.Browser) *Harness {
	t.Helper()
	h := newNoteHarness(t)
	h.mgr, h.lastErr, h.lastStart = mcpmgr.NewManager(), map[string]string{}, map[string]time.Duration{}
	t.Cleanup(h.mgr.Close)
	h.Session = &agent.Session{}
	h.cfg.OnLogin = func(string, string) { t.Error("the engine showed a login: dund's to show") }
	h.cfg.OAuthBroker = func(ctx context.Context, req oauth.Request) (oauth.Result, error) {
		return b.Do(ctx, req, br.Notify)
	}
	return h
}

// Under dund, the login and the refresh are the broker's: the engine asks
// it, two engines share one login, and a 401 makes the broker refresh.
func TestOAuthBroker_EnginesShareOneLogin(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	br := &oauthtest.Browser{}
	b := oauth.NewBroker(nil)
	spec := mcpserver.Spec{ID: "snow", URL: w.MCPURL(), OAuth: &oauth.Spec{ClientID: w.ClientID, RedirectPort: oauthtest.FreePort(t)}}

	h1 := brokerHarness(t, b, br)
	if _, err := h1.AddServer(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if br.Count() != 1 {
		t.Fatalf("logins: %d", br.Count())
	}
	h2 := brokerHarness(t, b, br)
	if _, err := h2.AddServer(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if br.Count() != 1 {
		t.Errorf("the second engine logged in again: %d logins", br.Count())
	}
	tokens := len(w.TokenForms)

	// The server refuses the token: the broker refreshes, once, and both
	// engines' next requests carry the new one.
	w.Mu.Lock()
	w.Valid = map[string]bool{}
	w.Mu.Unlock()
	src := h1.oauthFor(mcpserver.Server{ID: "snow", URL: w.MCPURL(), OAuth: spec.OAuth})
	if err := src.OnUnauthorized(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := len(w.TokenForms) - tokens; got != 1 || w.TokenForms[len(w.TokenForms)-1].Get("grant_type") != "refresh_token" {
		t.Fatalf("token requests after the 401: %d, last %v", got, w.TokenForms[len(w.TokenForms)-1])
	}
	// The second engine still sends the old token until the server
	// refuses it too; then it gets the broker's new one, with no second
	// refresh.
	src2 := h2.oauthFor(mcpserver.Server{ID: "snow", URL: w.MCPURL(), OAuth: spec.OAuth})
	if err := src2.OnUnauthorized(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	a1, _ := src.Authorization(context.Background())
	a2, _ := src2.Authorization(context.Background())
	if a1 == "" || a1 != a2 || len(w.TokenForms)-tokens != 1 {
		t.Errorf("after the refresh: %q and %q, %d token requests", a1, a2, len(w.TokenForms)-tokens)
	}
	if st := src.Status(); !st.LoggedIn || !st.Fresh {
		t.Errorf("status: %+v", st)
	}
	if br.Count() != 1 {
		t.Errorf("a refresh opened a login: %d", br.Count())
	}
}

// A scripted OAuth server's auth_cmd is `dun oauth token` with its spec,
// which decodes back; no bearer goes into the file.
func TestShellConfig_OAuthAuthCmd(t *testing.T) {
	s := mcpserver.Server{ID: "eleven", URL: "https://api.example/mcp", Via: mcpserver.ViaShell,
		Headers: map[string]string{"Authorization": "Bearer stale", "X-A": "1"},
		OAuth:   &oauth.Spec{ClientID: "https://dun.example/client.json", ClientSecret: "$ELEVEN_SECRET"}}
	b, err := mcpshellConfig([]mcpserver.Server{s}, func(s mcpserver.Server) string {
		return "'/bin/dun' oauth token --spec " + oauth.EncodeAuthSpec(oauth.AuthSpec{ID: s.ID, URL: s.URL, OAuth: *s.OAuth})
	})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]mcpshellServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	e := cfg.MCPServers[ShellNamespace("eleven")]
	if e.Headers["Authorization"] != "" || e.Headers["X-A"] != "1" {
		t.Errorf("headers: %v", e.Headers)
	}
	spec, ok := strings.CutPrefix(e.AuthCmd, "'/bin/dun' oauth token --spec ")
	if !ok {
		t.Fatalf("auth_cmd %q", e.AuthCmd)
	}
	got, err := oauth.DecodeAuthSpec(spec)
	if err != nil || got.ID != "eleven" || got.OAuth.ClientSecret != "$ELEVEN_SECRET" || got.OAuth.ClientID != s.OAuth.ClientID {
		t.Errorf("spec: %+v %v", got, err)
	}
	if strings.Contains(string(b), "stale") {
		t.Error("the static bearer was written")
	}
}

// Adding a server that answers with an OAuth challenge gives it an oauth
// block; dun then picks the client.
func TestAddServer_DetectsOAuth(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	if !wantsOAuth(context.Background(), mcpserver.Spec{ID: "snow", URL: w.MCPURL()}) {
		t.Error("the challenge was not seen")
	}
	if wantsOAuth(context.Background(), mcpserver.Spec{ID: "snow", URL: w.MCPURL(), Bearer: "x"}) {
		t.Error("a server with a bearer is not an oauth one")
	}
}
