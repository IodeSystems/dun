package harness

import (
	"context"
	"testing"
	"time"
)

// A login whose URL was shown says how it ended: nil error when it went
// through (the mcp_login "done"), its error when not ("failed").
func TestOAuth_LoginEndIsSaid(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	b := &browser{}
	oc := w.client(OAuthSpec{}, b)
	var ends []error
	oc.ended = func(id string, err error) {
		if id != "snow" {
			t.Errorf("ended for %q", id)
		}
		ends = append(ends, err)
	}
	if err := oc.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ends) != 1 || ends[0] != nil {
		t.Fatalf("after a login: %v", ends)
	}
	// A token on hand: no login, nothing said.
	if err := oc.ensure(context.Background()); err != nil || len(ends) != 1 {
		t.Fatalf("no login: err %v, ends %v", err, ends)
	}
}

// A login nobody finishes ends with its error.
func TestOAuth_LoginFailedIsSaid(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	old := oauthLoginTimeout
	oauthLoginTimeout = 300 * time.Millisecond
	defer func() { oauthLoginTimeout = old }()
	spec := OAuthSpec{ClientID: w.clientID, RedirectPort: freePort(t)}
	nobody := newOAuthClient(Server{ID: "snow", URL: w.mcpURL(), OAuth: &spec}, func(_, _ string) {}, nil)
	var failed error
	nobody.ended = func(_ string, err error) { failed = err }
	if err := nobody.ensure(context.Background()); err == nil || failed == nil {
		t.Fatalf("a login nobody finished: err %v, ended with %v", err, failed)
	}
}
