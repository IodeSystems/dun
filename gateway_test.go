package dun

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// gwLink is an engine's way to the gateway, as dund's peer op is.
type gwLink struct {
	g *Gateway
	c GatewayClient
}

func (l gwLink) Share(ctx context.Context, req ShareRequest) (ShareGrant, error) {
	return l.g.Share(ctx, l.c, req)
}

func (l gwLink) Release(_ context.Context, bearer string) error {
	l.g.Release(bearer)
	return nil
}

// countingMCP is a remote MCP server that counts its connections' handshakes and echoes
// its one tool's argument.
func countingMCP(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var inits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments  map[string]any `json:"arguments"`
				ClientInfo struct {
					Version string `json:"version"`
				} `json:"clientInfo"`
			} `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &msg)
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			if msg.Params.ClientInfo.Version != "probe" { // AddServer's OAuth probe is no connection
				inits.Add(1)
			}
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "counter", "version": "1.2"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "echo", "description": "echoes x",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}}}}
		case "tools/call":
			x, _ := msg.Params.Arguments["x"].(string)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "echo " + x}}, "isError": x == "fail"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}))
	t.Cleanup(ts.Close)
	return ts, &inits
}

func newGateway(t *testing.T) *Gateway {
	g := NewGateway(NewOAuthBroker(nil))
	t.Cleanup(g.Close)
	return g
}

// gatewayHarness is an engine under dund: its shared servers go through g.
func gatewayHarness(t *testing.T, g *Gateway, session string, login func(id, url string)) *Harness {
	h := newServerHarness(t)
	h.cfg.Gateway = gwLink{g: g, c: GatewayClient{Session: session, Engine: h, Login: login,
		Notify: func(key, method string, params map[string]any) { h.GatewayNotify(key, method, params) }}}
	return h
}

// Two sessions that resolve a server the same start ONE upstream: one
// handshake, and both call its tool through it.
func TestGateway_TwoSessionsOneUpstream(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts, inits := countingMCP(t)
	g := newGateway(t)
	ctx := context.Background()
	h1 := gatewayHarness(t, g, "s1", nil)
	h2 := gatewayHarness(t, g, "s2", nil)
	for _, h := range []*Harness{h1, h2} {
		if _, err := h.AddServer(ctx, ServerSpec{ID: "ctr", URL: ts.URL}); err != nil {
			t.Fatal(err)
		}
		if out, err := h.mgr.CallTool(ctx, "ctr", "echo", map[string]any{"x": "hi"}); err != nil || out != "echo hi" {
			t.Fatalf("call: %q %v", out, err)
		}
	}
	if n := inits.Load(); n != 1 || g.Starts() != 1 {
		t.Fatalf("upstream handshakes %d, starts %d: want one", n, g.Starts())
	}
	// The engine sees the upstream's own handshake and tools.
	if info, ok := h2.mgr.ServerInfo("ctr"); !ok || info.Name != "counter" || info.Version != "1.2" {
		t.Errorf("server info: %+v", info)
	}
	if !strings.Contains(strings.Join(modelTools(h1), ","), "ctr__echo") {
		t.Errorf("tools: %v", modelTools(h1))
	}
	// A tool error stays a tool error, worded as a direct one is.
	if _, err := h1.mgr.CallTool(ctx, "ctr", "echo", map[string]any{"x": "fail"}); err == nil || err.Error() != "mcp tool error: [echo fail]" {
		t.Errorf("tool error: %v", err)
	}
	// session: this engine's own connection, a handshake of its own.
	if _, err := h2.SetServerInstance(ctx, "ctr", ScopeSession, InstanceSession); err != nil {
		t.Fatal(err)
	}
	if n := inits.Load(); n != 2 || !h2.mgr.ServerStarted("ctr") {
		t.Errorf("session instance: %d handshakes, started %v", n, h2.mgr.ServerStarted("ctr"))
	}
	if h2.shared["ctr"] != (ShareGrant{}) {
		t.Error("a session instance still names the gateway")
	}
}

// A client's DELETE (the manager's stop) and its engine's exit release it;
// the upstream stops a grace after its last client, and a share in the
// grace keeps it.
func TestGateway_ReleaseAndGrace(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	old := GatewayGrace
	GatewayGrace = 100 * time.Millisecond
	t.Cleanup(func() { GatewayGrace = old })
	ts, inits := countingMCP(t)
	g := newGateway(t)
	ctx := context.Background()
	h1 := gatewayHarness(t, g, "s1", nil)
	if _, err := h1.AddServer(ctx, ServerSpec{ID: "ctr", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	key := h1.shared["ctr"].Key
	if err := h1.StopServer("ctr"); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	n := g.clientsOf(key)
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("the stop's DELETE left %d clients", n)
	}
	// Back within the grace: the same upstream.
	if err := h1.StartServer(ctx, "ctr"); err != nil {
		t.Fatal(err)
	}
	if inits.Load() != 1 {
		t.Errorf("a share within the grace started it again: %d handshakes", inits.Load())
	}
	g.ReleaseEngine(h1) // the engine exited
	time.Sleep(300 * time.Millisecond)
	if g.Running(key) {
		t.Error("still running a grace after its last client")
	}
}

// An upstream the gateway dropped answers 404; the engine's call asks for
// the endpoint again, once, and goes through.
func TestGateway_RegainAfterDrop(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts, inits := countingMCP(t)
	g := newGateway(t)
	ctx := context.Background()
	h := gatewayHarness(t, g, "s1", nil)
	if _, err := h.AddServer(ctx, ServerSpec{ID: "ctr", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	up := g.ups[h.shared["ctr"].Key]
	g.mu.Unlock()
	g.drop(up)
	tools := filterTools(h.mgr.GetTools(), h.specs)
	call := mcpDispatcher(h.mgr, tools, nil, h.regainShared)
	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = "ctr__echo", `{"x":"again"}`
	out, err := call(ctx, tc)
	if err != nil || out != "echo again" {
		t.Fatalf("after the drop: %q %v", out, err)
	}
	if inits.Load() != 2 {
		t.Errorf("handshakes: %d, want the restart's second", inits.Load())
	}
}

// An upstream's notification reaches each attached engine once, as its
// own server's.
func TestGateway_NotificationFanOut(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts, _ := countingMCP(t)
	g := newGateway(t)
	ctx := context.Background()
	var mu sync.Mutex
	got := map[string]int{}
	share := func(engine string) ShareGrant {
		gr, err := g.Share(ctx, GatewayClient{Session: engine, Engine: engine, Notify: func(key, method string, _ map[string]any) {
			mu.Lock()
			got[engine+" "+method]++
			mu.Unlock()
		}}, ShareRequest{Spec: ShareSpec{ID: "ctr", URL: ts.URL}, Instance: InstanceShared})
		if err != nil {
			t.Fatal(err)
		}
		return gr
	}
	gr := share("e1")
	share("e1") // a second client of the same engine: still one notification
	share("e2")
	g.notify(mcpmgr.Notification{ServerID: gr.Key, Method: "notifications/message", Params: map[string]any{"data": "hi"}})
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if got["e1 notifications/message"] != 1 || got["e2 notifications/message"] != 1 || len(got) != 2 {
		t.Errorf("fan-out: %v", got)
	}
}

// agent adds the session and the agent number to the key: its own
// upstream.
func TestShareRequest_Key(t *testing.T) {
	sp := ShareSpec{ID: "x", Command: "/bin/x", Args: []string{"--root", "/a"}}
	shared := ShareRequest{Spec: sp, Instance: InstanceShared}
	if shared.Key("s1") != shared.Key("s2") {
		t.Error("shared: one key for every session")
	}
	other := ShareRequest{Spec: ShareSpec{ID: "x", Command: "/bin/x", Args: []string{"--root", "/b"}}, Instance: InstanceShared}
	if other.Key("s1") == shared.Key("s1") {
		t.Error("another workspace's args: another key")
	}
	a0 := ShareRequest{Spec: sp, Instance: InstanceAgent}
	a1 := ShareRequest{Spec: sp, Instance: InstanceAgent, Agent: 1}
	if a0.Key("s1") == a1.Key("s1") || a0.Key("s1") == a0.Key("s2") || a0.Key("s1") == shared.Key("s1") {
		t.Error("agent: a key per session and agent")
	}
}

// An OAuth server logs in once for every session: the gateway's upstream
// asks dund's broker, the login's URL goes to the session that started it,
// and no engine runs a login of its own.
func TestGateway_OAuthLogsInOnce(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	w := newOAuthWorld(t)
	br := &browser{}
	g := NewGateway(NewOAuthBroker(nil))
	t.Cleanup(g.Close)
	var mu sync.Mutex
	shown := map[string]int{}
	login := func(session string) func(id, url string) {
		return func(id, url string) {
			mu.Lock()
			shown[session]++
			mu.Unlock()
			br.notify(id, url)
		}
	}
	spec := ServerSpec{ID: "snow", URL: w.mcpURL(), OAuth: &OAuthSpec{ClientID: w.clientID, RedirectPort: freePort(t)}}
	ctx := context.Background()
	for _, s := range []string{"s1", "s2"} {
		h := gatewayHarness(t, g, s, login(s))
		h.cfg.OnLogin = func(string, string) { t.Error("the engine showed a login: the gateway's to show") }
		h.cfg.OAuthBroker = func(context.Context, OAuthRequest) (OAuthResult, error) {
			t.Error("the engine asked for a token: the gateway's upstream holds it")
			return OAuthResult{}, nil
		}
		if _, err := h.AddServer(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if _, err := h.mgr.CallTool(ctx, "snow", "query", nil); err != nil && !strings.Contains(err.Error(), "mcp") {
			t.Fatal(err)
		}
	}
	if br.count() != 1 || g.Starts() != 1 {
		t.Errorf("logins %d, starts %d: want one each", br.count(), g.Starts())
	}
	mu.Lock()
	defer mu.Unlock()
	if shown["s1"] != 1 || shown["s2"] != 0 {
		t.Errorf("the login's URL went to %v: want the session that started it", shown)
	}
}
