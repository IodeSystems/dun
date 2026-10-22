package dundapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/internal/dundapi"
)

func setup(t *testing.T) (*httptest.Server, string, *dund.Service) {
	t.Helper()
	dir := t.TempDir()
	svc, err := dund.New(dir, &fakeSpawner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	h, err := dundapi.Handler(svc, dundapi.Options{Owner: "unix:1000"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok, err := dund.NewToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	return srv, tok, svc
}

func call(t *testing.T, srv *httptest.Server, tok, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestHealthzNeedsNoToken(t *testing.T) {
	srv, _, _ := setup(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp, err)
	}
}

func TestEveryOperationNeedsTheToken(t *testing.T) {
	srv, tok, _ := setup(t)
	for _, k := range []string{"", "dun_wrong", tok + "x"} {
		if code, _ := call(t, srv, k, "GET", "/api/v1/sessions", nil); code != 401 {
			t.Errorf("token %q: %d", k, code)
		}
	}
	if code, _ := call(t, srv, tok, "GET", "/api/v1/sessions", nil); code != 200 {
		t.Errorf("good token: %d", code)
	}
}

// A browser holding the local token drives a session end to end: create,
// message, and the reply comes back through the events long-poll.
func TestSessionOverHTTP(t *testing.T) {
	srv, tok, _ := setup(t)
	code, m := call(t, srv, tok, "POST", "/api/v1/sessions", map[string]any{"workspace": t.TempDir()})
	if code != 200 {
		t.Fatalf("create: %d %v", code, m)
	}
	id := m["id"].(string)
	if code, m := call(t, srv, tok, "POST", "/api/v1/sessions/"+id+"/messages", map[string]any{"content": "hi"}); code != 200 {
		t.Fatalf("message: %d %v", code, m)
	}
	var after float64
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		code, m := call(t, srv, tok, "GET", "/api/v1/sessions/"+id+"/events?after="+jsonNum(after)+"&wait=2", nil)
		if code != 200 {
			t.Fatalf("events: %d %v", code, m)
		}
		evs, _ := m["events"].([]any)
		for _, e := range evs {
			ev := e.(map[string]any)
			if ev["type"] == "message" && ev["content"] == "re: hi" {
				return
			}
		}
		after = m["last"].(float64)
	}
	t.Fatal("no reply in the events")
}

func jsonNum(f float64) string { b, _ := json.Marshal(f); return string(b) }

// Waiting with nothing new returns empty after the wait, not at once and not
// never.
func TestEventsWait(t *testing.T) {
	srv, tok, _ := setup(t)
	_, m := call(t, srv, tok, "POST", "/api/v1/sessions", map[string]any{"workspace": t.TempDir()})
	id := m["id"].(string)
	time.Sleep(100 * time.Millisecond)
	_, m = call(t, srv, tok, "GET", "/api/v1/sessions/"+id+"/events?after=0", nil)
	last := jsonNum(m["last"].(float64))
	start := time.Now()
	_, m = call(t, srv, tok, "GET", "/api/v1/sessions/"+id+"/events?after="+last+"&wait=1", nil)
	if d := time.Since(start); d < 900*time.Millisecond || d > 3*time.Second {
		t.Errorf("waited %s", d)
	}
	if evs, _ := m["events"].([]any); len(evs) != 0 {
		t.Errorf("events: %v", evs)
	}
}

func TestUnknownSessionIs404(t *testing.T) {
	srv, tok, _ := setup(t)
	if code, _ := call(t, srv, tok, "POST", "/api/v1/sessions/nope/messages", map[string]any{"content": "x"}); code != 404 {
		t.Errorf("code %d", code)
	}
}

// The same operations are GraphQL, through gat.
func TestGraphQLSchemaCarriesTheOperations(t *testing.T) {
	srv, tok, _ := setup(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/schema/graphql", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	sdl := string(b)
	for _, op := range []string{"listSessions", "createSession", "sendMessage", "pollSessionEvents", "dund_sessionEvents"} {
		if !strings.Contains(sdl, op) {
			t.Errorf("%s missing from the SDL", op)
		}
	}
	t.Logf("SDL %d bytes", len(sdl))
}

func gql(t *testing.T, srv *httptest.Server, tok, query string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"query": query})
	req, _ := http.NewRequest("POST", srv.URL+"/api/graphql", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out)
}

// GraphQL goes through the same operations, token check included.
func TestGraphQLRunsTheOperations(t *testing.T) {
	srv, tok, svc := setup(t)
	id, err := svc.Create("unix:1000", t.TempDir(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	out := gql(t, srv, tok, `{ dund { listSessions { items { id running } } } }`)
	if !strings.Contains(out, id) {
		t.Fatalf("with the token: %s", out)
	}
	if out := gql(t, srv, "", `{ dund { listSessions { items { id } } } }`); strings.Contains(out, id) || !strings.Contains(out, `"status":401`) {
		t.Fatalf("without the token: %s", out)
	}
}

// The page is open (it holds nothing) and is HTML.
func TestPageIsServedWithoutAToken(t *testing.T) {
	srv, _, _ := setup(t)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "dun dund token") {
		t.Fatalf("page: %d %.80s", resp.StatusCode, b)
	}
}

// A web root is served at /, with the app shell for its client routes; the
// API still needs the token.
func TestWebRootServesTheClient(t *testing.T) {
	dir := t.TempDir()
	svc, _ := dund.New(dir, &fakeSpawner{})
	defer svc.Close()
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>app shell</html>"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "_expo"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "_expo", "app.js"), []byte("js"), 0o644)
	h, err := dundapi.Handler(svc, dundapi.Options{Owner: "unix:1000", WebRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	for path, want := range map[string]string{"/": "app shell", "/session/abc": "app shell", "/_expo/app.js": "js"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Errorf("%s: %d %q", path, resp.StatusCode, b)
		}
	}
	if resp, _ := http.Get(srv.URL + "/api/v1/sessions"); resp.StatusCode != 401 {
		t.Errorf("api without token: %d", resp.StatusCode)
	}
	if _, err := dundapi.Handler(svc, dundapi.Options{Owner: "unix:1000", WebRoot: t.TempDir()}); err == nil {
		t.Error("a web root without index.html was accepted")
	}
}

// A release published by tools/release-android is announced by hello and
// downloadable; /dun.apk is the latest; directories are not listed.
func TestAndroidReleases(t *testing.T) {
	svc, _ := dund.New(t.TempDir(), &fakeSpawner{})
	defer svc.Close()
	rel := t.TempDir()
	_ = os.MkdirAll(filepath.Join(rel, "android"), 0o755)
	_ = os.WriteFile(filepath.Join(rel, "android", "dun-7.apk"), []byte("PK apk"), 0o644)
	_ = os.WriteFile(filepath.Join(rel, "android", "latest.json"),
		[]byte(`{"version_code":7,"version_name":"0.1.0+abc","sha256":"x","size":6,"file":"dun-7.apk"}`), 0o644)
	h, err := dundapi.Handler(svc, dundapi.Options{Owner: "unix:1000", Releases: rel})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/api/v1/hello")
	var hello struct {
		Android struct {
			VersionCode int    `json:"version_code"`
			APKPath     string `json:"apk_path"`
		} `json:"android"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&hello)
	if hello.Android.VersionCode != 7 || hello.Android.APKPath != "/releases/android/dun-7.apk" {
		t.Fatalf("hello: %+v", hello.Android)
	}
	resp, err = http.Get(srv.URL + "/dun.apk") // follows the redirect
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "PK apk" || resp.Header.Get("Content-Type") != "application/vnd.android.package-archive" {
		t.Fatalf("apk: %d %q %s", resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}
	if resp, _ := http.Get(srv.URL + "/releases/android/"); resp.StatusCode != 404 {
		t.Errorf("listing: %d", resp.StatusCode)
	}
}
