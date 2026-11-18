package dund

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// linkSpawner starts fake engines (fakeSpawner's) behind a fake engine
// link in dir, the way ExecSpawner starts dun --engine --listen: what a
// dund test needs to adopt engines across a Service's restart. The real
// engine half (cmd/dun/enginelink.go) has its own tests and the
// integration test (cmd/dun/adopt_integration_test.go).
type linkSpawner struct {
	*fakeSpawner
	dir     string
	build   string // EngineBuild
	version string // the engines' build, in their hellos
	proto   int    // the engines' proto version (0: this one)

	mu    sync.Mutex
	links map[int]*fakeLink
}

var fakePids atomic.Int64

func newLinkSpawner(t *testing.T, dir string) *linkSpawner {
	t.Helper()
	ls := &linkSpawner{fakeSpawner: &fakeSpawner{}, dir: dir, links: map[int]*fakeLink{}}
	old := adoptProc
	adoptProc = func(pid int) proc { return fakeProcOf(pid) }
	t.Cleanup(func() { adoptProc = old })
	return ls
}

// A second spawner on the same dir, for the next Service: the links stay.
func (ls *linkSpawner) next() *linkSpawner {
	return &linkSpawner{fakeSpawner: &fakeSpawner{}, dir: ls.dir, build: ls.build, version: ls.version, links: ls.links}
}

var (
	fakeProcsMu sync.Mutex
	fakeProcs   = map[int]*fakeLink{}
)

func fakeProcOf(pid int) proc {
	fakeProcsMu.Lock()
	defer fakeProcsMu.Unlock()
	if l := fakeProcs[pid]; l != nil {
		return l
	}
	return deadProc{}
}

type deadProc struct{}

func (deadProc) wait() error { return nil }
func (deadProc) kill()       {}

func (ls *linkSpawner) Spawn(workspace string, args []string) (Engine, error) {
	eng, err := ls.fakeSpawner.Spawn(workspace, args)
	if err != nil {
		return nil, err
	}
	pid := 900000 + int(fakePids.Add(1))
	l, err := startFakeLink(ls.dir, pid, ls.version, ls.proto, eng.(*fakeEngine))
	if err != nil {
		return nil, err
	}
	ls.mu.Lock()
	ls.links[pid] = l
	ls.mu.Unlock()
	c, sc, h, err := dialLink(l.sock, time.Second)
	if err != nil {
		return nil, err
	}
	return newLinkEngine(c, sc, h, l, proto.LinkResume{})
}

func (ls *linkSpawner) Adopt() []*Adoption  { return adoptEngines(ls.dir) }
func (ls *linkSpawner) EngineBuild() string { return ls.build }

// linkOf is the fake link of session id's engine.
func (ls *linkSpawner) linkOf(id string) *fakeLink {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for _, l := range ls.links {
		if l.sessionID() == id && !l.ended() {
			return l
		}
	}
	return nil
}

// fakeLink is the engine half of the link over a fakeEngine: every event
// numbered and kept, a hello, a resume, the replay, then live.
type fakeLink struct {
	dir, sock, info string
	pid             int
	version         string
	proto           int
	eng             *fakeEngine
	ln              net.Listener

	mu      sync.Mutex
	cond    *sync.Cond
	lines   [][]byte // seq i+1 at i
	first   uint64   // the oldest kept (a test drops some: lose)
	acked   uint64
	conn    net.Conn
	session string
	done    bool
	gone    chan struct{} // closed once the engine ended and the link closed
	state   map[string]uint64
}

func startFakeLink(dir string, pid int, version string, protoV int, eng *fakeEngine) (*fakeLink, error) {
	if protoV == 0 {
		protoV = proto.Version
	}
	n := strconv.Itoa(pid)
	l := &fakeLink{dir: dir, sock: filepath.Join(dir, n+".sock"), info: filepath.Join(dir, n+".json"), pid: pid,
		version: version, proto: protoV, eng: eng, first: 1, gone: make(chan struct{}), state: map[string]uint64{}}
	l.cond = sync.NewCond(&l.mu)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", l.sock)
	if err != nil {
		return nil, err
	}
	l.ln = ln
	l.writeInfo()
	fakeProcsMu.Lock()
	fakeProcs[pid] = l
	fakeProcsMu.Unlock()
	go l.accept()
	go l.pump()
	return l, nil
}

func (l *fakeLink) writeInfo() {
	l.mu.Lock()
	b, _ := json.Marshal(proto.EngineInfo{Pid: l.pid, Sock: l.sock, Session: l.session, Version: l.version,
		Proto: l.proto, Link: proto.LinkVersion})
	l.mu.Unlock()
	_ = os.WriteFile(l.info, b, 0o600)
}

// pump numbers the engine's events, as the emitter's writes are.
func (l *fakeLink) pump() {
	sc := newScanner(l.eng.stdoutR)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if ev, ok := decodeEvent(line); ok {
			if p, ok := ev.Payload.(*proto.Session); ok {
				l.mu.Lock()
				l.session = p.ID
				l.mu.Unlock()
				l.writeInfo()
			}
		}
		l.add(line)
	}
	// The engine ended: the last events go, then the link closes.
	l.mu.Lock()
	l.done = true
	l.cond.Broadcast()
	l.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	l.mu.Lock()
	c := l.conn
	l.mu.Unlock()
	l.ln.Close()
	if c != nil {
		c.Close()
	}
	os.Remove(l.sock)
	os.Remove(l.info)
	close(l.gone)
}

func (l *fakeLink) add(line []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
	if ev, ok := decodeEvent(line); ok && proto.IsStateType(ev.Type) {
		l.state[ev.Type] = uint64(len(l.lines))
	}
	l.cond.Broadcast()
}

// inject is an event of the engine's own, as a wake's turn would write.
func (l *fakeLink) inject(p any) {
	ev := proto.NewEvent(p)
	ev.TS = time.Now()
	b, _ := json.Marshal(ev)
	l.add(b)
}

// lose drops the events not acked, as a ring overflow with no dund does.
func (l *fakeLink) lose() {
	l.mu.Lock()
	l.first = uint64(len(l.lines)) + 1
	l.mu.Unlock()
}

func (l *fakeLink) sessionID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.session
}

func (l *fakeLink) ended() bool {
	select {
	case <-l.gone:
		return true
	default:
		return false
	}
}

func (l *fakeLink) attached() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil
}

func (l *fakeLink) ackedSeq() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acked
}

func (l *fakeLink) seq() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return uint64(len(l.lines))
}

// proc, for the linkEngine: the fake engine's life.
func (l *fakeLink) wait() error { <-l.gone; return nil }
func (l *fakeLink) kill()       { l.eng.end() }

func (l *fakeLink) accept() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		l.mu.Lock()
		busy := l.conn != nil
		l.mu.Unlock()
		if busy {
			_, _ = c.Write([]byte(proto.LinkBusyLine + "\n"))
			c.Close()
			continue
		}
		l.serve(c)
	}
}

func (l *fakeLink) serve(c net.Conn) {
	l.mu.Lock()
	seq := uint64(len(l.lines))
	first := max(l.first, l.acked+1)
	h := proto.LinkHello{Type: proto.LinkHelloType, Link: proto.LinkVersion, Proto: l.proto, Version: l.version,
		Pid: l.pid, Session: l.session, Seq: seq, Acked: l.acked, First: first}
	l.mu.Unlock()
	b, _ := json.Marshal(h)
	if _, err := c.Write(append(b, '\n')); err != nil {
		c.Close()
		return
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	if !sc.Scan() {
		c.Close()
		return
	}
	var res proto.LinkResume
	_ = json.Unmarshal(sc.Bytes(), &res)
	l.mu.Lock()
	var out []byte
	after := min(res.After, seq)
	if after+1 < first {
		g, _ := json.Marshal(proto.LinkGap{Type: proto.LinkGapType, After: after, First: first})
		out = append(append(out, g...), '\n')
	}
	if res.State {
		for _, s := range l.state {
			if s <= after {
				out = append(append(append(out, "0 "...), l.lines[s-1]...), '\n')
			}
		}
	}
	from := max(after+1, first)
	for s := from; s <= seq; s++ {
		out = append(append(append(strconv.AppendUint(out, s, 10), ' '), l.lines[s-1]...), '\n')
	}
	out = append(out, proto.LinkLiveLine+"\n"...)
	l.conn = c
	l.mu.Unlock()
	if _, err := c.Write(out); err != nil {
		l.release(c)
		return
	}
	go l.live(c, seq)
	go func() {
		defer l.release(c)
		for sc.Scan() {
			line := sc.Bytes()
			var a proto.LinkAck
			if json.Unmarshal(line, &a) == nil && a.Type == proto.LinkAckType {
				l.mu.Lock()
				l.acked = max(l.acked, a.Seq)
				l.mu.Unlock()
				continue
			}
			if _, err := l.eng.stdin.Write(append(append([]byte(nil), line...), '\n')); err != nil {
				return
			}
		}
	}()
}

func (l *fakeLink) live(c net.Conn, sent uint64) {
	for {
		l.mu.Lock()
		for l.conn == c && sent >= uint64(len(l.lines)) && !l.done {
			l.cond.Wait()
		}
		if l.conn != c || (l.done && sent >= uint64(len(l.lines))) {
			l.mu.Unlock()
			return
		}
		var out []byte
		for s := sent + 1; s <= uint64(len(l.lines)); s++ {
			out = append(append(append(strconv.AppendUint(out, s, 10), ' '), l.lines[s-1]...), '\n')
		}
		sent = uint64(len(l.lines))
		l.mu.Unlock()
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

func (l *fakeLink) release(c net.Conn) {
	c.Close()
	l.mu.Lock()
	if l.conn == c {
		l.conn = nil
	}
	l.cond.Broadcast()
	l.mu.Unlock()
}

// die is the engine's process gone without its clean-up: the socket stops
// answering, the files stay.
func (l *fakeLink) die() {
	l.ln.(*net.UnixListener).SetUnlinkOnClose(false)
	l.ln.Close()
	l.mu.Lock()
	c := l.conn
	l.mu.Unlock()
	if c != nil {
		c.Close()
	}
}
