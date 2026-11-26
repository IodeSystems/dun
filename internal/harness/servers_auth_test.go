package harness

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/oauth"
)

// slowSource is a login whose Status takes d, as dund's does when it is slow.
type slowSource struct {
	oauth.Source
	d       time.Duration
	calls   *atomic.Int32
	started chan struct{}
}

func (s slowSource) Status() oauth.Status {
	s.calls.Add(1)
	select {
	case s.started <- struct{}{}:
	default:
	}
	time.Sleep(s.d)
	return oauth.Status{LoggedIn: true}
}

// authHarness has n OAuth servers whose auth state takes d each.
func authHarness(t testing.TB, n int, d time.Duration) (*Harness, *atomic.Int32, chan struct{}) {
	t.Helper()
	h := &Harness{mgr: mcpmgr.NewManager(), lastErr: map[string]string{}, lastStart: map[string]time.Duration{},
		oauth: map[string]oauthEntry{}}
	t.Cleanup(h.mgr.Close)
	calls, started := new(atomic.Int32), make(chan struct{}, n)
	for i := 0; i < n; i++ {
		spec := oauth.Spec{ClientID: "c"}
		s := mcpserver.Server{ID: fmt.Sprintf("s%d", i), URL: "http://127.0.0.1:1/mcp", OAuth: &spec}
		h.specs = append(h.specs, s)
		h.oauth[s.ID] = oauthEntry{url: s.URL, spec: spec, src: slowSource{d: d, calls: calls, started: started}}
	}
	return h, calls, started
}

// A /mcp asks for the list several times (the listing, the server event, the
// command list, the slash count): the servers' auth state is asked once each,
// at the same time, and srvMu is free while it is.
func TestServersAuth_OncePerServerConcurrentUnlocked(t *testing.T) {
	const n, d = 4, 200 * time.Millisecond
	h, calls, started := authHarness(t, n, d)

	done := make(chan []ServerState)
	t0 := time.Now()
	go func() { done <- h.Servers() }()
	for i := 0; i < n; i++ { // every Status is in flight at once
		select {
		case <-started:
		case <-time.After(d / 2):
			t.Fatalf("only %d of %d auth states asked within %v: not concurrent", i, n, d/2)
		}
	}
	lock := make(chan struct{})
	go func() { h.HasOAuth("s0"); close(lock) }() // takes srvMu
	select {
	case <-lock:
	case <-time.After(d / 4):
		t.Error("srvMu is held while the auth state is asked")
	}
	var got []ServerState
	select {
	case got = <-done:
	case <-time.After(5 * d):
		t.Fatal("Servers did not return")
	}
	for i := 0; i < 3; i++ {
		h.Servers()
	}
	if el := time.Since(t0); el > 2*d {
		t.Errorf("four Servers() took %v, want about %v", el, d)
	}
	if c := calls.Load(); c != n {
		t.Errorf("auth states asked %d times, want %d (one per server)", c, n)
	}
	for _, st := range got {
		if st.Auth == "" {
			t.Errorf("%s: no auth state", st.ID)
		}
	}
	// A login drops what is kept.
	h.forgetAuth()
	h.Servers()
	if c := calls.Load(); c != 2*n {
		t.Errorf("after forgetAuth: %d asks, want %d", c, 2*n)
	}
}

// BenchmarkBareMCP times what a bare /mcp asks of the harness: four Servers()
// calls over four OAuth servers whose auth state takes 200 ms each.
func BenchmarkBareMCP(b *testing.B) {
	for b.Loop() {
		h, _, _ := authHarness(b, 4, 200*time.Millisecond)
		for i := 0; i < 4; i++ {
			h.Servers()
		}
	}
}
