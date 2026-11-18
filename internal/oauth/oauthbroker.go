package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// dund holds OAuth (USER 2026-10-29: "dund manages refresh"). One process
// refreshes each server's token and runs its login: the engines and
// mcpshell (through `dun oauth token`, its auth_cmd) ask it. Before, every
// engine refreshed from the shared token file itself, so a server that
// rotates its refresh token could lose one to a race, ten sessions opened
// ten logins, and a scripted (mcpshell-hosted) OAuth server could not be
// had at all. The login still finishes on dund's machine: the redirect is
// its loopback.
//
// An engine with no dund (a test, a bare `dun --engine`) keeps its own client, as
// before.

// Request is one ask of the broker (proto.OAuthReq). Op is token,
// ensure, unauthorized, login, logout or status.
type Request struct {
	Op   string `json:"op"`
	ID   string `json:"id"`
	URL  string `json:"url"`
	WWW  string `json:"www,omitempty"`
	Spec Spec   `json:"spec"`
	// Rejected is, for "unauthorized", the Authorization the server
	// refused: when the broker's is already another (a refresh for another
	// engine), that one is the answer and nothing is refreshed.
	Rejected string `json:"rejected,omitempty"`
}

// Result is the broker's answer.
type Result struct {
	Authorization string `json:"authorization,omitempty"`
	Status        Status `json:"status"`
	Revoked       bool   `json:"revoked,omitempty"`
}

// Broker is dund's: one client per server id.
type Broker struct {
	open func(string) error

	mu      sync.Mutex
	clients map[string]*Client

	wmu      sync.Mutex
	seq      int
	watchers map[string]map[int]func(id, url string) // a login's URL, to whoever is asking
	shown    map[string]string                       // the URL of a login waiting now
}

// NewBroker is a broker that opens a login URL with open (nil: only
// shows it).
func NewBroker(open func(string) error) *Broker {
	return &Broker{open: open, clients: map[string]*Client{},
		watchers: map[string]map[int]func(string, string){}, shown: map[string]string{}}
}

// client is req's server's, replaced when its URL or oauth block changed.
func (b *Broker) client(req Request) *Client {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.clients[req.ID]; c != nil && c.url == req.URL && fmt.Sprint(c.spec) == fmt.Sprint(req.Spec) {
		return c
	}
	spec := req.Spec
	c := NewClient(req.ID, req.URL, spec, b.showLogin, b.open)
	b.clients[req.ID] = c
	return c
}

// showLogin hands a login's URL to every request waiting on that server.
func (b *Broker) showLogin(id, url string) {
	b.wmu.Lock()
	b.shown[id] = url
	ws := make([]func(string, string), 0, len(b.watchers[id]))
	for _, w := range b.watchers[id] {
		ws = append(ws, w)
	}
	b.wmu.Unlock()
	for _, w := range ws {
		w(id, url)
	}
}

// watch registers notify for id's login URL while a request runs; one
// already shown is handed over now. The returned func ends the watch.
func (b *Broker) watch(id string, notify func(id, url string)) func() {
	if notify == nil {
		return func() {}
	}
	b.wmu.Lock()
	b.seq++
	n := b.seq
	if b.watchers[id] == nil {
		b.watchers[id] = map[int]func(string, string){}
	}
	b.watchers[id][n] = notify
	url := b.shown[id]
	b.wmu.Unlock()
	if url != "" {
		notify(id, url)
	}
	return func() {
		b.wmu.Lock()
		delete(b.watchers[id], n)
		b.wmu.Unlock()
	}
}

// Do runs one request. notify, when set, is shown a login's URL if this
// request waits on one.
func (b *Broker) Do(ctx context.Context, req Request, notify func(id, url string)) (Result, error) {
	if req.ID == "" || req.URL == "" {
		return Result{}, fmt.Errorf("oauth: a server id and url are needed")
	}
	c := b.client(req)
	defer b.watch(req.ID, notify)()
	var res Result
	var err error
	switch req.Op {
	case "token":
		res.Authorization, err = c.Authorization(ctx)
	case "ensure":
		if err = c.Ensure(ctx); err == nil {
			res.Authorization, err = c.Authorization(ctx)
		}
	case "unauthorized":
		if cur, _ := c.Authorization(ctx); req.Rejected != "" && cur != "" && cur != req.Rejected {
			res.Authorization = cur
			break
		}
		if err = c.OnUnauthorized(ctx, req.WWW); err == nil {
			res.Authorization, err = c.Authorization(ctx)
		}
	case "login":
		err = c.Login(ctx, req.WWW)
	case "logout":
		res.Revoked, err = c.Logout(ctx)
	case "status":
	default:
		err = fmt.Errorf("oauth: unknown op %q", req.Op)
	}
	if !c.loggingIn.Load() {
		b.wmu.Lock()
		delete(b.shown, req.ID)
		b.wmu.Unlock()
	}
	res.Status = c.Status()
	return res, err
}

// brokered is an engine's view of a server whose token dund holds: each
// method asks the broker. The token is kept until it is about to expire, or
// the server refuses it, so a request does not wait on dund.
type brokered struct {
	id, url string
	spec    Spec // its client secret resolved here, where the env is
	call    func(ctx context.Context, req Request) (Result, error)

	mu    sync.Mutex
	auth  string
	until time.Time
	last  Status
}

// NewBrokered is server id's login through call (dund's broker). spec goes
// as given: resolve its client secret first (ClientSecret) where the env is.
func NewBrokered(id, url string, spec Spec, call func(context.Context, Request) (Result, error)) Source {
	return &brokered{id: id, url: url, spec: spec, call: call}
}

func (b *brokered) ask(ctx context.Context, op, www string) (Result, error) {
	return b.askRejected(ctx, op, www, "")
}

func (b *brokered) askRejected(ctx context.Context, op, www, rejected string) (Result, error) {
	res, err := b.call(ctx, Request{Op: op, ID: b.id, URL: b.url, WWW: www, Spec: b.spec, Rejected: rejected})
	b.mu.Lock()
	if err == nil || res.Status != (Status{}) {
		b.last = res.Status
	}
	if res.Authorization != "" {
		b.auth, b.until = res.Authorization, res.Status.Until
	}
	b.mu.Unlock()
	return res, err
}

func (b *brokered) Authorization(ctx context.Context) (string, error) {
	b.mu.Lock()
	if b.auth != "" && (b.until.IsZero() || time.Now().Add(oauthSkew).Before(b.until)) {
		a := b.auth
		b.mu.Unlock()
		return a, nil
	}
	b.auth = ""
	b.mu.Unlock()
	res, err := b.ask(ctx, "token", "")
	return res.Authorization, err
}

func (b *brokered) OnUnauthorized(ctx context.Context, www string) error {
	b.mu.Lock()
	rejected := b.auth
	b.auth = ""
	b.mu.Unlock()
	_, err := b.askRejected(ctx, "unauthorized", www, rejected)
	return err
}

func (b *brokered) Ensure(ctx context.Context) error {
	_, err := b.ask(ctx, "ensure", "")
	return err
}

func (b *brokered) Login(ctx context.Context, www string) error {
	b.mu.Lock()
	b.auth = ""
	b.mu.Unlock()
	_, err := b.ask(ctx, "login", www)
	return err
}

func (b *brokered) Logout(ctx context.Context) (bool, error) {
	b.mu.Lock()
	b.auth = ""
	b.mu.Unlock()
	res, err := b.ask(ctx, "logout", "")
	return res.Revoked, err
}

// status asks dund, briefly: the /mcp listing waits on it. Unanswered, it
// is the last state dund gave.
func (b *brokered) Status() Status {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if res, err := b.ask(ctx, "status", ""); err == nil {
		return res.Status
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

// Source is what the harness needs of a server's login: the engine's
// own client, or dund's through brokered.
type Source interface {
	Authorization(ctx context.Context) (string, error)
	OnUnauthorized(ctx context.Context, wwwAuthenticate string) error
	Ensure(ctx context.Context) error
	Login(ctx context.Context, wwwAuthenticate string) error
	Logout(ctx context.Context) (bool, error)
	Status() Status
}

// MarshalSpec and the three below carry a request's spec and its
// result's status across proto, which does not import dun.
func MarshalSpec(s Spec) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func UnmarshalSpec(s string) (Spec, error) {
	var o Spec
	err := json.Unmarshal([]byte(s), &o)
	return o, err
}

func MarshalStatus(s Status) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func UnmarshalStatus(s string) Status {
	var o Status
	_ = json.Unmarshal([]byte(s), &o)
	return o
}

// TokenHere is `dun oauth token` with no dund to ask: this process's
// own client on the shared token file. retry is a server's refusal of the
// last token (mcpshell's MCPSHELL_AUTH_RETRY): refresh, else log in.
func TokenHere(ctx context.Context, s AuthSpec, retry bool, notify func(id, url string), open func(string) error) (string, error) {
	if s.URL == "" {
		return "", fmt.Errorf("%s: no oauth block and url", s.ID)
	}
	c := NewClient(s.ID, s.URL, s.OAuth, notify, open)
	if retry {
		if err := c.OnUnauthorized(ctx, ""); err != nil {
			return "", err
		}
	}
	a, err := c.Authorization(ctx)
	if err == nil && a == "" {
		if err = c.Ensure(ctx); err == nil {
			a, err = c.Authorization(ctx)
		}
	}
	return a, err
}

// AuthSpec is what `dun oauth token --spec` carries for a server: its
// id, url and oauth block, the client secret as configured ($NAME is read
// where the command runs).
type AuthSpec struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	OAuth Spec   `json:"oauth"`
}

// EncodeAuthSpec and DecodeAuthSpec are `dun oauth token --spec`:
// base64 (URL alphabet) JSON, so it needs no quoting in a shell.
func EncodeAuthSpec(s AuthSpec) string {
	b, _ := json.Marshal(s)
	return base64.RawURLEncoding.EncodeToString(b)
}

func DecodeAuthSpec(v string) (AuthSpec, error) {
	var s AuthSpec
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return s, fmt.Errorf("--spec: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("--spec: %w", err)
	}
	if s.ID == "" || s.URL == "" {
		return s, fmt.Errorf("--spec: an id and a url are needed")
	}
	return s, nil
}
