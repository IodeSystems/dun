package dundapi_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/internal/dundapi"
)

// fakeIdP is an OpenID provider with what the code flow needs. Its token
// endpoint refuses Basic auth, as Zitadel does for a client with no secret.
type fakeIdP struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	mu       sync.Mutex
	nonces   map[string]string // code → nonce
	email    string
	verified bool
}

func newIdP(t *testing.T) *fakeIdP {
	t.Helper()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &fakeIdP{key: k, nonces: map[string]string{}, email: "carl@example.com", verified: true}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	iss := f.srv.URL
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": iss, "authorization_endpoint": iss + "/authorize",
			"token_endpoint": iss + "/token", "jwks_uri": iss + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
			"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic"}})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if _, _, basic := r.BasicAuth(); basic {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "dund-client" || r.Form.Get("code_verifier") == "" {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		nonce := f.nonces[r.Form.Get("code")]
		f.mu.Unlock()
		sig, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		idt, _ := jwt.Signed(sig).Claims(map[string]any{"iss": iss, "aud": "dund-client", "sub": "u1",
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce,
			"email": f.email, "email_verified": f.verified}).Serialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "opaque", "token_type": "Bearer", "id_token": idt, "expires_in": 3600})
	})
	return f
}

// approve plays the provider's login page: it records the nonce for a code.
func (f *fakeIdP) approve(t *testing.T, authURL, code string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil || !strings.HasPrefix(authURL, f.srv.URL+"/authorize") {
		t.Fatalf("not sent to the provider: %s", authURL)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("client_id") != "dund-client" {
		t.Fatalf("authorize request without PKCE or client: %s", authURL)
	}
	f.mu.Lock()
	f.nonces[code] = q.Get("nonce")
	f.mu.Unlock()
	return q.Get("state")
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func ssoServer(t *testing.T, idp *fakeIdP) *httptest.Server {
	t.Helper()
	svc, err := dund.New(t.TempDir(), &fakeSpawner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	h, err := dundapi.Handler(svc, dundapi.Options{Owner: "unix:1000",
		OIDC: dundapi.OIDC{Issuer: idp.srv.URL + "/", ClientID: "dund-client", Allow: []string{"Carl@Example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// signIn runs the flow to the callback and returns where dund lands.
func signIn(t *testing.T, srv *httptest.Server, idp *fakeIdP, ret, code string) string {
	t.Helper()
	c := noRedirect()
	resp, err := c.Get(srv.URL + "/auth/login?return=" + url.QueryEscape(ret))
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("login: %v %v", resp.Status, err)
	}
	state := idp.approve(t, resp.Header.Get("Location"), code)
	resp, err = c.Get(srv.URL + "/auth/callback?code=" + code + "&state=" + url.QueryEscape(state))
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("callback: %v %v", resp.Status, err)
	}
	return resp.Header.Get("Location")
}

func fragment(t *testing.T, loc string) url.Values {
	t.Helper()
	_, frag, _ := strings.Cut(loc, "#")
	v, err := url.ParseQuery(frag)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSignInWithTheIdentityProvider(t *testing.T) {
	idp := newIdP(t)
	srv := ssoServer(t, idp) // the issuer configured WITH a trailing slash

	// The web: back to /signin with the token in the fragment.
	loc := signIn(t, srv, idp, "/session/x", "c1")
	if !strings.HasPrefix(loc, "/signin#") {
		t.Fatalf("landed at %s", loc)
	}
	f := fragment(t, loc)
	tok := f.Get("token")
	if !strings.HasPrefix(tok, "dun_s_") || f.Get("return") != "/session/x" {
		t.Fatalf("fragment: %v", f)
	}
	// The token works on the API, and signing out ends it.
	if code, _ := call(t, srv, tok, "GET", "/api/v1/sessions", nil); code != 200 {
		t.Fatalf("sign-in token refused: %d", code)
	}
	if code, _ := call(t, srv, tok, "POST", "/api/v1/sign-out", nil); code != 200 {
		t.Fatalf("sign-out: %d", code)
	}
	if code, _ := call(t, srv, tok, "GET", "/api/v1/sessions", nil); code != 401 {
		t.Fatalf("after sign-out: %d", code)
	}

	// The app: back to its own dun:// link.
	if loc := signIn(t, srv, idp, "dun://auth", "c2"); !strings.HasPrefix(loc, "dun://auth#token=dun_s_") {
		t.Fatalf("app landing: %s", loc)
	}
}

func TestSignInRefusals(t *testing.T) {
	idp := newIdP(t)
	srv := ssoServer(t, idp)

	idp.email = "stranger@example.com"
	if f := fragment(t, signIn(t, srv, idp, "/", "c1")); f.Get("token") != "" || !strings.Contains(f.Get("error"), "may not sign in") {
		t.Errorf("not on the allow-list: %v", f)
	}
	idp.email, idp.verified = "carl@example.com", false
	if f := fragment(t, signIn(t, srv, idp, "/", "c2")); f.Get("token") != "" || !strings.Contains(f.Get("error"), "verified email") {
		t.Errorf("unverified: %v", f)
	}

	// A state is good once.
	idp.verified = true
	c := noRedirect()
	resp, _ := c.Get(srv.URL + "/auth/login?return=/")
	state := idp.approve(t, resp.Header.Get("Location"), "c3")
	resp, _ = c.Get(srv.URL + "/auth/callback?code=c3&state=" + url.QueryEscape(state))
	if fragment(t, resp.Header.Get("Location")).Get("token") == "" {
		t.Fatal("first use refused")
	}
	resp, _ = c.Get(srv.URL + "/auth/callback?code=c3&state=" + url.QueryEscape(state))
	if fragment(t, resp.Header.Get("Location")).Get("token") != "" {
		t.Error("a state was used twice")
	}

	// Not an open redirect.
	for _, bad := range []string{"https://evil.example/", "//evil.example/x", "javascript:alert(1)"} {
		resp, _ := c.Get(srv.URL + "/auth/login?return=" + url.QueryEscape(bad))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("return %q: %d", bad, resp.StatusCode)
		}
	}
}
