// Package dundapi is dund's HTTP surface: huma operations at /api/v1, the
// same ones as GraphQL at /api/graphql through gat (API-7), and /healthz.
//
// Every operation but hello needs a bearer token, checked by HTTP middleware
// in front of both /api/v1 and /api/graphql: gat calls the operations
// in-process, so huma's per-operation middlewares never run for GraphQL.
// Each handler also refuses to run without a principal. Today the token is
// the local token
// (AUTH-2, see dund.NewToken), which stands for the user the service runs
// as — so a browser holding it sees the same sessions as that user's TUI.
// OIDC principals come with remote access (caveat 10).
//
// Live events: GET /api/v1/sessions/{id}/events long-polls — a stopgap
// until gat carries graphql-ws subscriptions (RT-1..3).
package dundapi

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/gwag/v2/gw/gat"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/proto"
)

// Version is reported by /healthz and /api/v1/hello.
var Version = "dev"

const (
	pollWaitMax = 25 * time.Second // under hz's 50 s server timeout
	pollMax     = 2000
)

type principalKey struct{}

func principal(ctx context.Context) string {
	p, _ := ctx.Value(principalKey{}).(string)
	return p
}

// Server holds what the handlers need.
type Server struct {
	svc *dund.Service
	// owner is the principal the local token stands for: the user the
	// service runs as (unix:<uid>), the same one its socket clients are.
	owner    string
	api      huma.API
	signer   *subSigner
	releases string
	sso      *sso // nil: sign-in with the identity provider is off
}

// Options are what the HTTP surface serves besides the API.
type Options struct {
	// Owner is the principal the local token stands for (unix:<uid>).
	Owner string
	// WebRoot, when set, is the built client (mobile: expo export --platform
	// web) served at / from the filesystem (WEB-5); without it / is the
	// embedded stopgap page.
	WebRoot string
	// OIDC, when its issuer and client id are set, turns on sign-in with
	// the identity provider (oidc.go).
	OIDC OIDC
	// Releases, when set, is where tools/release-android publishes:
	// android/latest.json and the APKs, served under /releases with /dun.apk
	// the latest, and announced by /api/v1/hello.
	Releases string
}

// Handler is the whole HTTP surface.
func Handler(svc *dund.Service, o Options) (http.Handler, error) {
	if o.WebRoot != "" {
		if st, err := os.Stat(filepath.Join(o.WebRoot, "index.html")); err != nil || st.IsDir() {
			return nil, fmt.Errorf("web root %s has no index.html", o.WebRoot)
		}
	}
	return newRouter(svc, o, true)
}

// spa serves webRoot's files, and its index.html for any other path: a
// client route (/session/…) is the app's shell, which routes itself.
func spa(webRoot string) http.HandlerFunc {
	files := http.FileServer(http.Dir(webRoot))
	return func(w http.ResponseWriter, req *http.Request) {
		p := filepath.Join(webRoot, filepath.Clean("/"+req.URL.Path))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			files.ServeHTTP(w, req)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, req, filepath.Join(webRoot, "index.html"))
	}
}

// SDL is the GraphQL schema, for client codegen (WEB-4). No service runs:
// building the schema calls no handler.
func SDL() (string, error) {
	h, err := newRouter(nil, Options{}, false)
	if err != nil {
		return "", err
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/schema/graphql", nil))
	if rec.Code != http.StatusOK {
		return "", fmt.Errorf("schema: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String(), nil
}

func newRouter(svc *dund.Service, o Options, auth bool) (http.Handler, error) {
	r := chi.NewRouter()
	s := &Server{svc: svc, owner: o.Owner, signer: newSigner(), releases: o.Releases}
	if o.OIDC.on() {
		s.sso = newSSO(o.OIDC, svc, o.Owner)
	}
	webRoot := o.WebRoot
	if auth {
		r.Use(s.authenticate)
	}
	page := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(indexHTML)
	}
	if s.sso != nil {
		r.Get(loginPath, s.sso.login)
		r.Get(callbackRP, s.sso.callback)
	}
	if o.Releases != "" {
		r.Handle(releasesPath+"/*", http.StripPrefix(releasesPath, releaseFiles(o.Releases)))
		r.Get(apkPath, s.latestAPK)
	}
	switch {
	case webRoot != "":
		r.NotFound(spa(webRoot))
	case auth: // the stopgap page, when there is no built client
		r.Get("/", page)
		r.Get("/index.html", page)
	}
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok " + Version + "\n"))
	})
	cfg := huma.DefaultConfig("dund", Version)
	api := humachi.New(r, cfg)
	s.api = api
	g, err := gat.New()
	if err != nil {
		return nil, err
	}
	s.register(api, g)
	s.registerStream(api, g)
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		return nil, err
	}
	return r, nil
}

// open are the API paths anyone may reach. Everything outside /api is the
// client (pages and assets, which hold no data) and is open too; it asks for
// the token and calls the API with it.
var open = map[string]bool{"/api/v1/hello": true}

// indexHTML is the stopgap browser client (web/index.html). Embedded: dund is
// a tool binary with no slot model (WEB-5's exception).
//
//go:embed web/index.html
var indexHTML []byte

// authenticate 401s a request without the local token, except to the open
// paths, and puts the caller's principal in the request context — which is
// the context every handler gets, whether called as REST or through gat.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if open[r.URL.Path] || !strings.HasPrefix(r.URL.Path, "/api/") || isUpgrade(r) {
			next.ServeHTTP(w, r)
			return
		}
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		who, ok := s.principalFor(strings.TrimSpace(tok))
		if !ok {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"detail":"a valid bearer token is required"}`))
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, who)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, bearerKey{}, strings.TrimSpace(tok))))
	})
}

type bearerKey struct{}

// principalFor is whom a bearer token stands for: the local token and a
// sign-in token both stand for the owner.
func (s *Server) principalFor(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	if s.svc.CheckToken(tok) {
		return s.owner, true
	}
	return s.svc.CheckSignIn(tok)
}

// who is the caller, or a 401 when the request was somehow not
// authenticated: no handler runs on the empty principal.
func who(ctx context.Context) (string, error) {
	if p := principal(ctx); p != "" {
		return p, nil
	}
	return "", huma.Error401Unauthorized("a valid bearer token is required")
}

// --- wire types ---

type HelloOutput struct {
	Body struct {
		Version  string `json:"version"`
		Protocol int    `json:"protocol"`
		// Android is the latest released APK; version_code 0 when none.
		Android AndroidRelease `json:"android"`
		// SignIn is whether /auth/login (the identity provider) is on.
		SignIn bool `json:"sign_in"`
	}
}

type SessionsOutput struct {
	Body struct {
		Items []proto.SessionSummary `json:"items"`
	}
}

type CreateInput struct {
	Body struct {
		Workspace string   `json:"workspace"`
		Args      []string `json:"args,omitempty"`
		Resume    string   `json:"resume,omitempty"`
	}
}

type IDOutput struct {
	Body struct {
		ID string `json:"id"`
	}
}

type SessionPath struct {
	ID string `path:"id"`
}

type MessageInput struct {
	SessionPath
	Body struct {
		Content string `json:"content"`
	}
}

type AnswerInput struct {
	SessionPath
	Body struct {
		Value string `json:"value"`
	}
}

type ControlInput struct {
	SessionPath
	Body struct {
		Kind   string `json:"kind" enum:"control,server" doc:"control: /isolate, /worktree, /mcp…; server: /raglit, /poly-lsp"`
		Target string `json:"target"`
		Action string `json:"action"`
	}
}

type SwitchModelInput struct {
	SessionPath
	Body struct {
		URL   string `json:"url" doc:"the provider's base URL, from the models event"`
		Model string `json:"model"`
	}
}

type RestartInput struct {
	SessionPath
	Body struct {
		Args []string `json:"args"`
	}
}

type OKOutput struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

type UIOutput struct {
	Body struct {
		State map[string]any `json:"state"`
	}
}

type UIInput struct {
	SessionPath
	Body struct {
		State map[string]any `json:"state"`
	}
}

type EventsInput struct {
	SessionPath
	After uint64 `query:"after" doc:"the last seq seen; 0 for the start"`
	Epoch string `query:"epoch" doc:"the epoch of that seq (last call's epoch); another epoch than the log's gets a snapshot"`
	Wait  int    `query:"wait" doc:"seconds to wait for the first new event (max 25)"`
}

type EventsOutput struct {
	Body struct {
		// Snapshot is set when the caller was too far behind to replay:
		// render it, then the events.
		Snapshot map[string]any   `json:"snapshot,omitempty"`
		Events   []map[string]any `json:"events"`
		Last     uint64           `json:"last" doc:"pass as after on the next call"`
		Epoch    string           `json:"epoch,omitempty" doc:"pass as epoch on the next call"`
	}
}

func ok() *OKOutput {
	o := &OKOutput{}
	o.Body.OK = true
	return o
}

// fail maps the service's errors onto HTTP: a session that is not the
// caller's does not exist (404), a bad request is 400.
func fail(err error) error {
	switch {
	case errors.Is(err, dund.ErrNoSession):
		return huma.Error404NotFound("no such session")
	case err != nil:
		return huma.Error400BadRequest(err.Error())
	}
	return nil
}

func (s *Server) register(api huma.API, g *gat.Gateway) {
	gat.Register(api, g, huma.Operation{OperationID: "hello", Method: http.MethodGet, Path: "/api/v1/hello",
		Summary: "Service version and protocol version."},
		func(ctx context.Context, _ *struct{}) (*HelloOutput, error) {
			o := &HelloOutput{}
			o.Body.Version, o.Body.Protocol = Version, proto.Version
			o.Body.Android = s.latestRelease()
			o.Body.SignIn = s.sso != nil
			return o, nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "listSessions", Method: http.MethodGet, Path: "/api/v1/sessions",
		Summary: "The caller's sessions, every workspace."},
		func(ctx context.Context, _ *struct{}) (*SessionsOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			o := &SessionsOutput{}
			o.Body.Items = s.svc.List(p)
			return o, nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "createSession", Method: http.MethodPost, Path: "/api/v1/sessions",
		Summary: "Start a session in a workspace, or adopt an existing conversation (resume)."},
		func(ctx context.Context, in *CreateInput) (*IDOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			id, err := s.svc.Create(p, in.Body.Workspace, in.Body.Args, in.Body.Resume)
			if err != nil {
				return nil, fail(err)
			}
			o := &IDOutput{}
			o.Body.ID = id
			return o, nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "sendMessage", Method: http.MethodPost, Path: "/api/v1/sessions/{id}/messages",
		Summary: "Send the session a message, as the user."},
		func(ctx context.Context, in *MessageInput) (*OKOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			return ok(), fail(s.svc.Send(p, in.ID, proto.NewCommand("", &proto.User{Content: in.Body.Content})))
		})
	gat.Register(api, g, huma.Operation{OperationID: "answer", Method: http.MethodPost, Path: "/api/v1/sessions/{id}/answer",
		Summary: "Answer the session's open question."},
		func(ctx context.Context, in *AnswerInput) (*OKOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			return ok(), fail(s.svc.Send(p, in.ID, proto.NewCommand("", &proto.Answer{Value: in.Body.Value})))
		})
	gat.Register(api, g, huma.Operation{OperationID: "control", Method: http.MethodPost, Path: "/api/v1/sessions/{id}/control",
		Summary: "Run a control or server command in the session."},
		func(ctx context.Context, in *ControlInput) (*OKOutput, error) {
			var payload any = &proto.ControlCmd{ID: in.Body.Target, Action: in.Body.Action}
			if in.Body.Kind == "server" {
				payload = &proto.ServerCmd{ID: in.Body.Target, Action: in.Body.Action}
			}
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			return ok(), fail(s.svc.Send(p, in.ID, proto.NewCommand("", payload)))
		})
	gat.Register(api, g, huma.Operation{OperationID: "restartSession", Method: http.MethodPost, Path: "/api/v1/sessions/{id}/restart",
		Summary: "Restart the session's engine with new flags (a model switch)."},
		func(ctx context.Context, in *RestartInput) (*OKOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			return ok(), fail(s.svc.Restart(p, in.ID, in.Body.Args))
		})
	gat.Register(api, g, huma.Operation{OperationID: "switchModel", Method: http.MethodPost, Path: "/api/v1/sessions/{id}/model",
		Summary: "Restart the session's engine on another model (its other flags kept)."},
		func(ctx context.Context, in *SwitchModelInput) (*OKOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			return ok(), fail(s.svc.SwitchModel(p, in.ID, in.Body.URL, in.Body.Model))
		})
	gat.Register(api, g, huma.Operation{OperationID: "signOut", Method: http.MethodPost, Path: "/api/v1/sign-out",
		Summary: "End this sign-in token (the local token is not ended this way)."},
		func(ctx context.Context, _ *struct{}) (*OKOutput, error) {
			if _, err := who(ctx); err != nil {
				return nil, err
			}
			tok, _ := ctx.Value(bearerKey{}).(string)
			if strings.HasPrefix(tok, "dun_s_") {
				return ok(), fail(s.svc.RevokeSignIn(tok))
			}
			return ok(), nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "stopSession", Method: http.MethodPost, Path: "/api/v1/sessions/{id}/stop",
		Summary: "Stop the session's engine; the next use starts it again."},
		func(ctx context.Context, in *SessionPath) (*OKOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			return ok(), fail(s.svc.Stop(p, in.ID))
		})
	gat.Register(api, g, huma.Operation{OperationID: "upgradeNow", Method: http.MethodPost, Path: "/api/v1/upgrade/now",
		Summary: "Restart into the pending upgrade without waiting: running turns are interrupted and resumed after the restart."},
		func(ctx context.Context, _ *struct{}) (*OKOutput, error) {
			if _, err := who(ctx); err != nil {
				return nil, err
			}
			if err := s.svc.UpgradeNow(); err != nil {
				return nil, huma.Error409Conflict(err.Error())
			}
			return ok(), nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "forgetSession", Method: http.MethodDelete, Path: "/api/v1/sessions/{id}",
		Summary: "Remove the session from the service (the transcript stays)."},
		func(ctx context.Context, in *SessionPath) (*OKOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			return ok(), fail(s.svc.Forget(p, in.ID))
		})
	gat.Register(api, g, huma.Operation{OperationID: "getUI", Method: http.MethodGet, Path: "/api/v1/sessions/{id}/ui",
		Summary: "The caller's UI state in the session."},
		func(ctx context.Context, in *SessionPath) (*UIOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			st, err := s.svc.UI(p, in.ID)
			if err != nil {
				return nil, fail(err)
			}
			o := &UIOutput{}
			o.Body.State = st
			return o, nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "putUI", Method: http.MethodPut, Path: "/api/v1/sessions/{id}/ui",
		Summary: "Replace the caller's UI state in the session."},
		func(ctx context.Context, in *UIInput) (*OKOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			return ok(), fail(s.svc.PutUI(p, in.ID, in.Body.State))
		})
	gat.Register(api, g, huma.Operation{OperationID: "pollSessionEvents", Method: http.MethodGet, Path: "/api/v1/sessions/{id}/events",
		Summary: "The session's events after a seq; waits for the first one."},
		func(ctx context.Context, in *EventsInput) (*EventsOutput, error) {
			wait := time.Duration(min(max(in.Wait, 0), int(pollWaitMax/time.Second))) * time.Second
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			snap, evs, err := s.svc.Poll(p, in.ID, in.After, in.Epoch, wait, pollMax)
			if err != nil {
				return nil, fail(err)
			}
			o := &EventsOutput{}
			o.Body.Last, o.Body.Epoch = in.After, in.Epoch
			if snap != nil {
				o.Body.Snapshot = flat(*snap)
				o.Body.Last, o.Body.Epoch = snap.Payload.(*proto.Snapshot).After, snap.Epoch
			}
			o.Body.Events = make([]map[string]any, 0, len(evs))
			for _, ev := range evs {
				o.Body.Events = append(o.Body.Events, flat(ev))
				o.Body.Last, o.Body.Epoch = ev.Seq, ev.Epoch
			}
			return o, nil
		})
}

// flat is an event as the JSON object it is on every other transport.
func flat(ev proto.Event) map[string]any {
	b, err := ev.MarshalJSON()
	if err != nil {
		return map[string]any{"type": ev.Type, "error": err.Error()}
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// Owner is the local principal for the uid the service runs as.
func Owner(uid int) string { return "unix:" + strconv.Itoa(uid) }
