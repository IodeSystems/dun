package dundapi_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/internal/dundapi"
	"github.com/iodesystems/dun/internal/webterm"
)

// signedIn is a browser that signed in at /auth/signin with tok: its jar
// holds the session cookie. It follows no redirects.
func signedIn(t *testing.T, srv *httptest.Server, tok string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.PostForm(dl(srv)+"/auth/signin", url.Values{"token": {tok}, "return": {"/"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("sign-in: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	u, _ := url.Parse(dl(srv))
	var ck *http.Cookie
	for _, k := range jar.Cookies(u) {
		if k.Name == "dun_session" {
			ck = k
		}
	}
	if ck == nil || !strings.HasPrefix(ck.Value, "dun_s_") {
		t.Fatalf("no session cookie: %v", jar.Cookies(u))
	}
	return c
}

func get(t *testing.T, c *http.Client, u string) (int, string, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

type front struct {
	srv   *httptest.Server
	dir   string
	tok   string
	tuiOn *atomic.Bool
}

func setupFront(t *testing.T) front {
	t.Helper()
	dir := t.TempDir()
	svc, err := dund.New(dir, &fakeSpawner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	on := &atomic.Bool{}
	term := webterm.New(webterm.Config{Bin: "/bin/sh", Args: []string{"-c", "echo in-the-tui; sleep 30"}, Env: os.Environ(), Back: "/?choose=1"})
	t.Cleanup(term.Close)
	h, err := dundapi.Handler(svc, dundapi.Options{Owner: "unix:1000", Term: term, TUIOn: on.Load})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok, _ := dund.NewToken(dir)
	return front{srv: srv, dir: dir, tok: tok, tuiOn: on}
}

// / needs a sign-in; then it offers the choices, remembers one in a cookie
// and goes straight there next time; ?choose=1 offers them again; a
// choice cookie naming anything else is not followed.
func TestChooser(t *testing.T) {
	f := setupFront(t)
	if code, loc, _ := get(t, noRedirect(), dl(f.srv)+"/"); code != http.StatusFound || loc != "/auth/signin?return=%2F" {
		t.Fatalf("signed out: %d %s", code, loc)
	}
	c := signedIn(t, f.srv, f.tok)
	code, _, body := get(t, c, dl(f.srv)+"/")
	if code != 200 || !strings.Contains(body, "/choose/web") || !strings.Contains(body, "experimental") || strings.Contains(body, "/choose/tui") {
		t.Fatalf("chooser with the tui off: %d %s", code, body)
	}
	if code, _, _ := get(t, c, dl(f.srv)+"/choose/tui"); code != 404 {
		t.Errorf("choose tui while off: %d", code)
	}
	f.tuiOn.Store(true)
	if _, _, body := get(t, c, dl(f.srv)+"/"); !strings.Contains(body, "/choose/tui") {
		t.Errorf("chooser with the tui on: %s", body)
	}
	if code, loc, _ := get(t, c, dl(f.srv)+"/choose/web"); code != http.StatusFound || loc != "/web/" {
		t.Fatalf("choose web: %d %s", code, loc)
	}
	if code, loc, _ := get(t, c, dl(f.srv)+"/"); code != http.StatusFound || loc != "/web/" {
		t.Errorf("remembered: %d %s", code, loc)
	}
	if code, _, body := get(t, c, dl(f.srv)+"/?choose=1"); code != 200 || !strings.Contains(body, "/choose/web") {
		t.Errorf("choose again: %d", code)
	}
	if code, loc, _ := get(t, c, dl(f.srv)+"/choose/tui"); code != http.StatusFound || loc != "/tui/" {
		t.Errorf("choose tui: %d %s", code, loc)
	}
	if code, _, _ := get(t, c, dl(f.srv)+"/choose/https:%2F%2Fevil.example"); code != 404 {
		t.Errorf("choose elsewhere: %d", code)
	}
	// Not an open redirect: a forged choice is ignored.
	u, _ := url.Parse(dl(f.srv))
	c.Jar.SetCookies(u, []*http.Cookie{{Name: "dun_choice", Value: "https://evil.example/", Path: "/"}})
	if code, loc, _ := get(t, c, dl(f.srv)+"/"); code != 200 || loc != "" {
		t.Errorf("forged choice: %d %s", code, loc)
	}
	// Nor is a sign-in's return.
	for _, bad := range []string{"https://evil.example/", "//evil.example/", "dun://auth"} {
		resp, _ := noRedirect().PostForm(dl(f.srv)+"/auth/signin", url.Values{"token": {f.tok}, "return": {bad}})
		if loc := resp.Header.Get("Location"); loc != "/" {
			t.Errorf("sign-in return %q went to %q", bad, loc)
		}
	}
}

// /tui is 404 while the tui is off; on, its page needs a sign-in, and its
// WebSocket needs the session cookie and an Origin of this site.
func TestTUIBehindSignIn(t *testing.T) {
	f := setupFront(t)
	c := signedIn(t, f.srv, f.tok)
	if code, _, _ := get(t, c, dl(f.srv)+"/tui/"); code != 404 {
		t.Fatalf("tui off: %d", code)
	}
	f.tuiOn.Store(true)
	if code, loc, _ := get(t, c, dl(f.srv)+"/tui"); code != http.StatusMovedPermanently || loc != "/tui/" {
		t.Errorf("/tui: %d %s", code, loc)
	}
	if code, _, body := get(t, c, dl(f.srv)+"/tui/"); code != 200 || !strings.Contains(body, "xterm") || !strings.Contains(body, `href="/?choose=1"`) {
		t.Errorf("page: %d", code)
	}
	if code, _, _ := get(t, c, dl(f.srv)+"/tui/static/xterm.js"); code != 200 {
		t.Errorf("assets: %d", code)
	}
	if code, loc, _ := get(t, noRedirect(), dl(f.srv)+"/tui/"); code != http.StatusFound || !strings.HasPrefix(loc, "/auth/signin") {
		t.Errorf("signed out: %d %s", code, loc)
	}

	ws := "ws" + strings.TrimPrefix(dl(f.srv), "http") + "/tui/ws"
	u, _ := url.Parse(dl(f.srv))
	cookie := ""
	for _, k := range c.Jar.Cookies(u) {
		cookie += k.Name + "=" + k.Value + "; "
	}
	dial := func(origin, cookie string) (*websocket.Conn, int) {
		h := http.Header{"Origin": {origin}}
		if cookie != "" {
			h.Set("Cookie", cookie)
		}
		conn, resp, err := websocket.DefaultDialer.Dial(ws, h)
		if err != nil {
			if resp == nil {
				t.Fatalf("dial: %v", err)
			}
			return nil, resp.StatusCode
		}
		return conn, 101
	}
	if _, code := dial(dl(f.srv), ""); code != http.StatusUnauthorized {
		t.Errorf("no cookie: %d", code)
	}
	if _, code := dial("https://evil.example", cookie); code != http.StatusForbidden {
		t.Errorf("foreign origin: %d", code)
	}
	conn, code := dial(dl(f.srv), cookie)
	if code != 101 {
		t.Fatalf("signed in, same origin: %d", code)
	}
	defer conn.Close()
	got := ""
	for !strings.Contains(got, "in-the-tui") {
		_, b, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v (%q)", err, got)
		}
		got += string(b)
	}
	// Off again: the page goes.
	f.tuiOn.Store(false)
	if code, _, _ := get(t, c, dl(f.srv)+"/tui/"); code != 404 {
		t.Errorf("tui off again: %d", code)
	}
}

// The session cookie works on the API, refuses a change from another site,
// and ends when the token it was made from is replaced; a bad token or a
// post from another site does not sign in.
func TestSessionCookie(t *testing.T) {
	f := setupFront(t)
	c := signedIn(t, f.srv, f.tok)
	if code, _, _ := get(t, c, dl(f.srv)+"/api/v1/sessions"); code != 200 {
		t.Errorf("api on the cookie: %d", code)
	}
	req, _ := http.NewRequest("POST", dl(f.srv)+"/api/v1/sessions", strings.NewReader(`{"workspace":"/tmp"}`))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Content-Type", "application/json")
	if resp, err := c.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site change: %v %v", resp.StatusCode, err)
	}
	resp, _ := noRedirect().PostForm(dl(f.srv)+"/auth/signin", url.Values{"token": {"dun_wrong"}, "return": {"/tui/"}})
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "error=") || len(resp.Cookies()) != 0 {
		t.Errorf("bad token: %s %v", loc, resp.Cookies())
	}
	req, _ = http.NewRequest("POST", dl(f.srv)+"/auth/signin", strings.NewReader(url.Values{"token": {f.tok}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := noRedirect().Do(req); resp.StatusCode != http.StatusForbidden || len(resp.Cookies()) != 0 {
		t.Errorf("cross-site sign-in: %d", resp.StatusCode)
	}
	if _, err := dund.NewToken(f.dir); err != nil { // the token is replaced
		t.Fatal(err)
	}
	if code, loc, _ := get(t, c, dl(f.srv)+"/"); code != http.StatusFound || !strings.HasPrefix(loc, "/auth/signin") {
		t.Errorf("after the token is replaced: %d %s", code, loc)
	}
	if code, _, _ := get(t, c, dl(f.srv)+"/api/v1/sessions"); code != 401 {
		t.Errorf("api after the token is replaced: %d", code)
	}
	// Open without a sign-in: hello, healthz, the sign-in page.
	for _, p := range []string{"/api/v1/hello", "/healthz", "/auth/signin"} {
		if code, _, _ := get(t, noRedirect(), dl(f.srv)+p); code != 200 {
			t.Errorf("%s: %d", p, code)
		}
	}
}

// A ".." segment, decoded or escaped, is refused before the open-path check:
// "/auth/../tui/ws" must not pass as /auth.
func TestDotDotPathsDoNotSkipSignIn(t *testing.T) {
	f := setupFront(t)
	f.tuiOn.Store(true)
	for _, p := range []string{"/auth/../tui/ws", "/auth/%2e%2e/tui/ws", "/auth/%2E%2E/tui/", "/auth/.%2e/web/", "/releases/../api/v1/sessions"} {
		c, err := net.Dial("tcp", strings.TrimPrefix(dl(f.srv), "http://"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", p)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		c.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", p, resp.StatusCode)
		}
	}
}

// A return a browser would read as another host is refused: control bytes
// (a tab is dropped, "/\t/evil" is "//evil"), schemes, hosts.
func TestReturnIsNotAnOpenRedirect(t *testing.T) {
	f := setupFront(t)
	for _, bad := range []string{"/\t/evil.example", "/\n/evil.example", "/\x7f/evil.example", "//evil.example", "/\\evil.example"} {
		resp, _ := noRedirect().PostForm(dl(f.srv)+"/auth/signin", url.Values{"token": {f.tok}, "return": {bad}})
		if loc := resp.Header.Get("Location"); loc != "/" {
			t.Errorf("sign-in return %q went to %q", bad, loc)
		}
		if code, _, body := get(t, noRedirect(), dl(f.srv)+"/auth/signin?return="+url.QueryEscape(bad)); code != 200 || strings.Contains(body, "evil") {
			t.Errorf("sign-in page return %q: %d", bad, code)
		}
	}
	idp := newIdP(t)
	srv := ssoServer(t, idp)
	for _, bad := range []string{"/%09/evil.com", "/%0a/evil.com", "/%7f/evil.com"} {
		resp, _ := noRedirect().Get(dl(srv) + "/auth/login?return=" + bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("login return %s: %d", bad, resp.StatusCode)
		}
	}
}

// Signing out ends the session: its token is revoked, not only the cookie
// cleared.
func TestSignOutRevokesTheSession(t *testing.T) {
	f := setupFront(t)
	c := signedIn(t, f.srv, f.tok)
	u, _ := url.Parse(dl(f.srv))
	var session string
	for _, k := range c.Jar.Cookies(u) {
		if k.Name == "dun_session" {
			session = k.Value
		}
	}
	resp, err := c.PostForm(dl(f.srv)+"/auth/signout", nil)
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-out: %v %v", resp, err)
	}
	if code, loc, _ := get(t, c, dl(f.srv)+"/"); code != http.StatusFound || !strings.HasPrefix(loc, "/auth/signin") {
		t.Errorf("after sign-out: %d %s", code, loc)
	}
	// The old cookie value, replayed, is dead too.
	req, _ := http.NewRequest("GET", dl(f.srv)+"/api/v1/sessions", nil)
	req.AddCookie(&http.Cookie{Name: "dun_session", Value: session})
	if resp, _ := noRedirect().Do(req); resp.StatusCode != 401 {
		t.Errorf("replayed cookie after sign-out: %d", resp.StatusCode)
	}
}
