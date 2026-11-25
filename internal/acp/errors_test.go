package acp

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// session/close with a prompt in flight interrupts the turn and ends the
// prompt "cancelled".
func TestCloseCancelsPrompt(t *testing.T) {
	r := newRig(t)
	sid := r.newSession()
	ch := r.prompt(sid, "long job")
	r.d.next()
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "long job"}, &proto.Turn{Kind: "user"}, &proto.Token{Text: "working"})
	if _, err := r.cc.CloseSession(context.Background(), sdk.CloseSessionRequest{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	if c := r.d.next(); c.Type != "interrupt" {
		t.Fatalf("want an interrupt, got %s", c.Type)
	}
	if res := waitPrompt(t, ch); res.stop != sdk.StopReasonCancelled {
		t.Fatalf("prompt: %+v", res)
	}
}

func wantNotFound(t *testing.T, err error, what string) {
	t.Helper()
	var re *sdk.RequestError
	if !errors.As(err, &re) || re.Code != -32002 || !strings.Contains(string(re.Data), "nope") {
		t.Errorf("%s: want -32002 with the session id, got %v", what, err)
	}
}

// An unknown session id is resource-not-found (-32002) with the id, for a
// prompt and for a load that has no session file to adopt.
func TestUnknownSession(t *testing.T) {
	r := newRig(t)
	_, err := r.cc.Prompt(context.Background(), sdk.PromptRequest{SessionId: "nope", Prompt: []sdk.ContentBlock{sdk.TextBlock("hi")}})
	wantNotFound(t, err, "prompt")
	old := sessionFileExists
	sessionFileExists = func(cwd, id string) bool { return false }
	defer func() { sessionFileExists = old }()
	r.d.mu.Lock()
	r.d.missing = true
	r.d.mu.Unlock()
	_, err = r.cc.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: "nope", Cwd: r.cwd, McpServers: []sdk.McpServer{}})
	wantNotFound(t, err, "load")
	select {
	case c := <-r.d.creates:
		t.Errorf("a load of no session created one: %+v", c)
	default:
	}
}

// A session dund lacks but the workspace has a file for is adopted.
func TestLoadAdopts(t *testing.T) {
	r := newRig(t)
	old := sessionFileExists
	sessionFileExists = func(cwd, id string) bool { return true }
	defer func() { sessionFileExists = old }()
	r.d.mu.Lock()
	r.d.missing = true
	r.d.mu.Unlock()
	if _, err := r.cc.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: "s1", Cwd: r.cwd, McpServers: []sdk.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	if c := <-r.d.creates; c.Resume != "s1" {
		t.Errorf("adopt: %+v", c)
	}
}

// A request before initialize is an invalid request (-32600).
func TestBeforeInitialize(t *testing.T) {
	a := &Agent{}
	_, err := a.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: t.TempDir()})
	var re *sdk.RequestError
	if !errors.As(err, &re) || re.Code != -32600 {
		t.Errorf("want -32600, got %v", err)
	}
}

// initialize advertises one agent auth method; authenticate accepts it and
// rejects other ids; a session needs no authenticate.
func TestAuthenticate(t *testing.T) {
	r := newRig(t)
	res, err := r.cc.Initialize(context.Background(), sdk.InitializeRequest{ProtocolVersion: sdk.ProtocolVersionNumber})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AuthMethods) != 1 || res.AuthMethods[0].Agent == nil || res.AuthMethods[0].Agent.Id != "dun" || res.AuthMethods[0].Agent.Name != "dun" || res.AuthMethods[0].Agent.Description == nil {
		t.Fatalf("authMethods: %+v", res.AuthMethods)
	}
	r.newSession()
	if _, err := r.cc.Authenticate(context.Background(), sdk.AuthenticateRequest{MethodId: "dun"}); err != nil {
		t.Errorf("authenticate dun: %v", err)
	}
	_, err = r.cc.Authenticate(context.Background(), sdk.AuthenticateRequest{MethodId: "nope"})
	var re *sdk.RequestError
	if !errors.As(err, &re) || re.Code != -32602 {
		t.Errorf("unknown method: want -32602, got %v", err)
	}
}
