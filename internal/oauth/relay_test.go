package oauth

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/oauth/oauthtest"
)

// Relay mode: a login whose redirect goes to a public URL completes when the
// host serving that URL calls Deliver with the code. Drives Login and Deliver
// concurrently and asserts the token lands. Before the fix, Login held
// loginMu across its wait on the callback channel while Deliver needed the
// same mutex to feed it — they deadlocked and only the login timeout got
// Login out, so no token ever landed.
func TestOAuth_RelayLoginDelivers(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	old := oauthLoginTimeout
	oauthLoginTimeout = 500 * time.Millisecond // a deadlocked login exits here, fast
	defer func() { oauthLoginTimeout = old }()

	w := oauthtest.NewWorld(t)
	var state, code string
	shown := make(chan struct{}, 1)
	oc := NewClient("snow", w.MCPURL(), Spec{ClientID: w.ClientID, Relay: "https://relay.example"},
		func(_, u string) {
			// Stand in for the person: open the login URL — the IdP's
			// /authorize registers the code and its challenge — and keep the
			// state the URL carries and the code it issued.
			parsed, _ := url.Parse(u)
			state = parsed.Query().Get("state")
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			}}
			resp, err := client.Get(u)
			if err == nil {
				loc := resp.Header.Get("Location")
				resp.Body.Close()
				if lp, lerr := url.Parse(loc); lerr == nil {
					code = lp.Query().Get("code")
				}
			}
			shown <- struct{}{}
		}, nil)

	errc := make(chan error, 1)
	go func() { errc <- oc.Login(context.Background(), "") }()
	select {
	case <-shown:
	case <-time.After(2 * time.Second):
		t.Fatal("the login URL was never shown")
	}

	// A state that is not this login's is refused without unblocking it.
	stale := make(chan bool, 1)
	go func() { stale <- oc.Deliver("stale", code, "", "") }()
	// The login's own state is accepted and the code relayed.
	ok := make(chan bool, 1)
	go func() { ok <- oc.Deliver(state, code, "", "") }()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("login after delivery: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("login did not finish within 3s of the code being delivered")
	}
	select {
	case got := <-ok:
		if !got {
			t.Fatal("Deliver refused this login's own state")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Deliver with the login's state did not return")
	}
	select {
	case got := <-stale:
		if got {
			t.Fatal("Deliver accepted a state that is not this login's")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Deliver with a stale state did not return")
	}
	oc.mu.Lock()
	tok := oc.tok
	oc.mu.Unlock()
	if tok == nil || tok.AccessToken == "" {
		t.Fatal("no token was stored")
	}
}
