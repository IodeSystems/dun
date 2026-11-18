package oauth

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/oauth/oauthtest"
)

// A login whose URL was shown says how it ended: nil error when it went
// through (the mcp_login "done"), its error when not ("failed").
func TestOAuth_LoginEndIsSaid(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	b := &oauthtest.Browser{}
	oc := client(t, w, Spec{}, b)
	var ends []error
	oc.Ended = func(id string, err error) {
		if id != "snow" {
			t.Errorf("ended for %q", id)
		}
		ends = append(ends, err)
	}
	if err := oc.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ends) != 1 || ends[0] != nil {
		t.Fatalf("after a login: %v", ends)
	}
	// A token on hand: no login, nothing said.
	if err := oc.Ensure(context.Background()); err != nil || len(ends) != 1 {
		t.Fatalf("no login: err %v, ends %v", err, ends)
	}
}

// A login nobody finishes ends with its error.
func TestOAuth_LoginFailedIsSaid(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := oauthtest.NewWorld(t)
	old := oauthLoginTimeout
	oauthLoginTimeout = 300 * time.Millisecond
	defer func() { oauthLoginTimeout = old }()
	spec := Spec{ClientID: w.ClientID, RedirectPort: oauthtest.FreePort(t)}
	nobody := NewClient("snow", w.MCPURL(), spec, func(_, _ string) {}, nil)
	var failed error
	nobody.Ended = func(_ string, err error) { failed = err }
	if err := nobody.Ensure(context.Background()); err == nil || failed == nil {
		t.Fatalf("a login nobody finished: err %v, ended with %v", err, failed)
	}
}
