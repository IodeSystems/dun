package harness

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// netRunner is nudgeRunner behind a network that can be down: every
// request fails with a refused dial, or with err when set.
type netRunner struct {
	*nudgeRunner
	down atomic.Bool
	err  error
}

func (r *netRunner) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.down.Load() {
		return nil, fmt.Errorf("llm: do: %w", &net.OpError{Op: "dial", Net: "tcp",
			Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}})
	}
	return r.nudgeRunner.ChatStream(ctx, msgs, tools, opts)
}

// fakeProvider is an address that refuses until up() and then answers
// every request (a 401, which is the network back all the same).
type fakeProvider struct {
	addr string
	srv  *http.Server
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{addr: ln.Addr().String()}
	ln.Close() // refuses now
	t.Cleanup(func() {
		if p.srv != nil {
			p.srv.Close()
		}
	})
	return p
}

func (p *fakeProvider) url() string { return "http://" + p.addr }

func (p *fakeProvider) up(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		t.Fatal(err)
	}
	p.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})}
	go p.srv.Serve(ln)
}

type netNotes struct {
	mu sync.Mutex
	ns []RetryNote
}

func (n *netNotes) add(r RetryNote) { n.mu.Lock(); n.ns = append(n.ns, r); n.mu.Unlock() }
func (n *netNotes) has(s string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, r := range n.ns {
		if strings.Contains(r.Reason, s) {
			return true
		}
	}
	return false
}

// startNet is startAuto with a provider to probe, no turn-scope retry and
// a fast probe schedule.
func startNet(t *testing.T, r *netRunner, p *fakeProvider, dir string, notes *netNotes) *Harness {
	t.Helper()
	t.Setenv("DUN_TURN_RETRY_BUDGET", "0")
	f, m := netWaitFirst, netWaitMax
	netWaitFirst, netWaitMax = 10*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { netWaitFirst, netWaitMax = f, m })
	h := startLedger(t, r.nudgeRunner, dir, filepath.Join(dir, "s.jsonl"), func(c *Config) {
		c.Client = r
		c.SettleDelay = -1
		c.Endpoint.URL = p.url()
		if notes != nil {
			c.OnRetry = notes.add
		}
	})
	t.Cleanup(h.Close)
	return h
}

func waitWake(t *testing.T, h *Harness, d time.Duration) bool {
	t.Helper()
	select {
	case <-h.Wake():
		return true
	case <-time.After(d):
		return false
	}
}

// A refused dial with the person's ask open: the session waits, probes
// while the provider refuses, and when it answers starts ONE continue
// turn naming the ask.
func TestNetWait_TransportGiveupWaitsThenContinuesOnce(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	r := &netRunner{nudgeRunner: &nudgeRunner{}}
	p := newFakeProvider(t)
	h := startNet(t, r, p, t.TempDir(), nil)
	r.down.Store(true)
	if _, err := h.Ask(context.Background(), "fix the login bug\nit 500s on submit"); !NetworkDown(err) {
		t.Fatalf("the turn's error %v is not a network fault", err)
	}
	w := h.NetWait()
	if !w.Waiting() || w.Next.IsZero() {
		t.Fatalf("not waiting for the network: %+v", w)
	}
	if waitWake(t, h, 150*time.Millisecond) {
		t.Fatal("woke while the provider still refuses")
	}
	if !h.NetWait().Waiting() {
		t.Fatal("the wait ended while the provider refuses")
	}
	r.down.Store(false)
	p.up(t)
	if !waitWake(t, h, 2*time.Second) {
		t.Fatal("no wake after the provider answered")
	}
	if h.NetWait().Waiting() {
		t.Error("still waiting after the probe succeeded")
	}
	n := r.calls()
	if _, err := h.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.calls() == n {
		t.Fatal("the continue sent no request")
	}
	if !r.sentHas(n, "[automatic] the network was down from ") || !r.sentHas(n, "; continue: fix the login bug") {
		t.Error("the continue's text is not in its request")
	}
	if waitWake(t, h, 150*time.Millisecond) {
		t.Error("a second wake: one resume per give-up")
	}
	if _, err := os.Stat(netWaitPath(h.cfg.SessionFile)); err != nil {
		t.Errorf("the sidecar keeps the resume count: %v", err)
	}
}

// A provider that answered with an error, or a budget, is not the network:
// no wait. A turn with no ask open does not wait either.
func TestNetWait_NoWaitForProviderErrorsOrNoAsk(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	for _, e := range []error{
		errors.New("llm: status 401: invalid key"),
		errors.New("llm: retry budget 5m0s exhausted after 5m2s (last status 503)"),
		errors.New("llm: retry budget 5m0s exhausted after 5m1s of 429 backpressure"),
		&llm.BudgetSpentError{},
	} {
		r := &netRunner{nudgeRunner: &nudgeRunner{}, err: e}
		h := startNet(t, r, newFakeProvider(t), t.TempDir(), nil)
		if _, err := h.Ask(context.Background(), "deploy it"); err == nil {
			t.Fatalf("%v: the turn did not fail", e)
		}
		if h.NetWait().Waiting() {
			t.Errorf("%v: waiting for the network", e)
		}
	}
	r := &netRunner{nudgeRunner: &nudgeRunner{}}
	h := startNet(t, r, newFakeProvider(t), t.TempDir(), nil)
	r.down.Store(true)
	if err := wakeTurn(h, "job 3 finished"); !NetworkDown(err) {
		t.Fatalf("the wake's turn: %v", err)
	}
	if h.NetWait().Waiting() {
		t.Error("waiting with no ask open")
	}
}

// The person's message ends the wait: it runs as usual, and no continue
// follows when the provider answers.
func TestNetWait_PersonMessageCancels(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	r := &netRunner{nudgeRunner: &nudgeRunner{}}
	p := newFakeProvider(t)
	var changes atomic.Int32
	h := startNet(t, r, p, t.TempDir(), nil)
	h.cfg.OnNetWait = func() { changes.Add(1) }
	r.down.Store(true)
	_, _ = h.Ask(context.Background(), "refactor the parser")
	if !h.NetWait().Waiting() {
		t.Fatal("not waiting")
	}
	r.down.Store(false)
	if _, err := h.Ask(context.Background(), "never mind, what time is it"); err != nil {
		t.Fatal(err)
	}
	if h.NetWait().Waiting() {
		t.Fatal("the person's message did not end the wait")
	}
	if changes.Load() == 0 {
		t.Error("OnNetWait never fired")
	}
	p.up(t)
	if waitWake(t, h, 200*time.Millisecond) {
		t.Error("a continue after the person's message cancelled the wait")
	}
}

// At netResumeCap resumes in a day the ask is not waited on again, and a
// give-up note says so.
func TestNetWait_Cap(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	r := &netRunner{nudgeRunner: &nudgeRunner{}}
	notes := &netNotes{}
	h := startNet(t, r, newFakeProvider(t), t.TempDir(), notes)
	r.down.Store(true)
	_, _ = h.Ask(context.Background(), "sync the mirror")
	ids, _ := h.openAsks()
	if len(ids) != 1 || !h.NetWait().Waiting() {
		t.Fatalf("asks %v, waiting %v", ids, h.NetWait().Waiting())
	}
	h.netWaitCancel()
	h.netWait.mu.Lock()
	h.netWait.resumes = map[string][]time.Time{}
	for range netResumeCap {
		h.netWait.resumes[ids[0]] = append(h.netWait.resumes[ids[0]], time.Now())
	}
	h.netWait.mu.Unlock()
	if err := wakeTurn(h, "job done"); !NetworkDown(err) {
		t.Fatalf("the wake's turn: %v", err)
	}
	if h.NetWait().Waiting() {
		t.Error("waiting past the cap")
	}
	if !notes.has("does not wait again") {
		t.Error("no note says the cap was reached")
	}
}

// The engine restarts mid-wait: the sidecar and the open ask start the
// wait again in the new harness, which continues when the provider answers.
func TestNetWait_SurvivesEngineRestart(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	r := &netRunner{nudgeRunner: &nudgeRunner{}}
	p := newFakeProvider(t)
	h := startNet(t, r, p, dir, nil)
	r.down.Store(true)
	_, _ = h.Ask(context.Background(), "migrate the database")
	since := h.NetWait().Since
	if since.IsZero() {
		t.Fatal("not waiting")
	}
	h.Close()
	h2 := startNet(t, r, p, dir, nil)
	if w := h2.NetWait(); !w.Waiting() || !w.Since.Equal(since) {
		t.Fatalf("after the restart: %+v, want since %v", w, since)
	}
	r.down.Store(false)
	p.up(t)
	if !waitWake(t, h2, 2*time.Second) {
		t.Fatal("no wake after the restart")
	}
	n := r.calls()
	if _, err := h2.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.sentHas(n, "continue: migrate the database") {
		t.Error("the resumed wait's continue does not name the ask")
	}
}

func TestNetworkDown(t *testing.T) {
	dns := &net.DNSError{Err: "no such host", Name: "api.example.com"}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("llm: do: %w", dns), true},
		{errors.New("llm: retry budget 5m0s exhausted after 5m3s of transport failures (last: dial tcp: lookup api: no such host)"), true},
		{errors.New("agent: chat: read tcp 10.0.0.2:5555->1.2.3.4:443: read: connection reset by peer"), true},
		{errors.New("dial tcp 1.2.3.4:443: connect: network is unreachable"), true},
		{errors.New("net/http: TLS handshake timeout"), true},
		{errors.New("llm: status 400: bad request"), false},
		{errors.New("llm: status 402: budget"), false},
		{errors.New("upstream returned 502"), false},
		{errors.New("no backend available"), false},
		{errors.New("model produced invalid tool call"), false},
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
		{nil, false},
	} {
		if got := NetworkDown(c.err); got != c.want {
			t.Errorf("NetworkDown(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
