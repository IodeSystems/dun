package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// brokerHarness is an engine whose OAuth is the broker's, as under dund.
func brokerHarness(t *testing.T, b *OAuthBroker, br *browser) *Harness {
	t.Helper()
	h := newNoteHarness(t)
	h.mgr, h.lastErr, h.lastStart = mcpmgr.NewManager(), map[string]string{}, map[string]time.Duration{}
	t.Cleanup(h.mgr.Close)
	h.Session = &agent.Session{}
	h.cfg.OnLogin = func(string, string) { t.Error("the engine showed a login: dund's to show") }
	h.cfg.OAuthBroker = func(ctx context.Context, req OAuthRequest) (OAuthResult, error) {
		return b.Do(ctx, req, br.notify)
	}
	return h
}

// Under dund, the login and the refresh are the broker's: the engine asks
// it, two engines share one login, and a 401 makes the broker refresh.
func TestOAuthBroker_EnginesShareOneLogin(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	br := &browser{}
	b := NewOAuthBroker(nil)
	spec := ServerSpec{ID: "snow", URL: w.mcpURL(), OAuth: &OAuthSpec{ClientID: w.clientID, RedirectPort: freePort(t)}}

	h1 := brokerHarness(t, b, br)
	if _, err := h1.AddServer(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if br.count() != 1 {
		t.Fatalf("logins: %d", br.count())
	}
	h2 := brokerHarness(t, b, br)
	if _, err := h2.AddServer(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if br.count() != 1 {
		t.Errorf("the second engine logged in again: %d logins", br.count())
	}
	tokens := len(w.tokenForms)

	// The server refuses the token: the broker refreshes, once, and both
	// engines' next requests carry the new one.
	w.mu.Lock()
	w.valid = map[string]bool{}
	w.mu.Unlock()
	src := h1.oauthFor(Server{ID: "snow", URL: w.mcpURL(), OAuth: spec.OAuth})
	if err := src.OnUnauthorized(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := len(w.tokenForms) - tokens; got != 1 || w.tokenForms[len(w.tokenForms)-1].Get("grant_type") != "refresh_token" {
		t.Fatalf("token requests after the 401: %d, last %v", got, w.tokenForms[len(w.tokenForms)-1])
	}
	// The second engine still sends the old token until the server
	// refuses it too; then it gets the broker's new one, with no second
	// refresh.
	src2 := h2.oauthFor(Server{ID: "snow", URL: w.mcpURL(), OAuth: spec.OAuth})
	if err := src2.OnUnauthorized(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	a1, _ := src.Authorization(context.Background())
	a2, _ := src2.Authorization(context.Background())
	if a1 == "" || a1 != a2 || len(w.tokenForms)-tokens != 1 {
		t.Errorf("after the refresh: %q and %q, %d token requests", a1, a2, len(w.tokenForms)-tokens)
	}
	if st := src.status(); !st.LoggedIn || !st.Fresh {
		t.Errorf("status: %+v", st)
	}
	if br.count() != 1 {
		t.Errorf("a refresh opened a login: %d", br.count())
	}
}

// A scripted OAuth server's auth_cmd is `dun oauth token` with its spec,
// which decodes back; no bearer goes into the file.
func TestShellConfig_OAuthAuthCmd(t *testing.T) {
	s := Server{ID: "eleven", URL: "https://api.example/mcp", Via: ViaShell,
		Headers: map[string]string{"Authorization": "Bearer stale", "X-A": "1"},
		OAuth:   &OAuthSpec{ClientID: "https://dun.example/client.json", ClientSecret: "$ELEVEN_SECRET"}}
	b, err := mcpshellConfig([]Server{s}, func(s Server) string {
		return "'/bin/dun' oauth token --spec " + EncodeOAuthAuthSpec(OAuthAuthSpec{ID: s.ID, URL: s.URL, OAuth: *s.OAuth})
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
	got, err := DecodeOAuthAuthSpec(spec)
	if err != nil || got.ID != "eleven" || got.OAuth.ClientSecret != "$ELEVEN_SECRET" || got.OAuth.ClientID != s.OAuth.ClientID {
		t.Errorf("spec: %+v %v", got, err)
	}
	if strings.Contains(string(b), "stale") {
		t.Error("the static bearer was written")
	}
}

// Setting an OAuth server scripted is allowed now.
func TestOAuth_ScriptedIsAllowed(t *testing.T) {
	if err := checkOAuth(ServerSpec{ID: "x", URL: "https://x/mcp", OAuth: &OAuthSpec{ClientID: "c"}}, ViaShell); err != nil {
		t.Errorf("checkOAuth: %v", err)
	}
}

// With no dund, `dun oauth token` refreshes from the token file itself;
// a retry (the server refused the token) refreshes even a fresh one.
func TestOAuthTokenHere(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	br := &browser{}
	spec := OAuthSpec{ClientID: w.clientID, RedirectPort: freePort(t)}
	s := Server{ID: "snow", URL: w.mcpURL(), OAuth: &spec}
	ctx := context.Background()
	a, err := OAuthTokenHere(ctx, s, false, br.notify, nil)
	if err != nil || !strings.HasPrefix(a, "Bearer ") || br.count() != 1 {
		t.Fatalf("first: %q %v, %d logins", a, err, br.count())
	}
	again, err := OAuthTokenHere(ctx, s, false, br.notify, nil)
	if err != nil || again != a {
		t.Errorf("a fresh token is handed back: %q %v", again, err)
	}
	retried, err := OAuthTokenHere(ctx, s, true, br.notify, nil)
	if err != nil || retried == a || br.count() != 1 {
		t.Errorf("retry: %q (was %q) %v, %d logins", retried, a, err, br.count())
	}
}

// With no client_id, dun registers itself (RFC 7591) once and logs in as
// that client; the next login reuses the registration.
func TestOAuthAuto_RegistersOnce(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	w.dcr, w.clientID = true, "not-yet"
	port := freePort(t)
	b := &browser{}
	c := newOAuthClient(Server{ID: "lin", URL: w.mcpURL(), OAuth: &OAuthSpec{RedirectPort: port}}, b.notify, nil)
	ctx := context.Background()
	if err := c.ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if len(w.registered) != 1 || w.authorizeQ[0].Get("client_id") != "reg-1" {
		t.Fatalf("registered %v, authorize %v", w.registered, w.authorizeQ)
	}
	if r := w.registered[0]; r["token_endpoint_auth_method"] != "none" ||
		fmt.Sprint(r["redirect_uris"]) != fmt.Sprintf("[http://127.0.0.1:%d/oauth/callback]", port) {
		t.Errorf("registration: %v", r)
	}
	if a, _ := c.Authorization(ctx); a == "" {
		t.Fatal("no token")
	}
	// A new client on the same files: the token is the registered client's.
	c2 := newOAuthClient(Server{ID: "lin", URL: w.mcpURL(), OAuth: &OAuthSpec{RedirectPort: port}}, b.notify, nil)
	if a, _ := c2.Authorization(ctx); a == "" || c2.clientID() != "reg-1" {
		t.Errorf("a new client: %q as %q", a, c2.clientID())
	}
	if err := c2.Login(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if len(w.registered) != 1 {
		t.Errorf("registered again: %d", len(w.registered))
	}
}

// A server that takes a client URL gets dun's, with the default port.
func TestOAuthAuto_MetadataDocument(t *testing.T) {
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", DefaultOAuthRedirectPort)); err != nil {
		t.Skipf("port %d is in use here", DefaultOAuthRedirectPort)
	} else {
		ln.Close()
	}
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	w.cimd, w.dcr, w.clientID = true, true, DunClientMetadataURL
	b := &browser{}
	c := newOAuthClient(Server{ID: "eleven", URL: w.mcpURL(), OAuth: &OAuthSpec{}}, b.notify, nil)
	if err := c.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.authorizeQ[0].Get("client_id") != DunClientMetadataURL || len(w.registered) != 0 {
		t.Errorf("authorize %v, registered %v", w.authorizeQ[0], w.registered)
	}
}

// Neither: the error says to set a client id, and the redirect URI.
func TestOAuthAuto_NeitherAsksForAClientID(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	port := freePort(t)
	c := newOAuthClient(Server{ID: "snow", URL: w.mcpURL(), OAuth: &OAuthSpec{RedirectPort: port}}, (&browser{}).notify, nil)
	err := c.ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "set oauth.client_id") ||
		!strings.Contains(err.Error(), fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", port)) {
		t.Errorf("err: %v", err)
	}
	if len(w.authorizeQ) != 0 {
		t.Error("a login was opened")
	}
}

// Adding a server that answers with an OAuth challenge gives it an oauth
// block; dun then picks the client.
func TestAddServer_DetectsOAuth(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	if !wantsOAuth(context.Background(), ServerSpec{ID: "snow", URL: w.mcpURL()}) {
		t.Error("the challenge was not seen")
	}
	if wantsOAuth(context.Background(), ServerSpec{ID: "snow", URL: w.mcpURL(), Bearer: "x"}) {
		t.Error("a server with a bearer is not an oauth one")
	}
}

// Two clients sharing a token (two mcpshells) both get a 401 and both ask
// again naming it: one refresh, and the second gets the first's new token.
func TestOAuthBroker_TwoRetriesOneRefresh(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	b := NewOAuthBroker(nil)
	req := OAuthRequest{ID: "snow", URL: w.mcpURL(), Spec: OAuthSpec{ClientID: w.clientID, RedirectPort: freePort(t)}}
	br := &browser{}
	ctx := context.Background()
	req.Op = "ensure"
	first, err := b.Do(ctx, req, br.notify)
	if err != nil || first.Authorization == "" {
		t.Fatalf("ensure: %+v %v", first, err)
	}
	tokens := len(w.tokenForms)
	req.Op, req.Rejected = "unauthorized", first.Authorization
	a, err := b.Do(ctx, req, br.notify)
	if err != nil || a.Authorization == first.Authorization {
		t.Fatalf("first retry: %+v %v", a, err)
	}
	c, err := b.Do(ctx, req, br.notify)
	if err != nil || c.Authorization != a.Authorization || len(w.tokenForms)-tokens != 1 {
		t.Errorf("second retry: %q (first got %q), %d refreshes", c.Authorization, a.Authorization, len(w.tokenForms)-tokens)
	}
}

// A scripted OAuth server with no client_id (dun picks the client) still
// gets its token through `dun oauth token --spec`.
func TestDecodeOAuthAuthSpec_NoClientID(t *testing.T) {
	enc := EncodeOAuthAuthSpec(OAuthAuthSpec{ID: "eleven", URL: "https://x/mcp"})
	if got, err := DecodeOAuthAuthSpec(enc); err != nil || got.ID != "eleven" {
		t.Errorf("spec: %+v %v", got, err)
	}
	if _, err := DecodeOAuthAuthSpec(EncodeOAuthAuthSpec(OAuthAuthSpec{ID: "eleven"})); err == nil {
		t.Error("a spec with no url was taken")
	}
}
