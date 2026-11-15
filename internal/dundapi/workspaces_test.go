package dundapi_test

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A workspace createSession cannot use comes back as dund's sentence, the
// one the client shows as it is, through GraphQL as through REST.
func TestCreateSessionSaysWhyAWorkspaceIsRefused(t *testing.T) {
	srv, tok, _ := setup(t)
	missing := filepath.Join(t.TempDir(), "aw4")
	out := gql(t, srv, tok, `mutation { dund { createSession(body: {workspace: "`+missing+`"}) { id } } }`)
	var r struct {
		Errors []struct{ Message string } `json:"errors"`
	}
	_ = json.Unmarshal([]byte(out), &r)
	want := "no directory " + missing + ": it does not exist"
	if len(r.Errors) == 0 || r.Errors[0].Message != want {
		t.Fatalf("got %s, want the error %q", out, want)
	}
	if code, m := call(t, srv, tok, "POST", "/api/v1/sessions", map[string]any{"workspace": missing}); code != 400 || !strings.Contains(m["detail"].(string), want) {
		t.Errorf("REST: %d %v", code, m)
	}
}

func TestListWorkspacesAndDirs(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	srv, tok, svc := setup(t)
	ws := t.TempDir()
	if _, err := svc.Create("unix:1000", ws, nil, ""); err != nil {
		t.Fatal(err)
	}
	code, m := call(t, srv, tok, "GET", "/api/v1/workspaces", nil)
	items, _ := m["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["path"] != ws {
		t.Fatalf("workspaces: %d %v", code, m)
	}

	if err := os.MkdirAll(filepath.Join(ws, "sub", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, m = call(t, srv, tok, "GET", "/api/v1/dirs?path="+url.QueryEscape(ws+"/su"), nil)
	items, _ = m["items"].([]any)
	if code != 200 || len(items) != 1 || items[0] != filepath.Join(ws, "sub") || m["exists"] != false || m["dir"] != ws {
		t.Fatalf("dirs: %d %v", code, m)
	}
	if code, m := call(t, srv, tok, "GET", "/api/v1/dirs?path="+url.QueryEscape("~bob/"), nil); code != 400 {
		t.Errorf("~bob: %d %v", code, m)
	}

	// Another principal sees its own workspaces only, and browses nothing.
	other, err := svc.MintSignIn("unix:2000", "x@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code, m := call(t, srv, other, "GET", "/api/v1/workspaces", nil); code != 200 || len(m["items"].([]any)) != 0 {
		t.Errorf("other's workspaces: %d %v", code, m)
	}
	if code, _ := call(t, srv, other, "GET", "/api/v1/dirs?path=/", nil); code != 403 {
		t.Errorf("other's dirs: %d", code)
	}
}
