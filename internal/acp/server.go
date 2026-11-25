// Package acp is dun as an Agent Client Protocol agent (docs/acp.md): an
// editor (Zed, JetBrains, Neovim) starts `dun acp` and speaks ACP's JSON-RPC
// on its stdio; the bridge is a client of dund over its unix socket, as the
// TUI is. dund owns the sessions; the bridge translates, and keeps only
// what a translation needs.
package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// Agent answers one editor. Its sessions are dund sessions.
type Agent struct {
	dial    func() (net.Conn, error)
	log     *slog.Logger
	version string
	// args are the engine flags a session/new creates with.
	args []string

	ctx     context.Context
	peer    peer
	started chan struct{} // closed once peer is set

	mu          sync.Mutex
	sessions    map[string]*session
	initialized bool
	caps        caps // the editor's, from initialize
}

// sessionFileExists says whether dun has a session file for id in the
// workspace cwd; a variable so tests need no dun home.
var sessionFileExists = func(cwd, id string) bool {
	_, err := os.Stat(harness.SessionFile(cwd, id))
	return err == nil
}

// notFound is ACP's resource-not-found (-32002) for a session id.
func notFound(id string) error {
	return sdk.NewRequestError(-32002, "Resource not found", map[string]any{"error": "no such session", "sessionId": id})
}

// ready fails a request that came before initialize (-32600).
func (a *Agent) ready() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.initialized {
		return sdk.NewInvalidRequest(map[string]any{"error": "initialize first"})
	}
	return nil
}

// The ACP methods the bridge implements; the build breaks when the SDK
// changes one.
var (
	_ sdk.Agent       = (*Agent)(nil)
	_ sdk.AgentLoader = (*Agent)(nil)
)

// engineArgs are the flags of an ACP session's engine. No --worktree: the
// editor shows the files on disk, so dun edits them in place (dun's
// default). No suggestions: no ACP client shows them, and each is a side
// call.
var engineArgs = []string{"--no-suggest"}

// Serve runs the agent on the editor's stdio until the editor closes it.
// dial reaches dund (tui.DialDund starts it when nothing listens).
func Serve(in io.Reader, out io.Writer, dial func() (net.Conn, error), log *slog.Logger, version string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &Agent{dial: dial, log: log, version: version, args: engineArgs, sessions: map[string]*session{},
		ctx: ctx, started: make(chan struct{})}
	conn := sdk.NewAgentSideConnection(a, out, in)
	conn.SetLogger(log)
	a.peer = conn
	close(a.started)
	<-conn.Done()
	cancel()
	a.closeAll()
	return nil
}

func (a *Agent) closeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, s := range a.sessions {
		s.close()
		delete(a.sessions, id)
	}
}

// dun has no per-editor sign-in. One agent auth method exists because
// editors (JetBrains Air) wait for one and the ACP registry requires one;
// authenticate checks nothing, and sessions do not need it.
const authMethodID = "dun"

var authDescription = "Uses the providers configured in dun. Run the dun TUI and /login to change them."

func (a *Agent) Initialize(ctx context.Context, p sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	a.mu.Lock()
	a.initialized = true
	a.caps = readCaps(p.ClientCapabilities)
	a.mu.Unlock()
	return sdk.InitializeResponse{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		AgentInfo:       &sdk.Implementation{Name: "dun", Version: a.version},
		AgentCapabilities: sdk.AgentCapabilities{
			LoadSession:         true,
			SessionCapabilities: sdk.SessionCapabilities{Close: &sdk.SessionCloseCapabilities{}},
			PromptCapabilities:  sdk.PromptCapabilities{EmbeddedContext: true},
		},
		AuthMethods: []sdk.AuthMethod{{Agent: &sdk.AuthMethodAgent{
			Id:          authMethodID,
			Name:        "dun",
			Description: &authDescription,
		}}},
	}, nil
}

func (a *Agent) NewSession(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	if err := a.ready(); err != nil {
		return sdk.NewSessionResponse{}, err
	}
	s, err := a.open(ctx, p.Cwd, "", len(p.McpServers))
	if err != nil {
		return sdk.NewSessionResponse{}, err
	}
	return sdk.NewSessionResponse{SessionId: sdk.SessionId(s.id), ConfigOptions: s.liveConfig()}, nil
}

// LoadSession attaches to a dun session (a dund id, as session/new gave)
// and streams its conversation before it answers.
func (a *Agent) LoadSession(ctx context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
	if err := a.ready(); err != nil {
		return sdk.LoadSessionResponse{}, err
	}
	if p.SessionId == "" {
		return sdk.LoadSessionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "sessionId is required"})
	}
	s, err := a.open(ctx, p.Cwd, string(p.SessionId), len(p.McpServers))
	if err != nil {
		return sdk.LoadSessionResponse{}, err
	}
	return sdk.LoadSessionResponse{ConfigOptions: s.liveConfig()}, nil
}

// readyWait bounds the wait for a session's first snapshot.
const readyWait = time.Minute

// open creates (resume "") or attaches to a dund session in cwd and follows
// it; it returns once the session's state is in (and, for a load, its
// history streamed).
func (a *Agent) open(ctx context.Context, cwd, resume string, mcpServers int) (*session, error) {
	if !filepath.IsAbs(cwd) {
		return nil, sdk.NewInvalidParams(map[string]any{"error": "cwd must be an absolute path", "cwd": cwd})
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return nil, sdk.NewInvalidParams(map[string]any{"error": "cwd is not a directory", "cwd": cwd})
	}
	<-a.started
	if mcpServers > 0 {
		// dun runs its own tool servers (dun.json); the editor's are not
		// passed to the engine (docs/acp.md).
		a.log.Info("acp: ignoring the editor's MCP servers", "count", mcpServers)
	}
	c, err := a.dial()
	if err != nil {
		return nil, fmt.Errorf("dund: %w", err)
	}
	l := newLink(c)
	id := resume
	if id == "" {
		ev, err := l.call("", &proto.Create{Workspace: cwd, Args: a.args})
		if err != nil {
			l.close()
			return nil, fmt.Errorf("start a dun session in %s: %w", cwd, err)
		}
		id = ev.Session
	}
	// Past the end: dund answers with a snapshot, the session as it is.
	sub := &proto.Subscribe{After: math.MaxUint64}
	if _, err := l.call(id, sub); err != nil {
		if resume == "" || !strings.Contains(err.Error(), "no such session") {
			l.close()
			return nil, err
		}
		// A conversation dund does not have (stopped, or from before it):
		// adopt it from the workspace's session file; with none, there is
		// no such session (the engine would start a new one under the id).
		if !sessionFileExists(cwd, id) {
			l.close()
			return nil, notFound(id)
		}
		if _, err := l.call("", &proto.Create{Workspace: cwd, Args: a.args, Resume: id}); err != nil {
			l.close()
			return nil, fmt.Errorf("load dun session %s in %s: %w", id, cwd, err)
		}
		if _, err := l.call(id, sub); err != nil {
			l.close()
			return nil, err
		}
	}
	s := newSession(a.ctx, id, cwd, a.peer, a.log, a.dial, resume != "")
	s.link = l
	a.mu.Lock()
	s.caps = a.caps
	if old := a.sessions[id]; old != nil {
		old.close()
	}
	a.sessions[id] = s
	a.mu.Unlock()
	go s.run(l)
	select {
	case <-s.ready:
		// The model list, for the model option: the engine asks each saved
		// provider (control "models", as the web app's picker).
		_ = s.send("", &proto.ControlCmd{ID: "models"})
		s.awaitConfig(ctx)
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(readyWait):
		return nil, errors.New("dund sent no snapshot of session " + id)
	}
}

func (a *Agent) session(id sdk.SessionId) (*session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[string(id)]
	if s == nil {
		return nil, notFound(string(id))
	}
	return s, nil
}

// Prompt sends the prompt as a dun message and answers when the turn that
// read it ends (turns.go). Its ctx is not used: the SDK cancels it when a
// newer prompt of the session arrives, and dun queues that one instead.
// session/cancel (Cancel) is what stops a turn.
func (a *Agent) Prompt(_ context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	s, err := a.session(p.SessionId)
	if err != nil {
		return sdk.PromptResponse{}, err
	}
	text := promptText(p.Prompt)
	if text == "" {
		return sdk.PromptResponse{}, sdk.NewInvalidParams(map[string]any{"error": "the prompt has no text"})
	}
	s.resendPlan()
	pr, err := s.submit(text)
	if err != nil {
		return sdk.PromptResponse{}, err
	}
	return s.wait(a.ctx, pr)
}

func (a *Agent) Cancel(ctx context.Context, p sdk.CancelNotification) error {
	s, err := a.session(p.SessionId)
	if err != nil {
		return err
	}
	s.stop()
	return nil
}

// CloseSession cancels the session's work as session/cancel does (its
// prompts end "cancelled"), then ends the bridge's link; dund keeps the
// session.
func (a *Agent) CloseSession(ctx context.Context, p sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	a.mu.Lock()
	s := a.sessions[string(p.SessionId)]
	delete(a.sessions, string(p.SessionId))
	a.mu.Unlock()
	if s != nil {
		s.stop()
		s.finish(outcome{stop: sdk.StopReasonCancelled}, func(*prompt) bool { return true })
		s.close()
	}
	return sdk.CloseSessionResponse{}, nil
}

// Authenticate accepts the one method, "dun", with no check: model access
// is dun's and dund's own configuration.
func (a *Agent) Authenticate(ctx context.Context, p sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	if p.MethodId != authMethodID {
		return sdk.AuthenticateResponse{}, sdk.NewInvalidParams(map[string]any{"error": "unknown auth method", "methodId": string(p.MethodId)})
	}
	return sdk.AuthenticateResponse{}, nil
}

// Not offered (docs/acp.md, "Not supported").

func (a *Agent) Logout(ctx context.Context, p sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodLogout)
}

func (a *Agent) DeleteSession(ctx context.Context, p sdk.DeleteSessionRequest) (sdk.DeleteSessionResponse, error) {
	return sdk.DeleteSessionResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionDelete)
}

func (a *Agent) ListSessions(ctx context.Context, p sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionList)
}

func (a *Agent) ResumeSession(ctx context.Context, p sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	return sdk.ResumeSessionResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionResume)
}

// SetSessionConfigOption sets the model or the thinking level (config.go)
// and answers with every option as the engine then reports it.
func (a *Agent) SetSessionConfigOption(ctx context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	if p.ValueId == nil {
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "dun's options are selects: value is a value id"})
	}
	s, err := a.session(p.ValueId.SessionId)
	if err != nil {
		return sdk.SetSessionConfigOptionResponse{}, err
	}
	opts, err := s.setConfig(ctx, string(p.ValueId.ConfigId), string(p.ValueId.Value))
	if err != nil {
		return sdk.SetSessionConfigOptionResponse{}, err
	}
	return sdk.SetSessionConfigOptionResponse{ConfigOptions: opts}, nil
}

func (a *Agent) SetSessionMode(ctx context.Context, p sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionSetMode)
}

func (a *Agent) UnstableAuthStatus(ctx context.Context, p sdk.UnstableAuthStatusRequest) (sdk.UnstableAuthStatusResponse, error) {
	return sdk.UnstableAuthStatusResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodAuthStatus)
}
