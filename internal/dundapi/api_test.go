package dundapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	h, err := dundapi.Handler(svc, "unix:1000")
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
	for _, op := range []string{"listSessions", "createSession", "sendMessage", "sessionEvents"} {
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
