package dun

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// oauthWorld is an identity provider and an MCP server that trusts it: the
// server 401s with resource_metadata, serves its protected-resource
// metadata, and accepts only tokens the IdP issued.
type oauthWorld struct {
	t        *testing.T
	idp, mcp *httptest.Server
	clientID string

	mu            sync.Mutex
	n             int
	valid         map[string]bool // bearer values the MCP server accepts
	codes         map[string]url.Values
	expiresIn     int64
	refuseRefresh bool
	secret        string   // the client secret the IdP expects; "" = public client
	authMethods   []string // token_endpoint_auth_methods_supported
	withPRM       bool     // the MCP server names its metadata
	acceptIDToken bool     // the MCP server takes the ID token, not the access token
	authorizeQ    []url.Values
	tokenForms    []url.Values
	clientAuth    []string // how each token request authenticated: none, basic, post
	revoked       []string
	mcpAuth       []string // the Authorization of each MCP request
	cimd          bool     // client_id_metadata_document_supported
	dcr           bool     // a registration_endpoint (RFC 7591)
	registered    []map[string]any
}

func newOAuthWorld(t *testing.T) *oauthWorld {
	w := &oauthWorld{t: t, clientID: "dun-client", valid: map[string]bool{}, codes: map[string]url.Values{},
		expiresIn: 3600, withPRM: true}
	w.idp = httptest.NewServer(http.HandlerFunc(w.serveIdP))
	w.mcp = httptest.NewServer(http.HandlerFunc(w.serveMCP))
	t.Cleanup(func() { w.idp.Close(); w.mcp.Close() })
	return w
}

func (w *oauthWorld) mcpURL() string { return w.mcp.URL + "/mcp" }

func (w *oauthWorld) serveIdP(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server":
		md := map[string]any{"issuer": w.idp.URL,
			"authorization_endpoint": w.idp.URL + "/authorize", "token_endpoint": w.idp.URL + "/token",
			"revocation_endpoint":                   w.idp.URL + "/revoke",
			"token_endpoint_auth_methods_supported": w.authMethods,
			"code_challenge_methods_supported":      []string{"S256"},
			"client_id_metadata_document_supported": w.cimd}
		if w.dcr {
			md["registration_endpoint"] = w.idp.URL + "/register"
		}
		writeJSONResp(rw, md)
	case "/register":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.registered = append(w.registered, body)
		w.clientID = fmt.Sprintf("reg-%d", len(w.registered))
		rw.WriteHeader(http.StatusCreated)
		writeJSONResp(rw, map[string]any{"client_id": w.clientID})
	case "/authorize":
		q := r.URL.Query()
		w.authorizeQ = append(w.authorizeQ, q)
		if q.Get("client_id") != w.clientID || q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" {
			http.Error(rw, "bad authorize request", http.StatusBadRequest)
			return
		}
		w.n++
		code := fmt.Sprintf("code-%d", w.n)
		w.codes[code] = q
		http.Redirect(rw, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	case "/token":
		_ = r.ParseForm()
		f := r.PostForm
		w.tokenForms = append(w.tokenForms, f)
		how := "none"
		if u, p, ok := r.BasicAuth(); ok {
			how = "basic"
			if u != w.clientID || p != url.QueryEscape(w.secret) {
				tokenErr(rw, "invalid_client")
				return
			}
		} else if f.Get("client_secret") != "" {
			how = "post"
			if f.Get("client_secret") != w.secret {
				tokenErr(rw, "invalid_client")
				return
			}
		}
		w.clientAuth = append(w.clientAuth, how)
		if w.secret != "" && how == "none" {
			tokenErr(rw, "invalid_client")
			return
		}
		var q url.Values
		switch f.Get("grant_type") {
		case "authorization_code":
			var ok bool
			q, ok = w.codes[f.Get("code")]
			delete(w.codes, f.Get("code"))
			sum := sha256.Sum256([]byte(f.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") || f.Get("redirect_uri") != q.Get("redirect_uri") {
				tokenErr(rw, "invalid_grant")
				return
			}
		case "refresh_token":
			if w.refuseRefresh || !strings.HasPrefix(f.Get("refresh_token"), "rt-") {
				tokenErr(rw, "invalid_grant")
				return
			}
			q = url.Values{"scope": {"openid email"}}
		default:
			tokenErr(rw, "unsupported_grant_type")
			return
		}
		w.n++
		resp := map[string]any{"access_token": fmt.Sprintf("at-%d", w.n), "token_type": "Bearer",
			"expires_in": w.expiresIn, "refresh_token": fmt.Sprintf("rt-%d", w.n)}
		w.valid[fmt.Sprintf("at-%d", w.n)] = !w.acceptIDToken
		if strings.Contains(q.Get("scope"), "openid") {
			idt := fakeJWT(map[string]any{"iss": w.idp.URL, "sub": "u1", "email": "ann@example.com",
				"aud": w.clientID, "exp": time.Now().Add(time.Hour).Unix(), "nonce": q.Get("nonce")})
			resp["id_token"] = idt
			w.valid[idt] = w.acceptIDToken
		}
		writeJSONResp(rw, resp)
	case "/revoke":
		_ = r.ParseForm()
		w.revoked = append(w.revoked, r.PostForm.Get("token"))
	default:
		http.NotFound(rw, r)
	}
}

func (w *oauthWorld) serveMCP(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/.well-known/oauth-protected-resource/mcp" {
		writeJSONResp(rw, map[string]any{"resource": w.mcpURL(), "authorization_servers": []string{w.idp.URL},
			"scopes_supported": []string{"openid", "email"}})
		return
	}
	authz := r.Header.Get("Authorization")
	w.mu.Lock()
	w.mcpAuth = append(w.mcpAuth, authz)
	ok := w.valid[strings.TrimPrefix(authz, "Bearer ")]
	withPRM := w.withPRM
	w.mu.Unlock()
	if !ok {
		if withPRM {
			rw.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+w.mcp.URL+`/.well-known/oauth-protected-resource/mcp", scope="openid email"`)
		} else {
			rw.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		}
		http.Error(rw, "no", http.StatusUnauthorized)
		return
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &msg)
	if len(msg.ID) == 0 {
		rw.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch msg.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
	case "tools/list":
		result = map[string]any{"tools": []map[string]any{{"name": "query", "description": "run a query"}}}
	}
	writeJSONResp(rw, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
}

func writeJSONResp(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}

func tokenErr(rw http.ResponseWriter, code string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(rw, `{"error":%q}`, code)
}

func fakeJWT(claims map[string]any) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".sig"
}

// freePort is a loopback port nothing listens on, for the redirect URI.
func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// browser stands in for the person: it opens each login URL it is shown —
// the IdP redirects it to the loopback callback — and counts them.
type browser struct {
	mu   sync.Mutex
	urls []string
}

func (b *browser) notify(_ string, authURL string) {
	if authURL == "" {
		return
	}
	b.mu.Lock()
	b.urls = append(b.urls, authURL)
	b.mu.Unlock()
	go func() {
		resp, err := http.Get(authURL)
		if err == nil {
			resp.Body.Close()
		}
	}()
}

func (b *browser) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.urls)
}

func (w *oauthWorld) client(spec OAuthSpec, b *browser) *oauthClient {
	if spec.ClientID == "" {
		spec.ClientID = w.clientID
	}
	if spec.RedirectPort == 0 {
		spec.RedirectPort = freePort(w.t)
	}
	return newOAuthClient(Server{ID: "snow", URL: w.mcpURL(), OAuth: &spec}, b.notify, nil)
}

// The whole flow through the harness: the first connect 401s, discovery
// finds the IdP, the browser logs in, the token is stored 0600, and the MCP
// requests carry it.
func TestOAuth_DiscoveryLoginThroughTheHarness(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	b := &browser{}
	h := newNoteHarness(t)
	h.mgr, h.lastErr, h.lastStart = mcpmgr.NewManager(), map[string]string{}, map[string]time.Duration{}
	defer h.mgr.Close()
	h.Session = &agent.Session{}
	h.cfg.OnLogin = b.notify
	port := freePort(t)

	spec := ServerSpec{ID: "snow", URL: w.mcpURL(), OAuth: &OAuthSpec{ClientID: w.clientID, RedirectPort: port}}
	if _, err := h.AddServer(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if b.count() != 1 {
		t.Fatalf("logins shown: %d", b.count())
	}
	if !strings.HasPrefix(b.urls[0], w.idp.URL+"/authorize?") {
		t.Errorf("login URL %s", b.urls[0])
	}
	q := w.authorizeQ[0]
	if q.Get("resource") != w.mcpURL() || q.Get("scope") != "openid email" || q.Get("nonce") == "" ||
		q.Get("redirect_uri") != fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", port) {
		t.Errorf("authorize request: %v", q)
	}
	if w.tokenForms[0].Get("resource") != w.mcpURL() || w.clientAuth[0] != "none" || w.tokenForms[0].Get("client_id") != w.clientID {
		t.Errorf("token request: %v by %v", w.tokenForms[0], w.clientAuth)
	}
	path := filepath.Join(OAuthDir(), "snow.json")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", st, err)
	}
	var tok oauthToken
	b2, _ := os.ReadFile(path)
	_ = json.Unmarshal(b2, &tok)
	if tok.TokenEndpoint != w.idp.URL+"/token" || tok.Issuer != w.idp.URL || tok.RefreshToken == "" {
		t.Errorf("stored: %+v", tok)
	}
	if !slices.Contains(w.mcpAuth, "Bearer "+tok.AccessToken) {
		t.Errorf("no MCP request carried the token: %v", w.mcpAuth)
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
	if f, _ := readServersFile(UserServersFile()); f == nil || f.Servers[0].OAuth == nil || f.Servers[0].OAuth.ClientID != w.clientID {
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
	if !slices.Contains(w.revoked, tok.RefreshToken) || h.mgr.ServerStarted("snow") {
		t.Errorf("revoked %v, running %v", w.revoked, h.mgr.ServerStarted("snow"))
	}
	for _, s := range h.Servers() {
		if s.ID == "snow" && !strings.Contains(s.Auth, "needs login") {
			t.Errorf("after logout: %q", s.Auth)
		}
	}
}

// A token about to expire is refreshed before it is sent.
func TestOAuth_RefreshBeforeExpiry(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	w.expiresIn = 30 // inside oauthSkew: stale as soon as it is issued
	b := &browser{}
	oc := w.client(OAuthSpec{}, b)
	ctx := context.Background()
	if err := oc.ensure(ctx); err != nil {
		t.Fatal(err)
	}
	first := oc.tok.AccessToken
	v, err := oc.Authorization(ctx)
	if err != nil || v == "" || v == "Bearer "+first {
		t.Fatalf("authorization %q %v; first token %s", v, err, first)
	}
	if last := w.tokenForms[len(w.tokenForms)-1]; last.Get("grant_type") != "refresh_token" || last.Get("resource") != w.mcpURL() {
		t.Errorf("last token request: %v", last)
	}
	if b.count() != 1 {
		t.Errorf("a refresh must not open a login: %d", b.count())
	}
}

// A refused refresh on a 401 goes to the interactive login.
func TestOAuth_RefreshRefusedRunsLogin(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	b := &browser{}
	oc := w.client(OAuthSpec{}, b)
	ctx := context.Background()
	if err := oc.ensure(ctx); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.refuseRefresh = true
	w.mu.Unlock()
	oc.lastLogin = time.Time{} // not "just logged in"
	if err := oc.OnUnauthorized(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if b.count() != 2 {
		t.Errorf("logins shown: %d, want a second one", b.count())
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
	w := newOAuthWorld(t)
	spec := OAuthSpec{ClientID: w.clientID, RedirectPort: freePort(t)}
	shown := ""
	oc := newOAuthClient(Server{ID: "snow", URL: w.mcpURL(), OAuth: &spec}, func(_, u string) { shown = u }, nil)
	err := oc.ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "/mcp login snow") || shown == "" {
		t.Errorf("err %v, shown %q", err, shown)
	}
}

// With an issuer the server's metadata is not needed (or asked for), no
// resource is sent unless configured, and the ID token can be what is sent.
func TestOAuth_IssuerModeAndIDToken(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	w.withPRM, w.acceptIDToken = false, true
	b := &browser{}
	oc := w.client(OAuthSpec{Issuer: w.idp.URL, Scopes: []string{"openid", "email"}, Send: SendIDToken,
		AuthParams: map[string]string{"access_type": "offline"}}, b)
	ctx := context.Background()
	if err := oc.ensure(ctx); err != nil {
		t.Fatal(err)
	}
	q := w.authorizeQ[0]
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
	err := mgr.StartServer(ctx, mcpmgr.MCPConfig{ID: "sup", Name: "sup", URL: w.mcpURL(),
		Authorization: oc.Authorization, OnUnauthorized: oc.OnUnauthorized})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitServerTools(ctx, mgr, "sup", 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

// An id token whose nonce is not this login's is refused.
func TestOAuth_NonceMismatchRefused(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	b := &browser{}
	oc := w.client(OAuthSpec{}, b)
	w.idp.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/authorize" {
			q := r.URL.Query()
			q.Set("nonce", "someone-elses")
			r.URL.RawQuery = q.Encode()
		}
		w.serveIdP(rw, r)
	})
	if err := oc.ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "nonce") {
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
		w := newOAuthWorld(t)
		w.secret, w.authMethods = "s3:cr/et", c.methods
		b := &browser{}
		oc := w.client(OAuthSpec{ClientSecret: "${SNOW_SECRET}"}, b)
		if err := oc.ensure(context.Background()); err != nil {
			t.Fatalf("%v: %v", c.methods, err)
		}
		if got := w.clientAuth[0]; got != c.want {
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
		p, err := probeMCP(context.Background(), ts.URL+"/mcp")
		ts.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.want == "" && p.gate != "" || c.want != "" && !strings.Contains(p.gate, c.want) {
			t.Errorf("%s: gate %q, want %q", c.name, p.gate, c.want)
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
	_, err := h.AddServer(context.Background(), ServerSpec{ID: "gated", URL: ts.URL + "/mcp", Timeout: 5})
	if err == nil || !strings.Contains(err.Error(), "gated is behind a login gate") || !strings.Contains(err.Error(), "Cloudflare Access") {
		t.Errorf("got %v", err)
	}
}

// The oauth block round-trips through mcp.json, merges field by field across
// layers, and is refused where it cannot work.
func TestOAuthSpec_ConfigRoundTripAndMerge(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	in := ServerSpec{ID: "snow", URL: "https://acct.snowflakecomputing.com/api/v2/databases/d/schemas/s/mcp-servers/m",
		OAuth: &OAuthSpec{ClientID: "c1", ClientSecret: "$SNOW_SECRET", Scopes: []string{"session:role:ANALYST"},
			Resource: "none", Send: SendAccessToken, RedirectPort: 4000, AuthParams: map[string]string{"prompt": "consent"}}}
	if _, err := PutUserServer(in); err != nil {
		t.Fatal(err)
	}
	f, err := readServersFile(UserServersFile())
	if err != nil || len(f.Servers) != 1 {
		t.Fatal(f, err)
	}
	b1, _ := json.Marshal(in)
	b2, _ := json.Marshal(f.Servers[0])
	if string(b1) != string(b2) {
		t.Errorf("round trip:\n%s\n%s", b1, b2)
	}
	if !strings.Contains(string(b2), `"client_secret":"$SNOW_SECRET"`) {
		t.Errorf("the secret reference must be kept as written: %s", b2)
	}
	dir := t.TempDir()
	writeJSON(t, dir, ProjectServersFile, `{"servers":[{"id":"snow","oauth":{"issuer":"https://idp.example","scopes":["a","b"]}}]}`)
	got, err := LoadServers(dir, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	o := byID(got)["snow"].OAuth
	if o == nil || o.ClientID != "c1" || o.Issuer != "https://idp.example" || !slices.Equal(o.Scopes, []string{"a", "b"}) || o.RedirectPort != 4000 {
		t.Errorf("merged %+v", o)
	}
	for _, c := range []struct {
		s    ServerSpec
		via  string
		want string
	}{
		{ServerSpec{ID: "x", Command: "x", OAuth: &OAuthSpec{ClientID: "c"}}, "", "remote server"},
		{ServerSpec{ID: "x", URL: "https://x", Bearer: "t", OAuth: &OAuthSpec{ClientID: "c"}}, "", "both bearer and oauth"},
		{ServerSpec{ID: "x", URL: "https://x", OAuth: &OAuthSpec{ClientID: "c", Send: "jwt"}}, "", "send"},
	} {
		if err := checkOAuth(c.s, c.via); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.s, err, c.want)
		}
	}
}
