package dun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// One connection per server (docs/mcp-and-tools.md, "One connection per
// server: dund's gateway"). dund runs one Gateway: for each shared server
// it holds one upstream connection (an mcpmgr.Manager entry, as an engine
// does) and serves it as a Streamable HTTP endpoint on a loopback listener.
// Every engine's manager, and mcpshell, connects to that endpoint as to any
// remote server, each with its own bearer. The control path (the engine
// asking for an endpoint, its exit releasing it) is dund's: internal/dund
// mcpgw.go. The gateway lives here, beside the OAuth broker and the
// harness's own start, which it reuses.

// ShareSpec is a server as the engine resolves it: the command found on
// PATH, its args and env with placeholders and $NAME values read, the url
// with its headers resolved, the oauth block with its client secret read,
// and the timeout. Two sessions that resolve a server the same share one
// upstream; any difference is another one.
type ShareSpec struct {
	ID      string            `json:"id"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     []string          `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	OAuth   *OAuthSpec        `json:"oauth,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
}

// ShareRequest is an engine's ask for an endpoint: the spec, its instance,
// and for InstanceAgent the sub-agent's path in its session ("2", "2.1").
type ShareRequest struct {
	Spec     ShareSpec `json:"spec"`
	Instance string    `json:"instance,omitempty"`
	Agent    string    `json:"agent,omitempty"`
}

// ShareGrant is the gateway's answer: the endpoint, this client's bearer,
// and the upstream's key (mcp_notify names it).
type ShareGrant struct {
	URL    string `json:"url"`
	Bearer string `json:"bearer"`
	Key    string `json:"key"`
}

// MCPGateway is how an engine reaches dund's gateway (Config.Gateway): nil
// with no dund, and every server is the engine's own.
type MCPGateway interface {
	Share(ctx context.Context, req ShareRequest) (ShareGrant, error)
	// Release gives back a bearer whose client will not close its MCP
	// session itself (a shell that was stopped).
	Release(ctx context.Context, bearer string) error
}

// GatewayClient is who asks for an endpoint: its session (whose agent
// numbers key InstanceAgent), its engine (whose exit releases its
// clients, and which gets one notification however many clients it has),
// and how to reach that session.
type GatewayClient struct {
	Session string
	Engine  any // comparable
	// Login shows a login's URL to the session (mcp_login).
	Login func(id, url string)
	// Notify hands an upstream's notification to the engine (mcp_notify).
	Notify func(key, method string, params map[string]any)
}

// GatewayGrace is how long an upstream with no client keeps running: a
// server switched off and on, or a session resumed, does not pay the start
// again. A var for tests.
var GatewayGrace = 2 * time.Minute

// Gateway is dund's MCP gateway.
type Gateway struct {
	broker *OAuthBroker
	mgr    *mcpmgr.Manager

	mu      sync.Mutex
	ln      net.Listener
	srv     *http.Server
	ups     map[string]*gwUpstream
	clients map[string]*gwClient // by bearer
	closed  bool
	starts  int // upstream starts, for tests
}

type gwUpstream struct {
	key  string
	spec ShareSpec

	ready chan struct{} // closed when the start ended; err is then set
	err   error

	timer *time.Timer // the grace stop, while it has no client

	lmu      sync.Mutex
	loginTo  map[string]func(id, url string) // sessions shown a login's URL
	loginURL string
}

type gwClient struct {
	bearer   string
	key      string
	who      GatewayClient
	sessions map[string]bool // its MCP sessions (Mcp-Session-Id)
}

// NewGateway is a gateway whose OAuth servers log in through b.
func NewGateway(b *OAuthBroker) *Gateway {
	g := &Gateway{broker: b, mgr: mcpmgr.NewManager(), ups: map[string]*gwUpstream{}, clients: map[string]*gwClient{}}
	g.mgr.SetNotificationHandler(g.notify)
	return g
}

// Key is the upstream a request shares: the spec, and for InstanceAgent
// the session and the agent's path.
func (r ShareRequest) Key(session string) string {
	b, _ := json.Marshal(r.Spec)
	if r.Instance == InstanceAgent {
		b = append(b, "\x00"+session+"\x00"+r.Agent...)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// Share gives c an endpoint for req's upstream, starting it first if none
// runs: the first client waits for its handshake and tools (and a login),
// later ones get the running one at once.
func (g *Gateway) Share(ctx context.Context, c GatewayClient, req ShareRequest) (ShareGrant, error) {
	if req.Spec.ID == "" || (req.Spec.Command == "") == (req.Spec.URL == "") {
		return ShareGrant{}, errors.New("mcp_share: a server id and a command or a url are needed")
	}
	base, err := g.listen()
	if err != nil {
		return ShareGrant{}, err
	}
	key := req.Key(c.Session)
	cl := &gwClient{bearer: randomHex(24), key: key, who: c, sessions: map[string]bool{}}
	g.mu.Lock()
	up := g.ups[key]
	if up != nil && up.failed() {
		up = nil // dropped as it failed; this ask starts it again
	}
	if up == nil {
		up = &gwUpstream{key: key, spec: req.Spec, ready: make(chan struct{}), loginTo: map[string]func(string, string){}}
		g.ups[key] = up
		g.starts++
		go g.start(up)
	}
	if up.timer != nil {
		up.timer.Stop()
		up.timer = nil
	}
	g.clients[cl.bearer] = cl
	g.mu.Unlock()
	select {
	case <-up.ready:
	default:
		up.watchLogin(c.Session, c.Login)
		select {
		case <-up.ready:
		case <-ctx.Done():
			g.Release(cl.bearer)
			return ShareGrant{}, ctx.Err()
		}
	}
	if up.err != nil {
		g.Release(cl.bearer)
		return ShareGrant{}, up.err
	}
	return ShareGrant{URL: base + "/mcp/" + key, Bearer: cl.bearer, Key: key}, nil
}

func (u *gwUpstream) failed() bool {
	select {
	case <-u.ready:
		return u.err != nil
	default:
		return false
	}
}

// watchLogin shows session a login of this upstream: one waiting now, and
// any while the start runs.
func (u *gwUpstream) watchLogin(session string, show func(id, url string)) {
	if show == nil {
		return
	}
	u.lmu.Lock()
	u.loginTo[session] = show
	url := u.loginURL
	u.lmu.Unlock()
	if url != "" {
		show(u.spec.ID, url)
	}
}

func (u *gwUpstream) showLogin(id, url string) {
	u.lmu.Lock()
	u.loginURL = url
	to := make([]func(string, string), 0, len(u.loginTo))
	for _, f := range u.loginTo {
		to = append(to, f)
	}
	u.lmu.Unlock()
	for _, f := range to {
		f(id, url)
	}
}

// start connects the upstream: an OAuth login first (through the broker,
// outside the handshake's timeout), then the handshake and its tools.
func (g *Gateway) start(up *gwUpstream) {
	s := up.spec
	timeout := time.Duration(s.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx := context.Background()
	cfg := mcpmgr.MCPConfig{ID: up.key, Name: s.ID, Command: s.Command, Args: s.Args, Env: s.Env,
		Timeout: int(timeout / time.Second), URL: s.URL, Headers: s.Headers}
	var err error
	if s.OAuth != nil && s.URL != "" {
		src := &brokered{id: s.ID, url: s.URL, spec: *s.OAuth, call: func(ctx context.Context, req OAuthRequest) (OAuthResult, error) {
			return g.broker.Do(ctx, req, up.showLogin)
		}}
		err = src.ensure(ctx)
		cfg.Authorization, cfg.OnUnauthorized = src.Authorization, src.OnUnauthorized
	}
	if err == nil {
		err = g.mgr.StartServer(ctx, cfg)
	}
	if err == nil {
		if err = waitServerTools(ctx, g.mgr, up.key, timeout); err != nil {
			g.mgr.StopServer(up.key)
		}
	}
	g.mu.Lock()
	up.err = err
	close(up.ready)
	if err != nil {
		if g.ups[up.key] == up {
			delete(g.ups, up.key)
		}
		log.Printf("dund: gateway: %s did not start: %v", s.ID, err)
	} else if g.clientsOf(up.key) == 0 {
		g.graceStop(up) // every asker gave up while it started
	}
	g.mu.Unlock()
}

// clientsOf counts key's clients. Caller holds mu.
func (g *Gateway) clientsOf(key string) int {
	n := 0
	for _, c := range g.clients {
		if c.key == key {
			n++
		}
	}
	return n
}

// graceStop stops up GatewayGrace from now unless a client comes. Caller
// holds mu.
func (g *Gateway) graceStop(up *gwUpstream) {
	if up.timer != nil {
		up.timer.Stop()
	}
	up.timer = time.AfterFunc(GatewayGrace, func() {
		g.mu.Lock()
		stop := g.ups[up.key] == up && g.clientsOf(up.key) == 0
		if stop {
			delete(g.ups, up.key)
		}
		g.mu.Unlock()
		if stop {
			g.mgr.StopServer(up.key)
		}
	})
}

// Release gives back one client. Its upstream stops GatewayGrace after its
// last client.
func (g *Gateway) Release(bearer string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.clients[bearer]
	if c == nil {
		return
	}
	delete(g.clients, bearer)
	if up := g.ups[c.key]; up != nil && !up.failed() && g.clientsOf(c.key) == 0 {
		select {
		case <-up.ready:
			g.graceStop(up)
		default: // still starting: start sets the grace stop
		}
	}
}

// ReleaseEngine gives back every client of an engine that exited.
func (g *Gateway) ReleaseEngine(engine any) {
	g.mu.Lock()
	var bearers []string
	for b, c := range g.clients {
		if c.who.Engine == engine {
			bearers = append(bearers, b)
		}
	}
	g.mu.Unlock()
	for _, b := range bearers {
		g.Release(b)
	}
}

// drop forgets an upstream that is gone (its process exited, its remote
// ended the session) and its clients: their next request is answered 404,
// and the engine asks again (Harness.regainShared).
func (g *Gateway) drop(up *gwUpstream) {
	g.mu.Lock()
	if g.ups[up.key] != up {
		g.mu.Unlock()
		return
	}
	delete(g.ups, up.key)
	for b, c := range g.clients {
		if c.key == up.key {
			delete(g.clients, b)
		}
	}
	g.mu.Unlock()
	g.mgr.StopServer(up.key)
}

// Starts is how many upstream starts the gateway has made.
func (g *Gateway) Starts() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.starts
}

// Clients counts key's clients.
func (g *Gateway) Clients(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.clientsOf(key)
}

// Running reports whether key's upstream runs.
func (g *Gateway) Running(key string) bool {
	g.mu.Lock()
	up := g.ups[key]
	g.mu.Unlock()
	return up != nil && !up.failed() && g.mgr.ServerStarted(key)
}

// Close stops every upstream and the listener.
func (g *Gateway) Close() {
	g.mu.Lock()
	g.closed = true
	srv := g.srv
	for _, up := range g.ups {
		if up.timer != nil {
			up.timer.Stop()
		}
	}
	g.ups, g.clients = map[string]*gwUpstream{}, map[string]*gwClient{}
	g.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
	g.mgr.Close()
}

// listen starts the loopback listener once; its base URL.
func (g *Gateway) listen() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return "", errors.New("mcp gateway: closed")
	}
	if g.ln == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", fmt.Errorf("mcp gateway: %w", err)
		}
		g.ln = ln
		g.srv = &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = g.srv.Serve(ln) }()
	}
	return "http://" + g.ln.Addr().String(), nil
}

// notify fans an upstream's notification out to every engine attached to
// it, once each.
func (g *Gateway) notify(n mcpmgr.Notification) {
	g.mu.Lock()
	seen := map[any]bool{}
	var to []func(string, string, map[string]any)
	for _, c := range g.clients {
		if c.key != n.ServerID || c.who.Notify == nil || seen[c.who.Engine] {
			continue
		}
		seen[c.who.Engine] = true
		to = append(to, c.who.Notify)
	}
	g.mu.Unlock()
	go func() {
		for _, f := range to {
			f(n.ServerID, n.Method, n.Params)
		}
	}()
}

// errNoSuchServer is the gateway's 404 body: an engine whose call gets it
// asks for the endpoint again.
const errNoSuchServer = "no such server"

// ServeHTTP is the endpoint: POST JSON-RPC (answered as application/json),
// DELETE ending the client.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, _ := strings.CutPrefix(r.URL.Path, "/mcp/")
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.mu.Lock()
	c := g.clients[bearer]
	var up *gwUpstream
	if c != nil && c.key == key {
		up = g.ups[key]
	}
	g.mu.Unlock()
	if up == nil || up.failed() {
		http.Error(w, errNoSuchServer, http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		g.Release(bearer)
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodPost:
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "POST or DELETE", http.StatusMethodNotAllowed)
		return
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&msg); err != nil {
		http.Error(w, "not JSON-RPC: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(msg.ID) == 0 || msg.Method == "" { // a notification, or a reply to nothing asked
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if sid := r.Header.Get("Mcp-Session-Id"); msg.Method != "initialize" && sid != "" {
		g.mu.Lock()
		ok := c.sessions[sid]
		g.mu.Unlock()
		if !ok {
			http.Error(w, errNoSuchServer, http.StatusNotFound)
			return
		}
	}
	result, rpcErr := g.answer(r.Context(), up, msg.Method, msg.Params)
	if msg.Method == "initialize" && rpcErr == nil {
		sid := randomHex(16)
		g.mu.Lock()
		c.sessions[sid] = true
		g.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", sid)
	}
	out := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
	if rpcErr != nil {
		out["error"] = rpcErr
	} else {
		out["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type gwRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// answer is one request, answered from the upstream: initialize from its
// handshake, tools/list from its tools, the rest forwarded.
func (g *Gateway) answer(ctx context.Context, up *gwUpstream, method string, raw json.RawMessage) (any, *gwRPCError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		URI       string         `json:"uri"`
	}
	_ = json.Unmarshal(raw, &p)
	fail := func(err error) *gwRPCError {
		msg := err.Error()
		if strings.Contains(msg, "transport closed") || strings.Contains(msg, "ended the session") {
			g.drop(up)
		}
		return &gwRPCError{Code: -32603, Message: msg}
	}
	switch method {
	case "initialize":
		info, ok := g.mgr.ServerInfo(up.key)
		if !ok {
			g.drop(up)
			return nil, &gwRPCError{Code: -32603, Message: up.spec.ID + ": the upstream is gone"}
		}
		return info.Raw, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools := []map[string]any{}
		for _, t := range g.mgr.GetTools() {
			if t.ServerID == up.key {
				tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
			}
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		text, err := g.mgr.CallTool(ctx, up.key, p.Name, p.Arguments)
		if err != nil {
			msg := err.Error()
			if m, ok := strings.CutPrefix(msg, "mcp tool error: "); ok {
				m = strings.TrimSuffix(strings.TrimPrefix(m, "["), "]")
				return map[string]any{"content": []map[string]any{{"type": "text", "text": m}}, "isError": true}, nil
			}
			return nil, fail(errors.New(strings.TrimPrefix(msg, "mcp call "+p.Name+": ")))
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}, nil
	case "resources/list":
		rs, err := g.mgr.ListResources(ctx, up.key)
		if err != nil {
			return nil, fail(err)
		}
		return map[string]any{"resources": rs}, nil
	case "resources/read":
		cs, err := g.mgr.ReadResource(ctx, up.key, p.URI)
		if err != nil {
			return nil, fail(err)
		}
		return map[string]any{"contents": cs}, nil
	case "prompts/list":
		ps, err := g.mgr.ListPrompts(ctx, up.key)
		if err != nil {
			return nil, fail(err)
		}
		return map[string]any{"prompts": ps}, nil
	case "prompts/get":
		var gp struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		_ = json.Unmarshal(raw, &gp)
		desc, msgs, err := g.mgr.GetPrompt(ctx, up.key, gp.Name, gp.Arguments)
		if err != nil {
			return nil, fail(err)
		}
		return map[string]any{"description": desc, "messages": msgs}, nil
	}
	return nil, &gwRPCError{Code: -32601, Message: "method not found: " + method}
}
