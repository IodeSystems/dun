package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/proto"
)

// The engine's half of the engine link (docs/engine-adoption.md). Started
// with --listen <dir>, the engine listens on <dir>/<pid>.sock instead of
// talking over its stdio, and dund dials it: the engine then outlives dund's
// restart, and the next dund dials it back. Events are numbered and kept in
// a ring until dund acks them, so the events of the gap are replayed, not
// lost.

// Variables so tests can shorten them.
var (
	// linkRingCap bounds the events kept for a dund (USER 2026-11-17: 16 MB).
	// Attached, a writer over it waits (as a full pipe did); with no dund,
	// the oldest go and the next resume is told (link_gap).
	linkRingCap = 16 << 20
	// linkOrphanWait: with no dund attached this long, the engine stops as
	// on a stop command. The next dund resumes the session from its record.
	linkOrphanWait = 2 * time.Minute
	// linkHandshake bounds a dialer's wait to send its resume.
	linkHandshake = 10 * time.Second
	// linkFlush bounds the wait at exit for the last events to reach dund.
	linkFlush = 5 * time.Second
	// linkForeignWait: a dund runs on this home and has not attached for
	// this long: it does not adopt engines (an older build, a downgrade) and
	// resumes the session on an engine of its own, so this one stops at
	// once rather than run beside it (docs/engine-adoption.md).
	linkForeignWait = 15 * time.Second
)

// maxLinkSock is the longest socket path the link listens on: under both
// Linux's 108-byte and macOS's 104-byte sun_path.
const maxLinkSock = 103

type linkFrame struct {
	seq  uint64
	typ  string
	line []byte // no newline
}

type engineLink struct {
	dir, sock, infoPath string
	pid                 int
	started             time.Time
	capBytes            int           // linkRingCap at the start
	orphanWait          time.Duration // linkOrphanWait at the start
	foreignWait         time.Duration // linkForeignWait at the start
	ln                  net.Listener
	// token is the secret in <pid>.json (0600): a dialer must name it in
	// its resume, so another process of this user cannot drive the engine
	// or hold its link.
	token string
	// dundUp reports whether a dund runs on this home (its pidfile names a
	// live process); nil: not known.
	dundUp func() bool

	// in is what the engine reads as its input (inputStream); every
	// connection's commands go into inW, and nothing closes it but stop.
	in  *io.PipeReader
	inW *io.PipeWriter

	mu      sync.Mutex
	cond    *sync.Cond
	ring    []linkFrame // the events dund has not acked, oldest first
	bytes   int
	seq     uint64 // the last event written
	acked   uint64
	sent    uint64               // the last event written to the attached dund
	state   map[string]linkFrame // the latest event of each state type
	partial []byte               // a line Write has not finished
	conn    net.Conn             // the attached dund (authenticated), nil when none
	alone   time.Time            // since when no dund is attached
	closing bool
	session string
	orphan  bool // the orphan stop was sent

	// onAttach runs after a dund attached (peers.go resends its open
	// requests); shared counts the gateway grants held (hello.shared).
	onAttach func()
	shared   func() int
}

// listenLink starts the link in dir: <pid>.json (with the token), then the
// socket beside it. dundUp: see engineLink.dundUp.
func listenLink(dir string, dundUp func() bool) (*engineLink, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("--listen: %w", err)
	}
	pid := os.Getpid()
	sock := filepath.Join(dir, strconv.Itoa(pid)+".sock")
	if len(sock) > maxLinkSock {
		return nil, fmt.Errorf("--listen: socket path is %d bytes, over %d: %s", len(sock), maxLinkSock, sock)
	}
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return nil, fmt.Errorf("--listen: %w", err)
	}
	pr, pw := io.Pipe()
	l := &engineLink{dir: dir, sock: sock, infoPath: filepath.Join(dir, strconv.Itoa(pid)+".json"), pid: pid,
		started: time.Now(), in: pr, inW: pw, state: map[string]linkFrame{}, alone: time.Now(),
		capBytes: linkRingCap, orphanWait: linkOrphanWait, foreignWait: linkForeignWait,
		token: hex.EncodeToString(tok), dundUp: dundUp}
	l.cond = sync.NewCond(&l.mu)
	// The entry first: a dund that sees the socket finds the token.
	if err := l.writeInfo(); err != nil {
		return nil, err
	}
	_ = os.Remove(sock) // a stale one of a process that had this pid
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.Remove(l.infoPath)
		return nil, fmt.Errorf("--listen: %w", err)
	}
	_ = os.Chmod(sock, 0o600)
	l.ln = ln
	go l.accept()
	go l.watchOrphan()
	return l, nil
}

func (l *engineLink) writeInfo() error {
	l.mu.Lock()
	info := proto.EngineInfo{Pid: l.pid, Sock: l.sock, Session: l.session, Version: buildinfo.Version,
		Proto: proto.Version, Link: proto.LinkVersion, Started: l.started.UTC().Format(time.RFC3339), Token: l.token}
	l.mu.Unlock()
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	tmp := l.infoPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	return os.Rename(tmp, l.infoPath)
}

// setSession names the session in the hello and in <pid>.json, which is
// how the next dund knows whose engine this is.
func (l *engineLink) setSession(id string) {
	l.mu.Lock()
	l.session = id
	l.mu.Unlock()
	if err := l.writeInfo(); err != nil {
		fmt.Fprintf(os.Stderr, "dun: %v\n", err)
	}
}

// connected reports whether a dund is attached.
func (l *engineLink) connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil
}

// Write takes the engine's event lines (the emitter's output): each is
// numbered and kept until dund acks it.
func (l *engineLink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	data := p
	if len(l.partial) > 0 {
		data = append(l.partial, p...)
		l.partial = nil
	}
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(data[:i])
		data = data[i+1:]
		if len(line) == 0 {
			continue
		}
		l.add(slices.Clone(line))
	}
	if len(data) > 0 {
		l.partial = slices.Clone(data)
	}
	return len(p), nil
}

// add numbers and keeps one line. Called with mu held.
func (l *engineLink) add(line []byte) {
	// Attached and over the cap: wait for dund (backpressure, as a pipe).
	for l.conn != nil && !l.closing && l.bytes+len(line) > l.capBytes && len(l.ring) > 0 {
		l.cond.Wait()
	}
	l.seq++
	f := linkFrame{seq: l.seq, typ: eventType(line), line: line}
	l.ring = append(l.ring, f)
	l.bytes += len(line)
	if proto.IsStateType(f.typ) {
		l.state[f.typ] = f
	}
	// With no dund, the oldest go: the next resume is told (link_gap).
	for l.conn == nil && l.bytes > l.capBytes && len(l.ring) > 1 {
		l.bytes -= len(l.ring[0].line)
		l.ring = l.ring[1:]
	}
	l.cond.Broadcast()
}

// eventType reads an event line's type without decoding it: an event's
// type comes before its payload (proto.Event), within its first bytes.
func eventType(line []byte) string {
	head := line[:min(len(line), 512)]
	const key = `"type":"`
	if i := bytes.Index(head, []byte(key)); i >= 0 {
		rest := head[i+len(key):]
		if j := bytes.IndexByte(rest, '"'); j >= 0 {
			return string(rest[:j])
		}
	}
	var ev struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(line, &ev)
	return ev.Type
}

// first is the oldest event still kept. Called with mu held.
func (l *engineLink) first() uint64 {
	if len(l.ring) > 0 {
		return l.ring[0].seq
	}
	return l.seq + 1
}

func (l *engineLink) accept() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		// Not claimed here: only a dialer that names the token claims the
		// link (serve), so a stray one cannot hold it.
		go l.serve(c)
	}
}

// serve is one dund's connection: the hello, its resume, the replay, then
// events out and commands in until it closes.
func (l *engineLink) serve(c net.Conn) {
	defer l.release(c)
	if l.isClosing() {
		return
	}
	l.mu.Lock()
	hello := proto.LinkHello{Type: proto.LinkHelloType, Link: proto.LinkVersion, Proto: proto.Version,
		Version: buildinfo.Version, Pid: l.pid, Session: l.session, Seq: l.seq, Acked: l.acked, First: l.first()}
	l.mu.Unlock()
	if l.shared != nil {
		hello.Shared = l.shared()
	}
	b, _ := json.Marshal(hello)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	_ = c.SetReadDeadline(time.Now().Add(linkHandshake))
	if !sc.Scan() {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	var res proto.LinkResume
	if json.Unmarshal(sc.Bytes(), &res) != nil || res.Type != proto.LinkResumeType ||
		subtle.ConstantTimeCompare([]byte(res.Token), []byte(l.token)) != 1 {
		fmt.Fprintln(os.Stderr, "dun: engine link: a dialer without the token was refused")
		return
	}
	// The replay, under the lock so no event slips between it and live.
	l.mu.Lock()
	if l.conn != nil || l.closing {
		l.mu.Unlock()
		_, _ = c.Write([]byte(proto.LinkBusyLine + "\n"))
		return
	}
	after := min(res.After, l.seq)
	var burst [][]byte
	if first := l.first(); after+1 < first {
		g, _ := json.Marshal(proto.LinkGap{Type: proto.LinkGapType, After: after, First: first})
		burst = append(burst, g)
	}
	if res.State {
		var st []linkFrame
		for _, f := range l.state {
			if f.seq <= after {
				st = append(st, f)
			}
		}
		slices.SortFunc(st, func(a, b linkFrame) int { return int(a.seq) - int(b.seq) })
		for _, f := range st {
			burst = append(burst, append([]byte("0 "), f.line...))
		}
	}
	for _, f := range l.ring {
		if f.seq > after {
			burst = append(burst, frameLine(f))
		}
	}
	burst = append([][]byte{[]byte(proto.LinkOkLine)}, burst...)
	burst = append(burst, []byte(proto.LinkLiveLine))
	upto := l.seq
	l.conn, l.sent = c, after
	l.cond.Broadcast()
	l.mu.Unlock()
	if err := writeLines(c, burst); err != nil {
		return
	}
	l.mu.Lock()
	l.sent = upto
	l.cond.Broadcast()
	l.mu.Unlock()
	if l.onAttach != nil {
		go l.onAttach()
	}
	go l.writeLive(c)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.HasPrefix(line, []byte(`{"type":"link_ack"`)) {
			var a proto.LinkAck
			if json.Unmarshal(line, &a) == nil {
				l.ack(a.Seq)
			}
			continue
		}
		if _, err := l.inW.Write(append(slices.Clone(line), '\n')); err != nil {
			return
		}
	}
}

func frameLine(f linkFrame) []byte {
	return append(strconv.AppendUint(nil, f.seq, 10), append([]byte(" "), f.line...)...)
}

func writeLines(w io.Writer, lines [][]byte) error {
	var buf bytes.Buffer
	for _, ln := range lines {
		buf.Write(ln)
		buf.WriteByte('\n')
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// writeLive sends each new event to c while it is the attached dund.
func (l *engineLink) writeLive(c net.Conn) {
	for {
		l.mu.Lock()
		for l.conn == c && l.sent >= l.seq {
			l.cond.Wait()
		}
		if l.conn != c {
			l.mu.Unlock()
			return
		}
		var out [][]byte
		for _, f := range l.ring {
			if f.seq > l.sent {
				out = append(out, frameLine(f))
			}
		}
		upto := l.seq
		l.mu.Unlock()
		if err := writeLines(c, out); err != nil {
			c.Close() // the reader sees it and releases
			return
		}
		// Sent once written: Close waits for it.
		l.mu.Lock()
		if l.conn == c {
			l.sent = upto
		}
		l.cond.Broadcast()
		l.mu.Unlock()
	}
}

// ack drops what dund took.
func (l *engineLink) ack(seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	seq = min(seq, l.seq)
	if seq <= l.acked {
		return
	}
	l.acked = seq
	n := 0
	for n < len(l.ring) && l.ring[n].seq <= seq {
		l.bytes -= len(l.ring[n].line)
		n++
	}
	l.ring = l.ring[n:]
	l.cond.Broadcast()
}

// release ends a connection: no dund is attached until the next one.
func (l *engineLink) release(c net.Conn) {
	c.Close()
	l.mu.Lock()
	if l.conn == c {
		l.conn = nil
		l.alone = time.Now()
	}
	l.cond.Broadcast()
	l.mu.Unlock()
}

func (l *engineLink) isClosing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closing
}

// watchOrphan stops the engine after linkOrphanWait with no dund, or
// after linkForeignWait with a dund running that does not attach (one that
// does not adopt engines resumes the session on its own engine).
func (l *engineLink) watchOrphan() {
	t := time.NewTicker(min(time.Second, min(l.orphanWait, l.foreignWait)/4+time.Millisecond))
	defer t.Stop()
	var foreign time.Time // since when a dund runs and is not attached
	for range t.C {
		l.mu.Lock()
		alone := l.conn == nil && !l.closing && !l.orphan
		closing := l.closing
		l.mu.Unlock()
		if closing {
			return
		}
		why := ""
		switch {
		case !alone:
			foreign = time.Time{}
			continue
		case l.dundUp != nil && l.dundUp():
			if foreign.IsZero() {
				foreign = time.Now()
			}
			if time.Since(foreign) >= l.foreignWait {
				why = fmt.Sprintf("a dund runs and has not attached in %s", l.foreignWait)
			}
		default:
			foreign = time.Time{}
		}
		l.mu.Lock()
		if why == "" && time.Since(l.alone) >= l.orphanWait {
			why = fmt.Sprintf("no dund for %s", l.orphanWait)
		}
		if why != "" {
			l.orphan = true
		}
		l.mu.Unlock()
		if why != "" {
			fmt.Fprintf(os.Stderr, "dun: %s: stopping\n", why)
			go func() { _, _ = l.inW.Write([]byte(`{"type":"stop"}` + "\n")) }()
		}
	}
}

// errLinkClosed: the engine is exiting.
var errLinkClosed = errors.New("engine link closed")

// Close is the engine exiting: the last events go to the attached dund
// (within linkFlush), then the socket and <pid>.json are removed.
func (l *engineLink) Close() {
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		return
	}
	if len(l.partial) > 0 {
		l.add(l.partial)
		l.partial = nil
	}
	l.closing = true
	l.cond.Broadcast()
	deadline := time.Now().Add(linkFlush)
	for l.conn != nil && l.sent < l.seq && time.Now().Before(deadline) {
		l.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		l.mu.Lock()
	}
	c := l.conn
	l.mu.Unlock()
	l.ln.Close()
	if c != nil {
		// What was written is in the socket; closing it is dund's EOF.
		if uc, ok := c.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
		c.Close()
	}
	_ = l.inW.CloseWithError(errLinkClosed)
	_ = os.Remove(l.sock)
	_ = os.Remove(l.infoPath)
}

// linkExit closes the engine link, when there is one, before the process
// exits (fatal, main's return).
var linkExit func()
