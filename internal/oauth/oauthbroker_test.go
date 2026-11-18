package oauth

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/oauth/oauthtest"
)

// With no dund, `dun oauth token` refreshes from the token file itself;
// a retry (the server refused the token) refreshes even a fresh one.
func TestOAuthTokenHere(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	br := &oauthtest.Browser{}
	spec := Spec{ClientID: w.ClientID, RedirectPort: oauthtest.FreePort(t)}
	s := AuthSpec{ID: "snow", URL: w.MCPURL(), OAuth: spec}
	ctx := context.Background()
	a, err := TokenHere(ctx, s, false, br.Notify, nil)
	if err != nil || !strings.HasPrefix(a, "Bearer ") || br.Count() != 1 {
		t.Fatalf("first: %q %v, %d logins", a, err, br.Count())
	}
	again, err := TokenHere(ctx, s, false, br.Notify, nil)
	if err != nil || again != a {
		t.Errorf("a fresh token is handed back: %q %v", again, err)
	}
	retried, err := TokenHere(ctx, s, true, br.Notify, nil)
	if err != nil || retried == a || br.Count() != 1 {
		t.Errorf("retry: %q (was %q) %v, %d logins", retried, a, err, br.Count())
	}
}

// With no client_id, dun registers itself (RFC 7591) once and logs in as
// that client; the next login reuses the registration.
func TestOAuthAuto_RegistersOnce(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	w.DCR, w.ClientID = true, "not-yet"
	port := oauthtest.FreePort(t)
	b := &oauthtest.Browser{}
	c := NewClient("lin", w.MCPURL(), Spec{RedirectPort: port}, b.Notify, nil)
	ctx := context.Background()
	if err := c.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if len(w.Registered) != 1 || w.AuthorizeQ[0].Get("client_id") != "reg-1" {
		t.Fatalf("registered %v, authorize %v", w.Registered, w.AuthorizeQ)
	}
	if r := w.Registered[0]; r["token_endpoint_auth_method"] != "none" ||
		fmt.Sprint(r["redirect_uris"]) != fmt.Sprintf("[http://127.0.0.1:%d/oauth/callback]", port) {
		t.Errorf("registration: %v", r)
	}
	if a, _ := c.Authorization(ctx); a == "" {
		t.Fatal("no token")
	}
	// A new client on the same files: the token is the registered client's.
	c2 := NewClient("lin", w.MCPURL(), Spec{RedirectPort: port}, b.Notify, nil)
	if a, _ := c2.Authorization(ctx); a == "" || c2.clientID() != "reg-1" {
		t.Errorf("a new client: %q as %q", a, c2.clientID())
	}
	if err := c2.Login(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if len(w.Registered) != 1 {
		t.Errorf("registered again: %d", len(w.Registered))
	}
}

// A server that takes a client URL gets dun's, with the default port.
func TestOAuthAuto_MetadataDocument(t *testing.T) {
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", DefaultRedirectPort)); err != nil {
		t.Skipf("port %d is in use here", DefaultRedirectPort)
	} else {
		ln.Close()
	}
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	w.CIMD, w.DCR, w.ClientID = true, true, DunClientMetadataURL
	b := &oauthtest.Browser{}
	c := NewClient("eleven", w.MCPURL(), Spec{}, b.Notify, nil)
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.AuthorizeQ[0].Get("client_id") != DunClientMetadataURL || len(w.Registered) != 0 {
		t.Errorf("authorize %v, registered %v", w.AuthorizeQ[0], w.Registered)
	}
}

// Two clients sharing a token (two mcpshells) both get a 401 and both ask
// again naming it: one refresh, and the second gets the first's new token.
func TestOAuthBroker_TwoRetriesOneRefresh(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	b := NewBroker(nil)
	req := Request{ID: "snow", URL: w.MCPURL(), Spec: Spec{ClientID: w.ClientID, RedirectPort: oauthtest.FreePort(t)}}
	br := &oauthtest.Browser{}
	ctx := context.Background()
	req.Op = "ensure"
	first, err := b.Do(ctx, req, br.Notify)
	if err != nil || first.Authorization == "" {
		t.Fatalf("ensure: %+v %v", first, err)
	}
	tokens := len(w.TokenForms)
	req.Op, req.Rejected = "unauthorized", first.Authorization
	a, err := b.Do(ctx, req, br.Notify)
	if err != nil || a.Authorization == first.Authorization {
		t.Fatalf("first retry: %+v %v", a, err)
	}
	c, err := b.Do(ctx, req, br.Notify)
	if err != nil || c.Authorization != a.Authorization || len(w.TokenForms)-tokens != 1 {
		t.Errorf("second retry: %q (first got %q), %d refreshes", c.Authorization, a.Authorization, len(w.TokenForms)-tokens)
	}
}

// A scripted OAuth server with no client_id (dun picks the client) still
// gets its token through `dun oauth token --spec`.
func TestDecodeOAuthAuthSpec_NoClientID(t *testing.T) {
	enc := EncodeAuthSpec(AuthSpec{ID: "eleven", URL: "https://x/mcp"})
	if got, err := DecodeAuthSpec(enc); err != nil || got.ID != "eleven" {
		t.Errorf("spec: %+v %v", got, err)
	}
	if _, err := DecodeAuthSpec(EncodeAuthSpec(AuthSpec{ID: "eleven"})); err == nil {
		t.Error("a spec with no url was taken")
	}
}

// Neither: the error says to set a client id, and the redirect URI.
func TestOAuthAuto_NeitherAsksForAClientID(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	port := oauthtest.FreePort(t)
	c := NewClient("snow", w.MCPURL(), Spec{RedirectPort: port}, (&oauthtest.Browser{}).Notify, nil)
	err := c.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "set oauth.client_id") ||
		!strings.Contains(err.Error(), fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", port)) {
		t.Errorf("err: %v", err)
	}
	if len(w.AuthorizeQ) != 0 {
		t.Error("a login was opened")
	}
}
