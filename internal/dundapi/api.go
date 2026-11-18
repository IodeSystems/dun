// Package dundapi is dund's HTTP surface: huma operations at /api/v1, the
// same ones as GraphQL at /api/graphql through gat (API-7), and /healthz.
//
// Every operation but hello needs a bearer token or a browser's session
// cookie (front.go), checked by HTTP middleware in front of both /api/v1 and
// /api/graphql: gat calls the operations in-process, so huma's
// per-operation middlewares never run for GraphQL. The pages (/, /web, /tui)
// need the session cookie too; see openPath for what does not.
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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
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
	// term is /tui (Options.Term); tuiEnabled says whether it is on now.
	term       http.Handler
	tuiEnabled func() bool
}

// Options are what the HTTP surface serves besides the API.
type Options struct {
	// Owner is the principal the local token stands for (unix:<uid>).
	Owner string
	// WebRoot, when set, is the built client (mobile: expo export --platform
	// web, baseUrl /web) served at /web from the filesystem (WEB-5); without
	// it, a release build serves the copy it embeds and a source build a page
	// saying so.
	WebRoot string
	// OIDC, when its issuer and client id are set, turns on sign-in with
	// the identity provider (oidc.go).
	OIDC OIDC
	// Releases, when set, is where tools/release-android publishes:
	// android/latest.json and the APKs, served under /releases with /dun.apk
	// the latest, and announced by /api/v1/hello.
	Releases string
	// Term, when set, is the TUI in the browser (internal/webterm), served
	// at /tui while TUIOn says so (`dun dund enable tui`); else /tui is 404.
	Term  http.Handler
	TUIOn func() bool
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

// spa serves app's files, and its index.html for any other path: a client
// route (/session/…) is the app's shell, which routes itself. A missing
// file (under /_expo/ or /assets/, or with an extension) is a 404: the shell
// served as a script or image gets cached by the service worker.
func spa(app fs.FS) http.HandlerFunc {
	files := http.FileServerFS(app)
	return func(w http.ResponseWriter, req *http.Request) {
		p := strings.TrimPrefix(path.Clean("/"+req.URL.Path), "/")
		if st, err := fs.Stat(app, p); err == nil && !st.IsDir() {
			files.ServeHTTP(w, req)
			return
		}
		if strings.HasPrefix(p, "_expo/") || strings.HasPrefix(p, "assets/") || path.Ext(p) != "" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, req, app, "index.html")
	}
}

// noApp is / when this dund has no web app: a source build without
// --web-root. The two renderers are the TUI and the web app (USER
// 2026-10-28); this page renders nothing, it says where the app is.
const noApp = "This dund was built without the web app.\n\n" +
	"Release builds include it. From a checkout: make webapp, then dun dund serve --web-root mobile/dist " +
	"(with the web listener on: dun dund enable web).\nThe HTTP API is here as usual: /api/v1/hello.\n"

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
	s := &Server{svc: svc, owner: o.Owner, signer: newSigner(), releases: o.Releases, term: o.Term, tuiEnabled: o.TUIOn}
	if o.OIDC.on() {
		s.sso = newSSO(o.OIDC, svc, o.Owner)
		s.sso.session = func(w http.ResponseWriter, r *http.Request, principal, from string) error {
			return s.startSession(w, r, principal, "", from)
		}
	}
	webRoot := o.WebRoot
	if auth {
		r.Use(s.authenticate)
	}
	noAppPage := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(noApp))
	}
	if auth {
		r.Get("/", s.chooser)
		r.Get("/choose/*", s.choose)
		r.Get(signinPath, s.signinPage)
		r.Post(signinPath, s.signin)
		r.Post(signoutPath, s.signout)
		r.Get("/tui", slash("/tui/"))
		r.Handle("/tui/*", http.HandlerFunc(s.tui))
		r.Get("/web", slash("/web/"))
	}
	if s.sso != nil {
		r.Get(loginPath, s.sso.login)
		r.Get(callbackRP, s.sso.callback)
	}
	if o.Releases != "" {
		r.Handle(releasesPath+"/*", http.StripPrefix(releasesPath, releaseFiles(o.Releases)))
		r.Get(apkPath, s.latestAPK)
	}
	app, embedded := embeddedApp()
	switch {
	case webRoot != "": // --web-root wins: a client being worked on
		r.Handle("/web/*", http.StripPrefix("/web", spa(os.DirFS(webRoot))))
	case embedded: // a release build carries the app (webapp_release.go)
		r.Handle("/web/*", http.StripPrefix("/web", spa(app)))
	case auth: // a source build without one: a page that says so
		r.Get("/web/*", noAppPage)
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

// authenticate refuses a request with neither a live bearer token nor a
// live session cookie, except to the open paths (openPath) and the
// graphql-ws upgrade (gated by its HMAC, subscribe.go), and puts the
// caller's principal in the request context — which is the context every
// handler gets, whether called as REST or through gat. A page is sent to the
// sign-in page; the API and WebSockets get a 401. A request on the cookie
// that changes something must come from this site (SameSite=Strict says so
// already; the Origin check holds where a browser ignores it).
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The open-path check reads the cleaned path; a ".." segment is
		// refused outright, so "/auth/../tui/ws" cannot pass as /auth.
		if dotDot(r.URL.Path) || dotDot(r.URL.EscapedPath()) {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if openPath(path.Clean("/"+r.URL.Path)) || isUpgrade(r) {
			next.ServeHTTP(w, r)
			return
		}
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		tok = strings.TrimSpace(tok)
		who, ok := s.principalFor(tok)
		byCookie := false
		if !ok && tok == "" {
			if tok = cookieSession(r); tok != "" {
				who, ok = s.svc.CheckSignIn(tok)
				byCookie = true
			}
		}
		if !ok {
			api := strings.HasPrefix(r.URL.Path, "/api/")
			if !api && r.Method == http.MethodGet && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				if cookieSession(r) != "" {
					clearCookie(w, r, sessionCookie) // ended: its bearer was replaced or revoked
				}
				http.Redirect(w, r, signinURL(pathReturn(r.URL.RequestURI()), ""), http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"detail":"a valid bearer token or sign-in is required"}`))
			return
		}
		if byCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && !sameSiteForm(r) {
			http.Error(w, "a request from another site", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, who)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, bearerKey{}, tok)))
	})
}

type bearerKey struct{}

// principalFor is whom a bearer token stands for: the local token, a named
// API token and a sign-in token all stand for the owner.
func (s *Server) principalFor(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	if s.svc.CheckToken(tok) || s.svc.CheckAPIToken(tok) {
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

type WorkspacesOutput struct {
	Body struct {
		Items []dund.Workspace `json:"items"`
	}
}

type DirsInput struct {
	Path string `query:"path" doc:"the path typed so far: absolute, ~/…, or under the home; a trailing / lists that directory"`
}

type DirsOutput struct {
	Body dund.DirListing
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
		// AskID: the question answered (the ask event's ask_id); an answer
		// to one no longer open is dropped. Empty answers the open one.
		AskID string `json:"ask_id,omitempty"`
	}
}

type ControlInput struct {
	SessionPath
	Body struct {
		Kind   string `json:"kind" enum:"control,server" doc:"control: /isolate, /worktree, /mcp…; server: /rag, /lsp"`
		Target string `json:"target"`
		Action string `json:"action"`
	}
}

type AgentInput struct {
	SessionPath
	Body struct {
		Agent   string `json:"agent" doc:"the sub-agent's number"`
		Action  string `json:"action" enum:"view,leave,tell" doc:"view: its conversation (agent_history, then agent_entry events) until a leave; tell: send it content"`
		Content string `json:"content,omitempty"`
	}
}

type JobOutputInput struct {
	SessionPath
	Job   int   `path:"job"`
	Since int64 `query:"since" default:"-1" doc:"the byte offset to read from (the last read's next); -1: use tail"`
	Tail  int64 `query:"tail" doc:"with since -1, the last this many bytes; 0: from the start"`
	Limit int   `query:"limit" doc:"the most bytes to return (at most 262144)"`
}

type JobOutputOutput struct {
	Body dund.JobOutput
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
	gat.Register(api, g, huma.Operation{OperationID: "listWorkspaces", Method: http.MethodGet, Path: "/api/v1/workspaces",
		Summary: "Where the caller's sessions ran, most recent activity first: the new-session picker."},
		func(ctx context.Context, _ *struct{}) (*WorkspacesOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			o := &WorkspacesOutput{}
			// ~/.dun/sessions is the user dund runs as: only that principal
			// sees the workspaces recorded there.
			o.Body.Items = s.svc.Workspaces(p, p == s.owner)
			return o, nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "listDirs", Method: http.MethodGet, Path: "/api/v1/dirs",
		Summary: "Directories completing a typed workspace path, and how createSession would read it."},
		func(ctx context.Context, in *DirsInput) (*DirsOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			// The filesystem is the user dund runs as; another principal
			// does not browse it.
			if p != s.owner {
				return nil, huma.Error403Forbidden("only the user dund runs as can browse its directories")
			}
			l, err := dund.BrowseDirs(in.Path)
			if err != nil {
				return nil, huma.Error400BadRequest(err.Error())
			}
			return &DirsOutput{Body: l}, nil
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
			return ok(), fail(s.svc.Send(p, in.ID, proto.NewCommand("", &proto.Answer{Value: in.Body.Value, AskID: in.Body.AskID})))
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
	gat.Register(api, g, huma.Operation{OperationID: "agent", Method: http.MethodPost, Path: "/api/v1/sessions/{id}/agent",
		Summary: "View, leave or tell one of the session's sub-agents. A view streams its conversation as session events until its leave."},
		func(ctx context.Context, in *AgentInput) (*OKOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			cmd := &proto.AgentCmd{ID: in.Body.Agent, Action: in.Body.Action, Content: in.Body.Content}
			return ok(), fail(s.svc.Send(p, in.ID, proto.NewCommand("", cmd)))
		})
	gat.Register(api, g, huma.Operation{OperationID: "jobOutput", Method: http.MethodGet, Path: "/api/v1/sessions/{id}/jobs/{job}/output",
		Summary: "Part of a background job's output: from an offset, or its tail. Only the session's own jobs."},
		func(ctx context.Context, in *JobOutputInput) (*JobOutputOutput, error) {
			p, err := who(ctx)
			if err != nil {
				return nil, err
			}
			out, err := s.svc.JobOutput(p, in.ID, in.Job, in.Since, in.Tail, in.Limit)
			if err != nil {
				return nil, fail(err)
			}
			return &JobOutputOutput{Body: *out}, nil
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
