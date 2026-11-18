package dundapi_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/internal/dundapi"
)

type rig struct {
	h   http.Handler
	tok string
}

func newRig(t *testing.T, trust ...string) rig {
	t.Helper()
	dir := t.TempDir()
	svc, err := dund.New(dir, &fakeSpawner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	var ps []netip.Prefix
	for _, s := range trust {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	h, err := dundapi.Handler(svc, dundapi.Options{Owner: "unix:1000", TrustedProxies: func() []netip.Prefix { return ps }})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := dund.NewToken(dir)
	return rig{h: h, tok: tok}
}

// req sends one request as a peer at from, to host, with headers.
func (g rig) req(method, target, from, host string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr, r.Host = from, host
	if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.h.ServeHTTP(w, r)
	return w
}

func (g rig) signin(from, host string, hdr map[string]string) *http.Cookie {
	w := g.req("POST", "/auth/signin", from, host, hdr, url.Values{"token": {g.tok}, "return": {"/"}}.Encode())
	for _, c := range w.Result().Cookies() {
		if strings.HasSuffix(c.Name, "dun_session") {
			return c
		}
	}
	return nil
}

// Over https from a trusted proxy the cookie is __Host- and Secure, with no
// Domain; from an untrusted peer the same headers are ignored.
func TestCookieOverHTTPSFromATrustedProxy(t *testing.T) {
	g := newRig(t, "192.0.2.0/24")
	https := map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "dun.example.com"}
	for _, from := range []string{"127.0.0.1:5000", "192.0.2.7:5000"} {
		c := g.signin(from, "127.0.0.1:20003", https)
		if c == nil || c.Name != "__Host-dun_session" || !c.Secure || c.Domain != "" || c.Path != "/" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
			t.Errorf("from %s: %+v", from, c)
		}
	}
	c := g.signin("198.51.100.9:5000", "198.51.100.1:20003", https)
	if c == nil || c.Name != "dun_session" || c.Secure {
		t.Fatalf("X-Forwarded-Proto believed from an untrusted peer: %+v", c)
	}
	// ... and its session is bound to Host, not the claimed X-Forwarded-Host.
	ck := map[string]string{"Cookie": c.Name + "=" + c.Value}
	if w := g.req("GET", "/api/v1/sessions", "198.51.100.9:5000", "198.51.100.1:20003", ck, ""); w.Code != 200 {
		t.Errorf("on its host: %d", w.Code)
	}
	ck["X-Forwarded-Host"] = "dun.example.com"
	if w := g.req("GET", "/api/v1/sessions", "198.51.100.9:5000", "dun.example.com", ck, ""); w.Code != 401 {
		t.Errorf("under another Host: %d", w.Code)
	}
}

// A session is good on the host it was made on only.
func TestSessionIsBoundToItsHost(t *testing.T) {
	g := newRig(t)
	c := g.signin("192.0.2.5:5000", "dun.lan:20003", nil)
	if c == nil {
		t.Fatal("no cookie")
	}
	ck := map[string]string{"Cookie": c.Name + "=" + c.Value}
	if w := g.req("GET", "/api/v1/sessions", "192.0.2.5:5000", "dun.lan:20003", ck, ""); w.Code != 200 {
		t.Fatalf("same host: %d", w.Code)
	}
	for _, other := range []string{"dun.lan:20004", "192.0.2.10:20003", "other.lan:20003"} {
		if w := g.req("GET", "/api/v1/sessions", "192.0.2.5:5000", other, ck, ""); w.Code != 401 {
			t.Errorf("%s: %d, want 401", other, w.Code)
		}
	}
	// Nor is a session a bearer.
	if w := g.req("GET", "/api/v1/sessions", "192.0.2.5:5000", "dun.lan:20003", map[string]string{"Authorization": "Bearer " + c.Value}, ""); w.Code != 401 {
		t.Errorf("session as a bearer: %d", w.Code)
	}
}

// localhost, 127.0.0.1 and ::1 over plain http go to dun.localhost before a
// sign-in; the API stays where it is; https through a proxy is left alone.
func TestSharedLoopbackGoesToDunLocalhost(t *testing.T) {
	g := newRig(t)
	for _, host := range []string{"127.0.0.1:20003", "localhost:20003", "[::1]:20003", "127.0.0.2:20003"} {
		w := g.req("GET", "/auth/signin?return=%2Ftui%2F", "127.0.0.1:5000", host, nil, "")
		if w.Code != http.StatusFound || w.Header().Get("Location") != "http://dun.localhost:20003/auth/signin?return=%2Ftui%2F" {
			t.Errorf("%s: %d %s", host, w.Code, w.Header().Get("Location"))
		}
	}
	if w := g.req("POST", "/auth/signin", "127.0.0.1:5000", "127.0.0.1:20003", nil, url.Values{"token": {g.tok}}.Encode()); w.Code != http.StatusBadRequest || len(w.Result().Cookies()) != 0 {
		t.Errorf("sign-in on 127.0.0.1: %d", w.Code)
	}
	if w := g.req("GET", "/api/v1/hello", "127.0.0.1:5000", "127.0.0.1:20003", nil, ""); w.Code != 200 {
		t.Errorf("api on 127.0.0.1: %d", w.Code)
	}
	if w := g.req("GET", "/auth/signin", "127.0.0.1:5000", "dun.localhost:20003", nil, ""); w.Code != 200 {
		t.Errorf("dun.localhost: %d", w.Code)
	}
	if w := g.req("GET", "/auth/signin", "127.0.0.1:5000", "localhost:20003", map[string]string{"X-Forwarded-Proto": "https"}, ""); w.Code != 200 {
		t.Errorf("https through a proxy on the box: %d", w.Code)
	}
}

// A bare LAN IP over plain http is allowed, with a warning on the sign-in
// page and the chooser.
func TestBareIPWarns(t *testing.T) {
	g := newRig(t)
	w := g.req("GET", "/auth/signin", "192.0.2.5:5000", "192.0.2.10:20003", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "other servers on 192.0.2.10 receive this sign-in") {
		t.Errorf("sign-in page: %d", w.Code)
	}
	c := g.signin("192.0.2.5:5000", "192.0.2.10:20003", nil)
	if c == nil {
		t.Fatal("sign-in on a bare IP refused")
	}
	w = g.req("GET", "/", "192.0.2.5:5000", "192.0.2.10:20003", map[string]string{"Cookie": c.Name + "=" + c.Value}, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "other servers on 192.0.2.10") {
		t.Errorf("chooser: %d", w.Code)
	}
	if w := g.req("GET", "/auth/signin", "192.0.2.5:5000", "dun.lan:20003", nil, ""); strings.Contains(w.Body.String(), "other servers") {
		t.Error("a warning for a host name")
	}
}

// The Origin is checked against the host the browser is looking at: a
// trusted proxy's X-Forwarded-Host, else Host.
func TestOriginAgainstTheEffectiveHost(t *testing.T) {
	g := newRig(t)
	form := url.Values{"token": {g.tok}}.Encode()
	via := map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "dun.example.com", "Origin": "https://dun.example.com"}
	if w := g.req("POST", "/auth/signin", "127.0.0.1:5000", "127.0.0.1:20003", via, form); w.Code != http.StatusSeeOther {
		t.Errorf("through the proxy: %d %s", w.Code, w.Body.String())
	}
	if w := g.req("POST", "/auth/signin", "198.51.100.9:5000", "198.51.100.1:20003", via, form); w.Code != http.StatusForbidden {
		t.Errorf("an untrusted peer claiming the proxy's host: %d", w.Code)
	}
}
