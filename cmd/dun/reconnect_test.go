package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/proto"
)

// A dund restart, end to end over the socket: the real service and server,
// the TUI's attach, and in-process engines.

// echoSpawner starts engines that name their session (the --resume id, or a
// new one), say ready, answer each user message, and on stop say exit.
type echoSpawner struct{ n int }

func (s *echoSpawner) Spawn(_ string, args []string) (dund.Engine, error) {
	s.n++
	id := fmt.Sprintf("20261022-10000%d", s.n)
	if i := slices.Index(args, "--resume"); i >= 0 {
		id = args[i+1]
	}
	e := &echoEngine{done: make(chan struct{})}
	e.inR, e.inW = io.Pipe()
	e.outR, e.outW = io.Pipe()
	go e.run(id)
	return e, nil
}

type echoEngine struct {
	inR  *io.PipeReader
	inW  *io.PipeWriter
	outR *io.PipeReader
	outW *io.PipeWriter
	done chan struct{}
	once sync.Once
}

func (e *echoEngine) run(id string) {
	defer e.Kill()
	enc := json.NewEncoder(e.outW)
	emit := func(p any) { _ = enc.Encode(proto.NewEvent(p)) }
	emit(&proto.Session{ID: id})
	emit(&proto.Ready{Tools: []string{"exec"}})
	sc := bufio.NewScanner(e.inR)
	for sc.Scan() {
		var c proto.Command
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		switch p := c.Payload.(type) {
		case *proto.User:
			emit(&proto.UserEcho{Content: p.Content})
			emit(&proto.Message{Role: "assistant", Content: "re: " + p.Content})
			emit(&proto.Done{})
		case *proto.Stop:
			emit(&proto.Exit{Reason: "stopped"})
			return
		}
	}
}

func (e *echoEngine) Stdin() io.Writer  { return e.inW }
func (e *echoEngine) Stdout() io.Reader { return e.outR }
func (e *echoEngine) Wait() error       { <-e.done; return nil }
func (e *echoEngine) Kill() {
	e.once.Do(func() { _ = e.outW.Close(); _ = e.inR.Close(); close(e.done) })
}

// connTracker remembers accepted connections, so a test can end them the way
// dund's exit (or exec) does.
type connTracker struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *connTracker) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *connTracker) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		_ = c.Close()
	}
}

// testDund is one dund process's lifetime on the test's DUN_HOME.
type testDund struct {
	svc *dund.Service
	ln  *connTracker
}

func startTestDund(t *testing.T) *testDund {
	t.Helper()
	svc, err := dund.New(filepath.Join(dunHome(), "dund"), &echoSpawner{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", dundcli.Socket(dunHome()))
	if err != nil {
		t.Fatal(err)
	}
	d := &testDund{svc: svc, ln: &connTracker{Listener: ln}}
	go func() { _ = dund.NewServer(svc, dund.UnixPeer).Serve(d.ln) }()
	t.Cleanup(d.stop)
	return d
}

// stop is what dund does before it exits or re-execs: stop listening, stop
// the engines, and (by exiting) end every client connection.
func (d *testDund) stop() {
	_ = d.ln.Close()
	_ = os.Remove(dundcli.Socket(dunHome()))
	d.svc.Close()
	d.ln.closeAll()
}

// waitFor reads p's events until one of type typ, returning the types seen.
func waitFor(t *testing.T, p *dunProc, typ string) []string {
	t.Helper()
	var seen []string
	timeout := time.After(3 * time.Second)
	for {
		select {
		case msg := <-p.ch:
			ev, ok := msg.(evMsg)
			if !ok && typ == "eof" {
				return seen
			}
			if !ok {
				t.Fatalf("stream ended waiting for %s (saw %v)", typ, seen)
			}
			seen = append(seen, str(ev["type"]))
			if ev["type"] == typ {
				return seen
			}
		case <-timeout:
			t.Fatalf("no %s event (saw %v)", typ, seen)
		}
	}
}

func testHome(t *testing.T) string {
	t.Helper()
	// A short home: the socket path must fit a sockaddr.
	home, err := os.MkdirTemp("", "dunrc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("DUN_HOME", home)
	return home
}

// After dund restarts, its log for the session is new and numbers events
// from 1 again. A TUI that resumes "after the last seq it saw" must not land
// in the middle of the NEW numbering — here another client has already run
// the session past that seq — and miss the engine's start (ready) and
// everything before it.
func TestReattachAfterDundRestartWithSeqsPastOurs(t *testing.T) {
	testHome(t)
	ws := t.TempDir()
	d1 := startTestDund(t)
	o := tuiOpts{workspace: ws}
	p, err := attachSession(o, resumeAt{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, p, "ready")
	for i := 0; i < 2; i++ {
		p.send(fmt.Sprintf("q%d", i), nil)
		waitFor(t, p, "done")
	}
	seen := p.seen()
	if seen.seq == 0 {
		t.Fatal("no seq recorded")
	}

	d1.stop()
	waitFor(t, p, "eof")
	startTestDund(t)

	// Another client (the phone) gets there first and runs the session past
	// the seq this TUI last saw.
	o2 := tuiOpts{workspace: ws, resume: p.session}
	other, err := attachSession(o2, resumeAt{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, other, "ready")
	for other.seen().seq <= seen.seq {
		other.send("from the phone", nil)
		waitFor(t, other, "done")
	}

	o.resume = p.session
	back, err := attachSession(o, seen)
	if err != nil {
		t.Fatal(err)
	}
	// A snapshot first (the conversation and the latest state, ready among
	// it), then the live stream.
	got := waitFor(t, back, "snapshot")
	if len(got) != 1 {
		t.Fatalf("events before the snapshot: %v", got)
	}
	back.send("still here", nil)
	waitFor(t, back, "done")
}
