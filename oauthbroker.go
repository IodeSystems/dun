package dun

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
// An engine with no dund (a test, a bare `dun -p`) keeps its own client, as
// before.

// OAuthRequest is one ask of the broker (proto.OAuthReq). Op is token,
// ensure, unauthorized, login, logout or status.
type OAuthRequest struct {
	Op   string    `json:"op"`
	ID   string    `json:"id"`
	URL  string    `json:"url"`
	WWW  string    `json:"www,omitempty"`
	Spec OAuthSpec `json:"spec"`
	// Rejected is, for "unauthorized", the Authorization the server
	// refused: when the broker's is already another (a refresh for another
	// engine), that one is the answer and nothing is refreshed.
	Rejected string `json:"rejected,omitempty"`
}

// OAuthResult is the broker's answer.
type OAuthResult struct {
	Authorization string      `json:"authorization,omitempty"`
	Status        OAuthStatus `json:"status"`
	Revoked       bool        `json:"revoked,omitempty"`
}

// OAuthBroker is dund's: one client per server id.
type OAuthBroker struct {
	open func(string) error

	mu      sync.Mutex
	clients map[string]*oauthClient

	wmu      sync.Mutex
	seq      int
	watchers map[string]map[int]func(id, url string) // a login's URL, to whoever is asking
	shown    map[string]string                       // the URL of a login waiting now
}

// NewOAuthBroker is a broker that opens a login URL with open (nil: only
// shows it).
func NewOAuthBroker(open func(string) error) *OAuthBroker {
	return &OAuthBroker{open: open, clients: map[string]*oauthClient{},
		watchers: map[string]map[int]func(string, string){}, shown: map[string]string{}}
}

// client is req's server's, replaced when its URL or oauth block changed.
func (b *OAuthBroker) client(req OAuthRequest) *oauthClient {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.clients[req.ID]; c != nil && c.url == req.URL && fmt.Sprint(c.spec) == fmt.Sprint(req.Spec) {
		return c
	}
	spec := req.Spec
	c := newOAuthClient(Server{ID: req.ID, URL: req.URL, OAuth: &spec}, b.showLogin, b.open)
	b.clients[req.ID] = c
	return c
}

// showLogin hands a login's URL to every request waiting on that server.
func (b *OAuthBroker) showLogin(id, url string) {
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
func (b *OAuthBroker) watch(id string, notify func(id, url string)) func() {
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
func (b *OAuthBroker) Do(ctx context.Context, req OAuthRequest, notify func(id, url string)) (OAuthResult, error) {
	if req.ID == "" || req.URL == "" {
		return OAuthResult{}, fmt.Errorf("oauth: a server id and url are needed")
	}
	c := b.client(req)
	defer b.watch(req.ID, notify)()
	var res OAuthResult
	var err error
	switch req.Op {
	case "token":
		res.Authorization, err = c.Authorization(ctx)
	case "ensure":
		if err = c.ensure(ctx); err == nil {
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
	res.Status = c.status()
	return res, err
}

// brokered is an engine's view of a server whose token dund holds: each
// method asks the broker. The token is kept until it is about to expire, or
// the server refuses it, so a request does not wait on dund.
type brokered struct {
	id, url string
	spec    OAuthSpec // its client secret resolved here, where the env is
	call    func(ctx context.Context, req OAuthRequest) (OAuthResult, error)

	mu    sync.Mutex
	auth  string
	until time.Time
	last  OAuthStatus
}

func newBrokered(s Server, call func(context.Context, OAuthRequest) (OAuthResult, error)) *brokered {
	spec := *s.OAuth
	spec.ClientSecret = (&oauthClient{spec: spec}).clientSecret()
	return &brokered{id: s.ID, url: s.URL, spec: spec, call: call}
}

func (b *brokered) ask(ctx context.Context, op, www string) (OAuthResult, error) {
	return b.askRejected(ctx, op, www, "")
}

func (b *brokered) askRejected(ctx context.Context, op, www, rejected string) (OAuthResult, error) {
	res, err := b.call(ctx, OAuthRequest{Op: op, ID: b.id, URL: b.url, WWW: www, Spec: b.spec, Rejected: rejected})
	b.mu.Lock()
	if err == nil || res.Status != (OAuthStatus{}) {
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

func (b *brokered) ensure(ctx context.Context) error {
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
func (b *brokered) status() OAuthStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if res, err := b.ask(ctx, "status", ""); err == nil {
		return res.Status
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

// oauthSource is what the harness needs of a server's login: the engine's
// own client, or dund's through brokered.
type oauthSource interface {
	Authorization(ctx context.Context) (string, error)
	OnUnauthorized(ctx context.Context, wwwAuthenticate string) error
	ensure(ctx context.Context) error
	Login(ctx context.Context, wwwAuthenticate string) error
	Logout(ctx context.Context) (bool, error)
	status() OAuthStatus
}

// MarshalOAuthSpec and the three below carry a request's spec and its
// result's status across proto, which does not import dun.
func MarshalOAuthSpec(s OAuthSpec) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func UnmarshalOAuthSpec(s string) (OAuthSpec, error) {
	var o OAuthSpec
	err := json.Unmarshal([]byte(s), &o)
	return o, err
}

func MarshalOAuthStatus(s OAuthStatus) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func UnmarshalOAuthStatus(s string) OAuthStatus {
	var o OAuthStatus
	_ = json.Unmarshal([]byte(s), &o)
	return o
}

// OAuthTokenHere is `dun oauth token` with no dund to ask: this process's
// own client on the shared token file. retry is a server's refusal of the
// last token (mcpshell's MCPSHELL_AUTH_RETRY): refresh, else log in.
func OAuthTokenHere(ctx context.Context, s Server, retry bool, notify func(id, url string), open func(string) error) (string, error) {
	if s.OAuth == nil || s.URL == "" {
		return "", fmt.Errorf("%s: no oauth block and url", s.ID)
	}
	c := newOAuthClient(s, notify, open)
	if retry {
		if err := c.OnUnauthorized(ctx, ""); err != nil {
			return "", err
		}
	}
	a, err := c.Authorization(ctx)
	if err == nil && a == "" {
		if err = c.ensure(ctx); err == nil {
			a, err = c.Authorization(ctx)
		}
	}
	return a, err
}

// OAuthAuthSpec is what `dun oauth token --spec` carries for a server: its
// id, url and oauth block, the client secret as configured ($NAME is read
// where the command runs).
type OAuthAuthSpec struct {
	ID    string    `json:"id"`
	URL   string    `json:"url"`
	OAuth OAuthSpec `json:"oauth"`
}

// EncodeOAuthAuthSpec and DecodeOAuthAuthSpec are `dun oauth token --spec`:
// base64 (URL alphabet) JSON, so it needs no quoting in a shell.
func EncodeOAuthAuthSpec(s OAuthAuthSpec) string {
	b, _ := json.Marshal(s)
	return base64.RawURLEncoding.EncodeToString(b)
}

func DecodeOAuthAuthSpec(v string) (OAuthAuthSpec, error) {
	var s OAuthAuthSpec
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return s, fmt.Errorf("--spec: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("--spec: %w", err)
	}
	if s.ID == "" || s.URL == "" || s.OAuth.ClientID == "" {
		return s, fmt.Errorf("--spec: an id, a url and an oauth client_id are needed")
	}
	return s, nil
}
