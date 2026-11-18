package oauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/dun/internal/oauth/oauthtest"
)

// client is w's server's login as spec, shown to b.
func client(t *testing.T, w *oauthtest.World, spec Spec, b *oauthtest.Browser) *Client {
	if spec.ClientID == "" {
		spec.ClientID = w.ClientID
	}
	if spec.RedirectPort == 0 {
		spec.RedirectPort = oauthtest.FreePort(t)
	}
	return NewClient("snow", w.MCPURL(), spec, b.Notify, nil)
}

// A token about to expire is refreshed before it is sent.
func TestOAuth_RefreshBeforeExpiry(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	w.ExpiresIn = 30 // inside oauthSkew: stale as soon as it is issued
	b := &oauthtest.Browser{}
	oc := client(t, w, Spec{}, b)
	ctx := context.Background()
	if err := oc.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	first := oc.tok.AccessToken
	v, err := oc.Authorization(ctx)
	if err != nil || v == "" || v == "Bearer "+first {
		t.Fatalf("authorization %q %v; first token %s", v, err, first)
	}
	if last := w.TokenForms[len(w.TokenForms)-1]; last.Get("grant_type") != "refresh_token" || last.Get("resource") != w.MCPURL() {
		t.Errorf("last token request: %v", last)
	}
	if b.Count() != 1 {
		t.Errorf("a refresh must not open a login: %d", b.Count())
	}
}

// A refused refresh on a 401 goes to the interactive login.
func TestOAuth_RefreshRefusedRunsLogin(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	b := &oauthtest.Browser{}
	oc := client(t, w, Spec{}, b)
	ctx := context.Background()
	if err := oc.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	w.Mu.Lock()
	w.RefuseRefresh = true
	w.Mu.Unlock()
	oc.lastLogin = time.Time{} // not "just logged in"
	if err := oc.OnUnauthorized(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if b.Count() != 2 {
		t.Errorf("logins shown: %d, want a second one", b.Count())
	}
	// The same 401 right after a login is a server refusing good tokens:
	// an error, not another browser tab.
	oc.deadRefresh = oc.tok.RefreshToken
	if err := oc.OnUnauthorized(ctx, ""); err == nil || !strings.Contains(err.Error(), "refused the token") {
		t.Errorf("got %v", err)
	}
}

// A login nobody finishes ends, with an error that says what to do.
func TestOAuth_LoginTimesOut(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	old := oauthLoginTimeout
	oauthLoginTimeout = 300 * time.Millisecond
	defer func() { oauthLoginTimeout = old }()
	w := oauthtest.NewWorld(t)
	spec := Spec{ClientID: w.ClientID, RedirectPort: oauthtest.FreePort(t)}
	shown := ""
	oc := NewClient("snow", w.MCPURL(), spec, func(_, u string) { shown = u }, nil)
	err := oc.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "/mcp login snow") || shown == "" {
		t.Errorf("err %v, shown %q", err, shown)
	}
}

// With an issuer the server's metadata is not needed (or asked for), no
// resource is sent unless configured, and the ID token can be what is sent.
func TestOAuth_IssuerModeAndIDToken(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	w.WithPRM, w.AcceptIDToken = false, true
	b := &oauthtest.Browser{}
	oc := client(t, w, Spec{Issuer: w.IdP.URL, Scopes: []string{"openid", "email"}, Send: SendIDToken,
		AuthParams: map[string]string{"access_type": "offline"}}, b)
	ctx := context.Background()
	if err := oc.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	q := w.AuthorizeQ[0]
	if q.Has("resource") || q.Get("access_type") != "offline" {
		t.Errorf("authorize: %v", q)
	}
	v, _ := oc.Authorization(ctx)
	if v != "Bearer "+oc.tok.IDToken {
		t.Errorf("sent %q, want the id token", v)
	}
	// And the MCP server takes it.
	mgr := mcpmgr.NewManager()
	defer mgr.Close()
	err := mgr.StartServer(ctx, mcpmgr.MCPConfig{ID: "sup", Name: "sup", URL: w.MCPURL(),
		Authorization: oc.Authorization, OnUnauthorized: oc.OnUnauthorized})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); len(mgr.GetTools()) == 0; {
		if err := mgr.ServerReadyErr("sup"); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("no tools discovered") // harness's waitServerTools, in short
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An id token whose nonce is not this login's is refused.
func TestOAuth_NonceMismatchRefused(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	b := &oauthtest.Browser{}
	oc := client(t, w, Spec{}, b)
	w.IdP.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/authorize" {
			q := r.URL.Query()
			q.Set("nonce", "someone-elses")
			r.URL.RawQuery = q.Encode()
		}
		w.ServeIdP(rw, r)
	})
	if err := oc.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Errorf("got %v", err)
	}
}

// A confidential client authenticates the way the server says: post when it
// lists only that, basic when it lists nothing. The secret is read from the
// environment and never written.
func TestOAuth_ConfidentialClient(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("SNOW_SECRET", "s3:cr/et")
	for _, c := range []struct {
		methods []string
		want    string
	}{{[]string{"client_secret_post"}, "post"}, {nil, "basic"}} {
		w := oauthtest.NewWorld(t)
		w.Secret, w.AuthMethods = "s3:cr/et", c.methods
		b := &oauthtest.Browser{}
		oc := client(t, w, Spec{ClientSecret: "${SNOW_SECRET}"}, b)
		if err := oc.Ensure(context.Background()); err != nil {
			t.Fatalf("%v: %v", c.methods, err)
		}
		if got := w.ClientAuth[0]; got != c.want {
			t.Errorf("%v: authenticated by %s, want %s", c.methods, got, c.want)
		}
		raw, _ := os.ReadFile(oc.path)
		if strings.Contains(string(raw), "s3:cr/et") {
			t.Error("the secret was written to the token file")
		}
		_, _ = oc.Logout(context.Background())
	}
}

// A login gate answers with a redirect to a web login: named, with the hint
// for the two gates dun recognises.
func TestProbe_LoginGates(t *testing.T) {
	for _, c := range []struct {
		name, location, ctype string
		status                int
		want                  string
	}{
		{"iap", "https://accounts.google.com/o/oauth2/v2/auth?client_id=x&state=_gcp_iap", "", 302, "Google IAP"},
		{"cloudflare", "https://corp.cloudflareaccess.com/cdn-cgi/access/login/mcp.corp", "", 302, "cloudflared access token -app="},
		{"sso", "https://sso.corp.example/login", "", 302, "redirects to sso.corp.example"},
		{"html", "", "text/html; charset=utf-8", 200, "behind a login gate"},
		{"slash", "/mcp/", "", 307, ""},
		{"mcp", "", "application/json", 200, ""},
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if c.location != "" {
				rw.Header().Set("Location", c.location)
			}
			if c.ctype != "" {
				rw.Header().Set("Content-Type", c.ctype)
			}
			rw.WriteHeader(c.status)
			io.WriteString(rw, "<html>sign in</html>")
		}))
		p, err := Probe(context.Background(), ts.URL+"/mcp")
		ts.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.want == "" && p.Gate != "" || c.want != "" && !strings.Contains(p.Gate, c.want) {
			t.Errorf("%s: gate %q, want %q", c.name, p.Gate, c.want)
		}
	}
}
