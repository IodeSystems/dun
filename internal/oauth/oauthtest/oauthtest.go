// Package oauthtest is a fake identity provider and the MCP server that
// trusts it, for tests of dun's OAuth client (internal/oauth) and of the
// harness that uses it.
package oauthtest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// World is an identity provider and an MCP server that trusts it: the
// server 401s with resource_metadata, serves its protected-resource
// metadata, and accepts only tokens the IdP issued.
type World struct {
	IdP, MCP *httptest.Server
	ClientID string

	Mu            sync.Mutex
	N             int
	Valid         map[string]bool // bearer values the MCP server accepts
	Codes         map[string]url.Values
	ExpiresIn     int64
	RefuseRefresh bool
	Secret        string   // the client secret the IdP expects; "" = public client
	AuthMethods   []string // token_endpoint_auth_methods_supported
	WithPRM       bool     // the MCP server names its metadata
	AcceptIDToken bool     // the MCP server takes the ID token, not the access token
	AuthorizeQ    []url.Values
	TokenForms    []url.Values
	ClientAuth    []string // how each token request authenticated: none, basic, post
	Revoked       []string
	MCPAuth       []string // the Authorization of each MCP request
	CIMD          bool     // client_id_metadata_document_supported
	DCR           bool     // a registration_endpoint (RFC 7591)
	Registered    []map[string]any
}

// NewWorld starts both servers, closed when t ends.
func NewWorld(t *testing.T) *World {
	w := &World{ClientID: "dun-client", Valid: map[string]bool{}, Codes: map[string]url.Values{},
		ExpiresIn: 3600, WithPRM: true}
	w.IdP = httptest.NewServer(http.HandlerFunc(w.ServeIdP))
	w.MCP = httptest.NewServer(http.HandlerFunc(w.serveMCP))
	t.Cleanup(func() { w.IdP.Close(); w.MCP.Close() })
	return w
}

func (w *World) MCPURL() string { return w.MCP.URL + "/mcp" }

// ServeIdP is the identity provider's handler, for a test that wraps it.
func (w *World) ServeIdP(rw http.ResponseWriter, r *http.Request) {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server":
		md := map[string]any{"issuer": w.IdP.URL,
			"authorization_endpoint": w.IdP.URL + "/authorize", "token_endpoint": w.IdP.URL + "/token",
			"revocation_endpoint":                   w.IdP.URL + "/revoke",
			"token_endpoint_auth_methods_supported": w.AuthMethods,
			"code_challenge_methods_supported":      []string{"S256"},
			"client_id_metadata_document_supported": w.CIMD}
		if w.DCR {
			md["registration_endpoint"] = w.IdP.URL + "/register"
		}
		writeJSONResp(rw, md)
	case "/register":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Registered = append(w.Registered, body)
		w.ClientID = fmt.Sprintf("reg-%d", len(w.Registered))
		rw.WriteHeader(http.StatusCreated)
		writeJSONResp(rw, map[string]any{"client_id": w.ClientID})
	case "/authorize":
		q := r.URL.Query()
		w.AuthorizeQ = append(w.AuthorizeQ, q)
		if q.Get("client_id") != w.ClientID || q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" {
			http.Error(rw, "bad authorize request", http.StatusBadRequest)
			return
		}
		w.N++
		code := fmt.Sprintf("code-%d", w.N)
		w.Codes[code] = q
		http.Redirect(rw, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	case "/token":
		_ = r.ParseForm()
		f := r.PostForm
		w.TokenForms = append(w.TokenForms, f)
		how := "none"
		if u, p, ok := r.BasicAuth(); ok {
			how = "basic"
			if u != w.ClientID || p != url.QueryEscape(w.Secret) {
				tokenErr(rw, "invalid_client")
				return
			}
		} else if f.Get("client_secret") != "" {
			how = "post"
			if f.Get("client_secret") != w.Secret {
				tokenErr(rw, "invalid_client")
				return
			}
		}
		w.ClientAuth = append(w.ClientAuth, how)
		if w.Secret != "" && how == "none" {
			tokenErr(rw, "invalid_client")
			return
		}
		var q url.Values
		switch f.Get("grant_type") {
		case "authorization_code":
			var ok bool
			q, ok = w.Codes[f.Get("code")]
			delete(w.Codes, f.Get("code"))
			sum := sha256.Sum256([]byte(f.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") || f.Get("redirect_uri") != q.Get("redirect_uri") {
				tokenErr(rw, "invalid_grant")
				return
			}
		case "refresh_token":
			if w.RefuseRefresh || !strings.HasPrefix(f.Get("refresh_token"), "rt-") {
				tokenErr(rw, "invalid_grant")
				return
			}
			q = url.Values{"scope": {"openid email"}}
		default:
			tokenErr(rw, "unsupported_grant_type")
			return
		}
		w.N++
		resp := map[string]any{"access_token": fmt.Sprintf("at-%d", w.N), "token_type": "Bearer",
			"expires_in": w.ExpiresIn, "refresh_token": fmt.Sprintf("rt-%d", w.N)}
		w.Valid[fmt.Sprintf("at-%d", w.N)] = !w.AcceptIDToken
		if strings.Contains(q.Get("scope"), "openid") {
			idt := FakeJWT(map[string]any{"iss": w.IdP.URL, "sub": "u1", "email": "ann@example.com",
				"aud": w.ClientID, "exp": time.Now().Add(time.Hour).Unix(), "nonce": q.Get("nonce")})
			resp["id_token"] = idt
			w.Valid[idt] = w.AcceptIDToken
		}
		writeJSONResp(rw, resp)
	case "/revoke":
		_ = r.ParseForm()
		w.Revoked = append(w.Revoked, r.PostForm.Get("token"))
	default:
		http.NotFound(rw, r)
	}
}

func (w *World) serveMCP(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/.well-known/oauth-protected-resource/mcp" {
		writeJSONResp(rw, map[string]any{"resource": w.MCPURL(), "authorization_servers": []string{w.IdP.URL},
			"scopes_supported": []string{"openid", "email"}})
		return
	}
	authz := r.Header.Get("Authorization")
	w.Mu.Lock()
	w.MCPAuth = append(w.MCPAuth, authz)
	ok := w.Valid[strings.TrimPrefix(authz, "Bearer ")]
	withPRM := w.WithPRM
	w.Mu.Unlock()
	if !ok {
		if withPRM {
			rw.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+w.MCP.URL+`/.well-known/oauth-protected-resource/mcp", scope="openid email"`)
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

func FakeJWT(claims map[string]any) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".sig"
}

// freePort is a loopback port nothing listens on, for the redirect URI.
func FreePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// browser stands in for the person: it opens each login URL it is shown —
// the IdP redirects it to the loopback callback — and counts them.
type Browser struct {
	mu   sync.Mutex
	urls []string
}

func (b *Browser) Notify(_ string, authURL string) {
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

func (b *Browser) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.urls)
}

// URLs are the login URLs b was shown, in order.
func (b *Browser) URLs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.urls)
}
