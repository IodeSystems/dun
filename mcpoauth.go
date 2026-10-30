package dun

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/mcpmgr/mcpauth"
)

// OAuth for remote MCP servers (ServerSpec.OAuth).
//
// agentkit's mcpmgr asks for the Authorization header on every request and
// runs a hook on a 401; mcpauth has the discovery steps. Everything that
// touches the user and the disk is here:
//
//   - discovery: the configured issuer, or the 401's resource_metadata →
//     protected-resource metadata (RFC 9728) → its first authorization server
//     → that server's metadata (RFC 8414 / OpenID discovery);
//   - the authorization-code flow with PKCE (S256), a state, and a nonce when
//     openid is asked for, redirected to a loopback listener on a FIXED port
//     (http://127.0.0.1:<port>/oauth/callback) so it can be registered
//     exactly; with no client_id, dun picks the client (oauthauto.go);
//   - the login URL is SURFACED (Config.OnLogin → an mcp_login event) as well
//     as opened in a browser: the engine runs under dund, often with no
//     display, and the person is wherever the TUI is;
//   - the token is kept in $DUN_HOME/oauth/<id>.json, 0600, with the token
//     endpoint it came from, so a refresh needs no discovery;
//   - refresh before expiry (in Authorization, with oauthSkew) and on a 401;
//     when refresh is impossible, the interactive login again — bounded by
//     oauthLoginTimeout, never forever.

// DefaultOAuthRedirectPort is the loopback port of the redirect URI when a
// spec names none. Fixed rather than random because Snowflake and others
// match the registered redirect URI exactly.
const DefaultOAuthRedirectPort = 33418

// oauthLoginTimeout bounds one interactive login: the person has this long
// to finish in the browser. A var for tests.
var oauthLoginTimeout = 5 * time.Minute

// oauthSkew refreshes a token this long before it expires, so a request does
// not set out with a token that expires on the way.
const oauthSkew = 60 * time.Second

// OAuthDir holds one token file per server: $DUN_HOME/oauth.
func OAuthDir() string {
	return filepath.Join(filepath.Dir(SessionsDir()), "oauth")
}

// oauthToken is a server's token file.
type oauthToken struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitzero"` // the access token's; zero = not stated
	Scope        string    `json:"scope,omitempty"`
	// How it was obtained: what a refresh, a revoke and a config check need.
	ClientID           string `json:"client_id"`
	Issuer             string `json:"issuer,omitempty"`
	TokenEndpoint      string `json:"token_endpoint"`
	RevocationEndpoint string `json:"revocation_endpoint,omitempty"`
	AuthMethod         string `json:"auth_method"` // none | client_secret_basic | client_secret_post
	Resource           string `json:"resource,omitempty"`
}

// idClaims are the ID-token claims dun reads. The token came straight from
// the token endpoint over TLS, so its signature is not checked (OpenID
// Connect Core 3.1.3.7); the server that receives it checks it.
type idClaims struct {
	Iss   string   `json:"iss"`
	Sub   string   `json:"sub"`
	Email string   `json:"email"`
	Exp   int64    `json:"exp"`
	Nonce string   `json:"nonce"`
	Aud   audience `json:"aud"`
}

// audience is a JWT aud: a string or a list.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// parseIDToken reads a JWT's claims without verifying it.
func parseIDToken(jwt string) (idClaims, error) {
	var c idClaims
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return c, fmt.Errorf("the id_token is not a JWT")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return c, fmt.Errorf("the id_token's claims: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("the id_token's claims: %w", err)
	}
	return c, nil
}

// oauthClient logs in to one server and hands out its token.
type oauthClient struct {
	id, url string
	spec    OAuthSpec
	path    string
	hc      *http.Client
	// notify surfaces the login URL to the person; open tries a browser.
	// Either may be nil.
	notify func(id, authURL string)
	open   func(string) error
	now    func() time.Time

	mu     sync.Mutex
	tok    *oauthToken
	tokMod time.Time // the file's mtime when tok was read
	// deadRefresh is a refresh token the server refused: not tried again.
	deadRefresh string
	// auto is the client dun chose when the spec names none (oauthauto.go):
	// its URL, or a registration.
	auto      autoClient
	lastLogin time.Time

	loginMu   sync.Mutex  // one interactive login at a time
	loggingIn atomic.Bool // a login is waiting on the person
}

func newOAuthClient(s Server, notify func(id, authURL string), open func(string) error) *oauthClient {
	return &oauthClient{id: s.ID, url: s.URL, spec: *s.OAuth,
		path:   filepath.Join(OAuthDir(), s.ID+".json"),
		hc:     &http.Client{Timeout: 30 * time.Second},
		notify: notify, open: open, now: time.Now}
}

func (c *oauthClient) send() string {
	if c.spec.Send == SendIDToken {
		return SendIDToken
	}
	return SendAccessToken
}

// value is the token that goes in Authorization, and when it expires.
func (c *oauthClient) value(t *oauthToken) (string, time.Time) {
	if c.send() == SendIDToken {
		if t.IDToken == "" {
			return "", time.Time{}
		}
		var exp time.Time
		if cl, err := parseIDToken(t.IDToken); err == nil && cl.Exp > 0 {
			exp = time.Unix(cl.Exp, 0)
		}
		return t.IDToken, exp
	}
	return t.AccessToken, t.Expiry
}

func (c *oauthClient) fresh(t *oauthToken) bool {
	v, exp := c.value(t)
	return v != "" && (exp.IsZero() || c.now().Add(oauthSkew).Before(exp))
}

func (c *oauthClient) canRefresh(t *oauthToken) bool {
	return t != nil && t.RefreshToken != "" && t.RefreshToken != c.deadRefresh && t.TokenEndpoint != ""
}

// current is the stored token, re-read when the file changed (another
// session refreshed it, or /mcp logout removed it). A token for another
// client or issuer than the spec names is not this config's: nil. Caller
// holds mu.
func (c *oauthClient) current() *oauthToken {
	fi, err := os.Stat(c.path)
	if err != nil {
		c.tok, c.tokMod = nil, time.Time{}
		return nil
	}
	if c.tok == nil || !fi.ModTime().Equal(c.tokMod) {
		c.tok, c.tokMod = nil, fi.ModTime()
		b, err := os.ReadFile(c.path)
		if err != nil {
			return nil
		}
		var t oauthToken
		if json.Unmarshal(b, &t) != nil {
			return nil
		}
		c.tok = &t
	}
	t := c.tok
	if c.spec.ClientID == "" && t.ClientID != "" && c.auto.ID == "" {
		c.auto = loadAutoClient(c.id, t.ClientID)
	}
	if t.ClientID != c.clientID() ||
		(c.spec.Issuer != "" && strings.TrimSuffix(t.Issuer, "/") != strings.TrimSuffix(c.spec.Issuer, "/")) {
		return nil
	}
	return t
}

// save writes t, 0600, and makes it current. Caller holds mu.
func (c *oauthClient) save(t *oauthToken) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(c.path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	c.tok = t
	if fi, err := os.Stat(c.path); err == nil {
		c.tokMod = fi.ModTime()
	}
	return nil
}

// Authorization is mcpmgr's per-request hook: "Bearer <token>", refreshed
// first when it is about to expire. With no usable token it sends none, and
// the server's 401 runs OnUnauthorized.
func (c *oauthClient) Authorization(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.current()
	if t == nil {
		return "", nil
	}
	if c.fresh(t) {
		v, _ := c.value(t)
		return "Bearer " + v, nil
	}
	if c.canRefresh(t) {
		// Just issued: good until it expires, even when its whole life is
		// shorter than the skew.
		if nt, err := c.refresh(ctx, t); err == nil {
			if v, exp := c.value(nt); v != "" && (exp.IsZero() || c.now().Before(exp)) {
				return "Bearer " + v, nil
			}
		}
	}
	return "", nil
}

// OnUnauthorized is mcpmgr's 401 hook: refresh, else log in again.
func (c *oauthClient) OnUnauthorized(ctx context.Context, wwwAuthenticate string) error {
	c.mu.Lock()
	t := c.current()
	refreshed := false
	if c.canRefresh(t) {
		_, err := c.refresh(ctx, t)
		refreshed = err == nil
	}
	justLoggedIn := !c.lastLogin.IsZero() && c.now().Sub(c.lastLogin) < 30*time.Second
	c.mu.Unlock()
	if refreshed {
		return nil
	}
	// A server that refuses the token it was just given will refuse the
	// next one too: say so rather than open the browser on every request.
	if justLoggedIn {
		return fmt.Errorf("%s refused the token from the login just made — check oauth scopes, audience, resource and send", c.id)
	}
	return c.Login(ctx, wwwAuthenticate)
}

// ensure gets a usable token before the server is connected, so the
// handshake does not wait out a login inside its own timeout. A server that
// answers without a token needs none.
func (c *oauthClient) ensure(ctx context.Context) error {
	if v, _ := c.Authorization(ctx); v != "" {
		return nil
	}
	www := ""
	if c.spec.Issuer == "" {
		p, err := probeMCP(ctx, c.url)
		if err != nil {
			return err
		}
		if p.gate != "" {
			return errors.New(p.gate)
		}
		if p.status/100 == 2 {
			return nil
		}
		www = p.www
	}
	return c.Login(ctx, www)
}

// asInfo is what a login found by discovery.
type asInfo struct {
	md       *mcpauth.AuthServerMetadata
	resource string // the resource parameter to send; "" sends none
	scopes   []string
}

// discover finds the authorization server: the spec's issuer, or the one the
// server names. wwwAuthenticate is the 401's; empty probes for it.
func (c *oauthClient) discover(ctx context.Context, wwwAuthenticate string) (asInfo, error) {
	if c.spec.Issuer != "" {
		md, err := mcpauth.FetchAuthServerMetadata(ctx, c.hc, c.spec.Issuer)
		if err != nil {
			return asInfo{}, fmt.Errorf("issuer %s: %w", c.spec.Issuer, err)
		}
		return asInfo{md: md, resource: c.resourceParam(""), scopes: c.spec.Scopes}, nil
	}
	if wwwAuthenticate == "" {
		p, err := probeMCP(ctx, c.url)
		if err != nil {
			return asInfo{}, err
		}
		if p.gate != "" {
			return asInfo{}, errors.New(p.gate)
		}
		wwwAuthenticate = p.www
	}
	ch, _ := mcpauth.BearerChallenge(wwwAuthenticate)
	mdURL := ch.Params["resource_metadata"]
	if mdURL == "" {
		var err error
		if mdURL, err = mcpauth.ResourceMetadataURL(c.url); err != nil {
			return asInfo{}, err
		}
	}
	prm, err := mcpauth.FetchResourceMetadata(ctx, c.hc, mdURL, "")
	if err != nil {
		return asInfo{}, fmt.Errorf("%s names no authorization server (%w) — set oauth.issuer to log in with a provider directly", c.id, err)
	}
	if prm.Resource != "" && !coversResource(prm.Resource, c.url) {
		return asInfo{}, fmt.Errorf("%s describes resource %q, not %s — refusing to send a login for one server to another", mdURL, prm.Resource, c.url)
	}
	if len(prm.AuthorizationServers) == 0 {
		return asInfo{}, fmt.Errorf("%s lists no authorization_servers — set oauth.issuer", mdURL)
	}
	md, err := mcpauth.FetchAuthServerMetadata(ctx, c.hc, prm.AuthorizationServers[0])
	if err != nil {
		return asInfo{}, err
	}
	scopes := c.spec.Scopes
	if len(scopes) == 0 {
		scopes = strings.Fields(ch.Params["scope"])
	}
	if len(scopes) == 0 {
		scopes = prm.ScopesSupported
	}
	resource := prm.Resource
	if resource == "" {
		resource = c.url
	}
	return asInfo{md: md, resource: c.resourceParam(resource), scopes: scopes}, nil
}

// coversResource: the metadata's resource is the server's URL, or a prefix of
// it on a path boundary (a server may describe its whole origin). Trailing
// slashes are not significant — servers disagree about them.
func coversResource(resource, serverURL string) bool {
	r, u := strings.TrimSuffix(resource, "/"), strings.TrimSuffix(serverURL, "/")
	return r == u || strings.HasPrefix(u, r+"/")
}

// resourceParam applies the spec's resource setting to the discovered one.
func (c *oauthClient) resourceParam(discovered string) string {
	switch c.spec.Resource {
	case "none":
		return ""
	case "":
		return discovered
	}
	return c.spec.Resource
}

func (c *oauthClient) redirectPort() int {
	if c.spec.RedirectPort > 0 {
		return c.spec.RedirectPort
	}
	return DefaultOAuthRedirectPort
}

// clientSecret resolves the spec's secret: "$NAME" or "${NAME}" from the
// environment, anything else as written.
func (c *oauthClient) clientSecret() string {
	if c.spec.ClientID == "" {
		return c.auto.Secret // a registration's, when it was given one
	}
	s := c.spec.ClientSecret
	if m := envRef.FindStringSubmatch(s); m != nil {
		return os.Getenv(m[1] + m[2])
	}
	return s
}

// authMethod picks how the client authenticates at the token endpoint: a
// public client sends its id; a confidential one uses basic when the server
// allows it (RFC 8414's default when it lists nothing), else post.
func (c *oauthClient) authMethod(supported []string) string {
	if c.clientSecret() == "" {
		return "none"
	}
	if len(supported) == 0 || slices.Contains(supported, "client_secret_basic") {
		return "client_secret_basic"
	}
	if slices.Contains(supported, "client_secret_post") {
		return "client_secret_post"
	}
	return "client_secret_basic"
}

// Login runs the interactive flow and stores the token. wwwAuthenticate is
// the 401 that asked for it, or "".
func (c *oauthClient) Login(ctx context.Context, wwwAuthenticate string) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	c.loggingIn.Store(true)
	defer c.loggingIn.Store(false)
	ctx, cancel := context.WithTimeout(ctx, oauthLoginTimeout)
	defer cancel()
	as, err := c.discover(ctx, wwwAuthenticate)
	if err != nil {
		return err
	}
	md := as.md
	if md.AuthorizationEndpoint == "" || md.TokenEndpoint == "" {
		return fmt.Errorf("%s: its metadata has no authorization or token endpoint", md.Issuer)
	}
	port := c.redirectPort()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return c.waitOtherLogin(ctx, port, err)
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", port)
	if c.spec.ClientID == "" {
		ac, err := c.chooseClient(ctx, md, redirect, port)
		if err != nil {
			ln.Close()
			return err
		}
		c.mu.Lock()
		c.auto = ac
		c.mu.Unlock()
	}
	verifier, state := randomToken(32), randomToken(16)
	nonce := ""
	if slices.Contains(as.scopes, "openid") {
		nonce = randomToken(16)
	}
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.clientID()},
		"redirect_uri":          {redirect},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	if len(as.scopes) > 0 {
		q.Set("scope", strings.Join(as.scopes, " "))
	}
	if nonce != "" {
		q.Set("nonce", nonce)
	}
	if as.resource != "" {
		q.Set("resource", as.resource)
	}
	if c.spec.Audience != "" {
		q.Set("audience", c.spec.Audience)
	}
	for k, v := range c.spec.AuthParams {
		q.Set(k, v)
	}
	sep := "?"
	if strings.Contains(md.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	authURL := md.AuthorizationEndpoint + sep + q.Encode()

	type result struct {
		code string
		err  error
	}
	got := make(chan result, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/callback" {
			http.NotFound(w, r)
			return
		}
		p := r.URL.Query()
		if p.Get("state") != state {
			// Not this login's redirect: answer it, and keep waiting.
			http.Error(w, "dun: this login link is stale — use the newest one", http.StatusBadRequest)
			return
		}
		var res result
		switch {
		case p.Get("error") != "":
			res.err = fmt.Errorf("the identity provider refused the login: %s %s", p.Get("error"), p.Get("error_description"))
		case p.Get("code") == "":
			res.err = fmt.Errorf("the identity provider redirected back without a code")
		default:
			res.code = p.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		msg := "Logged in to " + c.id + ". You can close this tab."
		if res.err != nil {
			msg = "Login to " + c.id + " failed: " + res.err.Error()
		}
		fmt.Fprintf(w, "<!doctype html><title>dun</title><p>%s</p>", html.EscapeString(msg))
		select {
		case got <- res:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	if c.notify != nil {
		c.notify(c.id, authURL)
	}
	if c.open != nil {
		_ = c.open(authURL) // best effort: the notified URL is the way that always works
	}
	var res result
	select {
	case res = <-got:
	case <-ctx.Done():
		return fmt.Errorf("login to %s not finished within %s — /mcp login %s to try again", c.id, oauthLoginTimeout, c.id)
	}
	if res.err != nil {
		return res.err
	}
	method := c.authMethod(md.TokenEndpointAuthMethodsSupported)
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	}
	if as.resource != "" {
		form.Set("resource", as.resource)
	}
	tr, err := c.tokenRequest(ctx, md.TokenEndpoint, method, form)
	if err != nil {
		return err
	}
	t := &oauthToken{ClientID: c.clientID(), Issuer: md.Issuer, TokenEndpoint: md.TokenEndpoint,
		RevocationEndpoint: md.RevocationEndpoint, AuthMethod: method, Resource: as.resource}
	tr.apply(t, c.now())
	if t.IDToken != "" {
		cl, err := parseIDToken(t.IDToken)
		if err != nil {
			return err
		}
		if nonce != "" && cl.Nonce != nonce {
			return fmt.Errorf("the id_token's nonce does not match this login — refusing it")
		}
		if !slices.Contains(cl.Aud, c.clientID()) {
			return fmt.Errorf("the id_token is for %v, not client %s — refusing it", []string(cl.Aud), c.clientID())
		}
	}
	if v, _ := c.value(t); v == "" {
		return fmt.Errorf("the token endpoint returned no %s (send = %s; for an id_token, ask for the openid scope)", c.send(), c.send())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadRefresh, c.lastLogin = "", c.now()
	return c.save(t)
}

// waitOtherLogin: the callback port is taken — most likely another session
// logging in to the same server, whose token lands in the same file. Wait
// for it rather than fail; if no token comes, the port is just busy.
func (c *oauthClient) waitOtherLogin(ctx context.Context, port int, bindErr error) error {
	if c.notify != nil {
		c.notify(c.id, "")
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		c.mu.Lock()
		t := c.current()
		ok := t != nil && c.fresh(t)
		c.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the login callback port %d is in use (%v) and no other login finished — free it, or set oauth.redirect_port (and register that redirect URI)", port, bindErr)
		case <-tick.C:
		}
	}
}

// tokenResponse is a token endpoint's answer, success or error.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// apply copies a response into t, keeping what it does not replace (a
// refresh often returns no new refresh or ID token).
func (r tokenResponse) apply(t *oauthToken, now time.Time) {
	t.AccessToken, t.TokenType = r.AccessToken, r.TokenType
	t.Expiry = time.Time{}
	if r.ExpiresIn > 0 {
		t.Expiry = now.Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	if r.RefreshToken != "" {
		t.RefreshToken = r.RefreshToken
	}
	if r.IDToken != "" {
		t.IDToken = r.IDToken
	}
	if r.Scope != "" {
		t.Scope = r.Scope
	}
}

// postForm POSTs form to an endpoint of the authorization server with the
// client's authentication, and returns the status and body.
func (c *oauthClient) postForm(ctx context.Context, endpoint, method string, form url.Values) (int, []byte, error) {
	switch method {
	case "none":
		form.Set("client_id", c.clientID())
	case "client_secret_post":
		form.Set("client_id", c.clientID())
		form.Set("client_secret", c.clientSecret())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if method == "client_secret_basic" {
		// RFC 6749 §2.3.1: form-encoded before base64.
		req.SetBasicAuth(url.QueryEscape(c.clientID()), url.QueryEscape(c.clientSecret()))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

// tokenRequest asks the token endpoint for a token. Errors carry the
// server's error code, never a token.
func (c *oauthClient) tokenRequest(ctx context.Context, endpoint, method string, form url.Values) (tokenResponse, error) {
	var tr tokenResponse
	status, b, err := c.postForm(ctx, endpoint, method, form)
	if err != nil {
		return tr, fmt.Errorf("token endpoint %w", err)
	}
	jerr := json.Unmarshal(b, &tr)
	if status != http.StatusOK || tr.Error != "" {
		why := tr.Error
		if tr.ErrorDescription != "" {
			why += ": " + tr.ErrorDescription
		}
		if why == "" {
			why = strings.TrimSpace(string(b[:min(len(b), 200)]))
		}
		return tr, fmt.Errorf("token endpoint %s: HTTP %d %s", endpoint, status, why)
	}
	if jerr != nil {
		return tr, fmt.Errorf("token endpoint %s: %w", endpoint, jerr)
	}
	return tr, nil
}

// refresh trades the refresh token for a new token and stores it. A refusal
// marks the refresh token dead, so the next 401 goes to a login. Caller
// holds mu.
func (c *oauthClient) refresh(ctx context.Context, t *oauthToken) (*oauthToken, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}}
	if t.Resource != "" {
		form.Set("resource", t.Resource)
	}
	tr, err := c.tokenRequest(ctx, t.TokenEndpoint, t.AuthMethod, form)
	if err != nil {
		c.deadRefresh = t.RefreshToken
		return nil, err
	}
	nt := *t
	if c.send() == SendIDToken && tr.IDToken == "" {
		// The old ID token is the expired one: nothing fresh to send.
		c.deadRefresh = t.RefreshToken
		return nil, fmt.Errorf("the refresh returned no id_token")
	}
	tr.apply(&nt, c.now())
	if err := c.save(&nt); err != nil {
		return nil, err
	}
	return &nt, nil
}

// Logout deletes the stored token and, when the server has a revocation
// endpoint, revokes it (best effort: the file is gone either way). It
// reports whether anything was revoked.
func (c *oauthClient) Logout(ctx context.Context) (bool, error) {
	c.mu.Lock()
	t := c.current()
	if t == nil {
		// A token for another client id is still this server's to delete.
		if b, err := os.ReadFile(c.path); err == nil {
			var old oauthToken
			if json.Unmarshal(b, &old) == nil {
				t = &old
			}
		}
	}
	err := os.Remove(c.path)
	c.tok, c.tokMod, c.deadRefresh = nil, time.Time{}, ""
	c.mu.Unlock()
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if t == nil || t.RevocationEndpoint == "" {
		return false, nil
	}
	revoked := false
	for _, tok := range []struct{ v, hint string }{{t.RefreshToken, "refresh_token"}, {t.AccessToken, "access_token"}} {
		if tok.v == "" {
			continue
		}
		form := url.Values{"token": {tok.v}, "token_type_hint": {tok.hint}}
		if status, _, err := c.postForm(ctx, t.RevocationEndpoint, t.AuthMethod, form); err == nil && status == http.StatusOK {
			revoked = true
		}
	}
	return revoked, nil
}

// OAuthStatus is a server's login as the stored token describes it.
type OAuthStatus struct {
	LoggedIn bool      // a token is stored for this client
	Pending  bool      // a login is waiting on the person
	Fresh    bool      // and it is not expired
	Refresh  bool      // and it can be refreshed
	Who      string    // the ID token's email, else its sub
	Until    time.Time // when the token sent expires; zero = not stated
}

// status reads the stored token, without any network.
func (c *oauthClient) status() OAuthStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.current()
	if t == nil {
		return OAuthStatus{Pending: c.loggingIn.Load()}
	}
	st := OAuthStatus{LoggedIn: true, Fresh: c.fresh(t), Refresh: c.canRefresh(t)}
	_, st.Until = c.value(t)
	if t.IDToken != "" {
		if cl, err := parseIDToken(t.IDToken); err == nil {
			st.Who = cl.Email
			if st.Who == "" {
				st.Who = cl.Sub
			}
		}
	}
	return st
}

// String is the /mcp listing's auth column.
func (s OAuthStatus) String() string {
	who := ""
	if s.Who != "" {
		who = " as " + s.Who
	}
	switch {
	case !s.LoggedIn && s.Pending:
		return "oauth: login waiting — open the URL shown"
	case !s.LoggedIn:
		return "oauth: needs login (/mcp login)"
	case s.Fresh && !s.Until.IsZero():
		return "oauth: logged in" + who + " until " + s.Until.Local().Format("Jan 2 15:04")
	case s.Fresh:
		return "oauth: logged in" + who
	case s.Refresh:
		return "oauth: logged in" + who + " · token expired, refreshes on next use"
	}
	return "oauth: login expired (/mcp login)"
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
