package dun

import (
	"context"
	"encoding/json"
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
