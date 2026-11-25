package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// fakeDund answers create and subscribe as dund does, hands every other
// command to the test, and sends the events the test emits to every
// subscriber.
type fakeDund struct {
	t       *testing.T
	mu      sync.Mutex
	subs    []net.Conn
	seq     uint64
	cmds    chan proto.Command
	creates chan proto.Create
	items   []proto.HistoryItem // the snapshot's history
	state   []any               // the snapshot's state events
	missing bool                // subscribe answers "no such session" until a create
	models  *proto.Models       // control "models" answers with it (none: an empty list)
}

// The tests that do not set the config's events wait for them briefly.
func init() { configWait = 50 * time.Millisecond }

func newFakeDund(t *testing.T) *fakeDund {
	return &fakeDund{t: t, cmds: make(chan proto.Command, 64), creates: make(chan proto.Create, 8)}
}

func (f *fakeDund) dial() (net.Conn, error) {
	c, s := net.Pipe()
	go f.serve(s)
	return c, nil
}

func (f *fakeDund) write(c net.Conn, ev proto.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		f.t.Errorf("marshal %s: %v", ev.Type, err)
		return
	}
	_, _ = c.Write(append(b, '\n'))
}

func (f *fakeDund) serve(c net.Conn) {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var cmd proto.Command
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			f.t.Errorf("command: %v", err)
			continue
		}
		ack := proto.NewEvent(&proto.Ack{})
		ack.Re, ack.Session = cmd.CID, "s1"
		switch p := cmd.Payload.(type) {
		case *proto.Create:
			f.creates <- *p
			f.mu.Lock()
			f.missing = false
			f.write(c, ack)
			f.mu.Unlock()
		case *proto.Subscribe:
			f.mu.Lock()
			if f.missing {
				nack := proto.NewEvent(&proto.Ack{Error: "no such session"})
				nack.Re = cmd.CID
				f.write(c, nack)
				f.mu.Unlock()
				continue
			}
			f.write(c, ack)
			sn := &proto.Snapshot{After: f.seq, Items: f.items}
			for _, st := range f.state {
				sn.State = append(sn.State, proto.NewEvent(st))
			}
			ev := proto.NewEvent(sn)
			ev.Session = "s1"
			f.write(c, ev)
			f.subs = append(f.subs, c)
			f.mu.Unlock()
		case *proto.ControlCmd:
			if p.ID != "models" {
				f.cmds <- cmd
				continue
			}
			m := f.models
			if m == nil {
				m = &proto.Models{}
			}
			go f.emit(m)
		default:
			f.cmds <- cmd
		}
	}
}

// emit sends events to the subscribers, numbered.
func (f *fakeDund) emit(payloads ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range payloads {
		f.seq++
		ev := proto.NewEvent(p)
		ev.Session, ev.Seq = "s1", f.seq
		for _, c := range f.subs {
			f.write(c, ev)
		}
	}
}

// next is the next command the bridge sent, of any type but detach.
func (f *fakeDund) next() proto.Command {
	f.t.Helper()
	for {
		select {
		case c := <-f.cmds:
			if c.Type == "detach" {
				continue
			}
			return c
		case <-time.After(5 * time.Second):
			f.t.Fatal("no command from the bridge")
		}
	}
}

// fakeEditor is the ACP client: it keeps the session updates and answers
// permission requests with perm.
type fakeEditor struct {
	mu      sync.Mutex
	updates []sdk.SessionUpdate
	perm    func(sdk.RequestPermissionRequest) sdk.RequestPermissionResponse
	asked   chan sdk.RequestPermissionRequest
}

func (e *fakeEditor) SessionUpdate(ctx context.Context, n sdk.SessionNotification) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.updates = append(e.updates, n.Update)
	return nil
}

func (e *fakeEditor) RequestPermission(ctx context.Context, r sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	if e.asked != nil {
		e.asked <- r
	}
	if e.perm == nil {
		<-ctx.Done()
		return sdk.RequestPermissionResponse{}, ctx.Err()
	}
	return e.perm(r), nil
}

// take returns the updates so far and forgets them.
func (e *fakeEditor) take() []sdk.SessionUpdate {
	e.mu.Lock()
	defer e.mu.Unlock()
	u := e.updates
	e.updates = nil
	return u
}

func (e *fakeEditor) CompleteElicitation(context.Context, sdk.CompleteElicitationNotification) error {
	return nil
}
func (e *fakeEditor) CreateElicitation(context.Context, sdk.CreateElicitationRequest) (sdk.CreateElicitationResponse, error) {
	return sdk.CreateElicitationResponse{}, sdk.NewMethodNotFound("elicitation/create")
}
func (e *fakeEditor) ReadTextFile(context.Context, sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, sdk.NewMethodNotFound("fs/read_text_file")
}
func (e *fakeEditor) WriteTextFile(context.Context, sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, sdk.NewMethodNotFound("fs/write_text_file")
}
func (e *fakeEditor) CreateTerminal(context.Context, sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, sdk.NewMethodNotFound("terminal/create")
}
func (e *fakeEditor) KillTerminal(context.Context, sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, sdk.NewMethodNotFound("terminal/kill")
}
func (e *fakeEditor) TerminalOutput(context.Context, sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, sdk.NewMethodNotFound("terminal/output")
}
func (e *fakeEditor) ReleaseTerminal(context.Context, sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, sdk.NewMethodNotFound("terminal/release")
}
func (e *fakeEditor) WaitForTerminalExit(context.Context, sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, sdk.NewMethodNotFound("terminal/wait_for_exit")
}

// rig is a bridge between a fake editor and a fake dund, initialized.
type rig struct {
	t   *testing.T
	d   *fakeDund
	ed  *fakeEditor
	cc  *sdk.ClientSideConnection
	cwd string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return newRigCaps(t, sdk.ClientCapabilities{})
}

// newRigCaps is a rig whose editor advertises caps at initialize.
func newRigCaps(t *testing.T, caps sdk.ClientCapabilities) *rig {
	t.Helper()
	r := &rig{t: t, d: newFakeDund(t), ed: &fakeEditor{}, cwd: t.TempDir()}
	agentIn, editorOut := io.Pipe()
	editorIn, agentOut := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Serve(agentIn, agentOut, r.d.dial, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	}()
	r.cc = sdk.NewClientSideConnection(r.ed, editorOut, editorIn)
	r.cc.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		editorOut.Close()
		agentOut.Close()
		<-done
	})
	if _, err := r.cc.Initialize(context.Background(), sdk.InitializeRequest{ProtocolVersion: sdk.ProtocolVersionNumber, ClientCapabilities: caps}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) newSession() sdk.SessionId {
	r.t.Helper()
	res, err := r.cc.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: r.cwd, McpServers: []sdk.McpServer{}})
	if err != nil {
		r.t.Fatal(err)
	}
	return res.SessionId
}

// prompt sends a prompt in the background; its answer comes on the channel.
func (r *rig) prompt(sid sdk.SessionId, text string) chan promptResult {
	ch := make(chan promptResult, 1)
	go func() {
		res, err := r.cc.Prompt(context.Background(), sdk.PromptRequest{SessionId: sid, Prompt: []sdk.ContentBlock{sdk.TextBlock(text)}})
		ch <- promptResult{res.StopReason, err}
	}()
	return ch
}

type promptResult struct {
	stop sdk.StopReason
	err  error
}

func waitPrompt(t *testing.T, ch chan promptResult) promptResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt was not answered")
	}
	return promptResult{}
}

// pending fails when the prompt is answered within a short wait.
func pending(t *testing.T, ch chan promptResult, what string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s: answered early: %+v", what, r)
	case <-time.After(100 * time.Millisecond):
	}
}
