package raglit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDaemon answers the projects API and records the last request it got,
// so a test can check what the client sent: the bearer token, the query, the
// body.
type fakeDaemon struct {
	t *testing.T

	lastMethod string
	lastPath   string
	lastQuery  map[string]string
	lastAuth   string
	lastBody   map[string]any

	status int
	resp   string
}

func (f *fakeDaemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastMethod = r.Method
		f.lastPath = r.URL.Path
		f.lastQuery = map[string]string{}
		for k, v := range r.URL.Query() {
			f.lastQuery[k] = v[0]
		}
		f.lastAuth = r.Header.Get("Authorization")
		f.lastBody = nil
		if b, err := io.ReadAll(r.Body); err == nil && len(b) > 0 {
			_ = json.Unmarshal(b, &f.lastBody)
		}
		if f.status == 0 {
			f.status = http.StatusOK
		}
		if f.resp == "" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, "{}")
			return
		}
		w.WriteHeader(f.status)
		io.WriteString(w, f.resp)
	})
}

func newClient(t *testing.T) (*Client, *fakeDaemon) {
	t.Helper()
	f := &fakeDaemon{t: t}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	c.Token = func() string { return "tok-1" }
	return c, f
}

func TestRegisterSendsProjectIDAndBearer(t *testing.T) {
	c, f := newClient(t)
	f.resp = `{"project_id":"abc123","namespace":"abc123","base":"abc123__base","private":"abc123__u0001-priv","public":"abc123__u0001-pub","new_base":true,"new_private":true,"new_public":true}`
	reg, err := c.Register(context.Background(), "abc123", "origin x")
	if err != nil {
		t.Fatal(err)
	}
	if f.lastMethod != "POST" || f.lastPath != "/api/projects/register" {
		t.Errorf("want POST /api/projects/register, got %s %s", f.lastMethod, f.lastPath)
	}
	if f.lastAuth != "Bearer tok-1" {
		t.Errorf("bearer header = %q, want Bearer tok-1", f.lastAuth)
	}
	if f.lastBody["project_id"] != "abc123" || f.lastBody["label"] != "origin x" {
		t.Errorf("body = %v", f.lastBody)
	}
	if reg.Namespace != "abc123" || reg.Base != "abc123__base" ||
		reg.Private != "abc123__u0001-priv" || reg.Public != "abc123__u0001-pub" {
		t.Errorf("registration = %+v", reg)
	}
	if !reg.NewBase || !reg.NewPrivate || !reg.NewPublic {
		t.Errorf("new flags = %+v", reg)
	}
}

func TestUploadPutsLayerInQueryAndNameInBody(t *testing.T) {
	c, f := newClient(t)
	f.resp = `{"index":"abc__u0001-priv","path":"/abs/xyz.md","job_id":42}`
	res, err := c.Upload(context.Background(), "abc__u0001-priv", UploadDoc{Name: "xyz.md", Text: "a fact", Title: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	if f.lastMethod != "POST" || f.lastPath != "/api/upload" {
		t.Errorf("want POST /api/upload, got %s %s", f.lastMethod, f.lastPath)
	}
	if f.lastQuery["index"] != "abc__u0001-priv" {
		t.Errorf("query index = %q", f.lastQuery["index"])
	}
	if f.lastBody["name"] != "xyz.md" || f.lastBody["text"] != "a fact" || f.lastBody["title"] != "fact" {
		t.Errorf("body = %v", f.lastBody)
	}
	if res.Index != "abc__u0001-priv" || res.Path != "/abs/xyz.md" || res.JobID != 42 {
		t.Errorf("result = %+v", res)
	}
}

func TestForgetsByIndexAndPath(t *testing.T) {
	c, f := newClient(t)
	f.resp = `{"ok":true}`
	if err := c.Forget(context.Background(), "abc__u0001-priv", "/abs/xyz.md"); err != nil {
		t.Fatal(err)
	}
	if f.lastMethod != "POST" || f.lastPath != "/api/forget" {
		t.Errorf("want POST /api/forget, got %s %s", f.lastMethod, f.lastPath)
	}
	if f.lastQuery["index"] != "abc__u0001-priv" || f.lastQuery["path"] != "/abs/xyz.md" {
		t.Errorf("query = %v", f.lastQuery)
	}
	if f.lastBody != nil {
		t.Errorf("forget takes no body, got %v", f.lastBody)
	}
}

func TestSetVisibilitySendsLayerAndVisibility(t *testing.T) {
	c, f := newClient(t)
	f.resp = `{"owner":"iss#sub","visibility":"public"}`
	acl, err := c.SetVisibility(context.Background(), "abc__u0001-priv", Public)
	if err != nil {
		t.Fatal(err)
	}
	if f.lastMethod != "POST" || f.lastPath != "/api/visibility" {
		t.Errorf("want POST /api/visibility, got %s %s", f.lastMethod, f.lastPath)
	}
	if f.lastBody["index"] != "abc__u0001-priv" || f.lastBody["visibility"] != "public" {
		t.Errorf("body = %v", f.lastBody)
	}
	if acl.Owner != "iss#sub" || acl.Visibility != Public {
		t.Errorf("acl = %+v", acl)
	}
}

func TestTransferOwnerSendsNewOwner(t *testing.T) {
	c, f := newClient(t)
	f.resp = `{"owner":"iss#other","visibility":"private"}`
	acl, err := c.TransferOwner(context.Background(), "abc__u0001-priv", "iss#other")
	if err != nil {
		t.Fatal(err)
	}
	if f.lastPath != "/api/owner" {
		t.Errorf("path = %s", f.lastPath)
	}
	if f.lastBody["index"] != "abc__u0001-priv" || f.lastBody["owner"] != "iss#other" {
		t.Errorf("body = %v", f.lastBody)
	}
	if acl.Owner != "iss#other" {
		t.Errorf("acl = %+v", acl)
	}
}

func TestSavingsFindsProjectByIDOrNamespace(t *testing.T) {
	c, f := newClient(t)
	f.resp = `{"project_id":"abc","base":"abc__base","layers":[{"name":"abc__u1-priv","documents":3,"duplicates":1}],"duplicate_documents":1,"recommend":true,"reason":"copies across layers","fanout_now":3,"fanout_with_base":2}`
	s, err := c.Savings(context.Background(), "abc", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.lastMethod != "GET" || f.lastPath != "/api/projects/savings" {
		t.Errorf("want GET /api/projects/savings, got %s %s", f.lastMethod, f.lastPath)
	}
	if f.lastQuery["id"] != "abc" || f.lastQuery["ns"] != "" {
		t.Errorf("query = %v", f.lastQuery)
	}
	if s.ProjectID != "abc" || len(s.Layers) != 1 || s.Layers[0].Duplicates != 1 || !s.Recommend || s.FanoutWithBase != 2 {
		t.Errorf("savings = %+v", s)
	}

	// By namespace, with the id left out.
	f.lastQuery = nil
	if _, err := c.Savings(context.Background(), "", "abc"); err != nil {
		t.Fatal(err)
	}
	if f.lastQuery["ns"] != "abc" || f.lastQuery["id"] != "" {
		t.Errorf("query = %v", f.lastQuery)
	}
}

func TestErrorCarriesTheDaemonMessage(t *testing.T) {
	c, f := newClient(t)
	f.status = http.StatusForbidden
	f.resp = `{"detail":"only the index owner may change it"}`
	_, err := c.SetVisibility(context.Background(), "abc__u0001-priv", Public)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "only the index owner may change it") {
		t.Errorf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "/api/visibility") {
		t.Errorf("error should name the endpoint: %v", err)
	}
}

func TestUnreachableDaemonIsErrUnavailable(t *testing.T) {
	// A closed server: nothing to talk to.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u := srv.URL
	srv.Close()
	c := New(u)
	c.Token = func() string { return "tok-1" }
	_, err := c.Register(context.Background(), "abc", "")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("error = %v", err)
	}
}

func TestSearchSendsQueryAndParsesHits(t *testing.T) {
	c, f := newClient(t)
	f.resp = `{"hits":[{"doc_id":"abc__u0001-priv/xyz.md","snippet":"a fact about x","score":0.91,"index":"abc__u0001-priv","title":"fact","rel":"xyz.md","name":"xyz.md","kind":"memory","summary":"short summary","similarity":0.91},{"doc_id":"abc__u0001-priv/other.md","snippet":"another","score":0.55,"index":"abc__u0001-priv","title":"other","rel":"other.md","name":"other.md","kind":"memory","summary":"","similarity":0.55}]}`
	hits, err := c.Search(context.Background(), "abc__u0001-priv", "a fact", 5)
	if err != nil {
		t.Fatal(err)
	}
	if f.lastMethod != "GET" || f.lastPath != "/api/search" {
		t.Errorf("want GET /api/search, got %s %s", f.lastMethod, f.lastPath)
	}
	if f.lastQuery["index"] != "abc__u0001-priv" || f.lastQuery["q"] != "a fact" || f.lastQuery["n"] != "5" {
		t.Errorf("query = %v", f.lastQuery)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if hits[0].DocID != "abc__u0001-priv/xyz.md" || hits[0].Snippet != "a fact about x" || hits[0].Score != 0.91 {
		t.Errorf("hit[0] = %+v", hits[0])
	}
	if hits[0].Title != "fact" || hits[0].Kind != "memory" || hits[0].Summary != "short summary" {
		t.Errorf("hit[0] fields = %+v", hits[0])
	}
	if hits[1].DocID != "abc__u0001-priv/other.md" || hits[1].Score != 0.55 {
		t.Errorf("hit[1] = %+v", hits[1])
	}
}

func TestSearchEmptyResponse(t *testing.T) {
	c, f := newClient(t)
	f.resp = `{"hits":[]}`
	hits, err := c.Search(context.Background(), "abc__u0001-priv", "nothing", 3)
	if err != nil {
		t.Fatal(err)
	}
	if hits != nil && len(hits) != 0 {
		t.Errorf("want empty or nil, got %v", hits)
	}
}

func TestTokenIsReadPerRequest(t *testing.T) {
	c, f := newClient(t)
	tok := "tok-1"
	c.Token = func() string { return tok }
	f.resp = `{"ok":true}`
	if err := c.Forget(context.Background(), "i", "p"); err != nil {
		t.Fatal(err)
	}
	if f.lastAuth != "Bearer tok-1" {
		t.Fatalf("first auth = %q", f.lastAuth)
	}
	// A rotated token: the client picks it up without a rebuild.
	tok = "tok-2"
	if err := c.Forget(context.Background(), "i", "p"); err != nil {
		t.Fatal(err)
	}
	if f.lastAuth != "Bearer tok-2" {
		t.Errorf("rotated auth = %q", f.lastAuth)
	}
}
