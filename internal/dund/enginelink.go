package dund

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/dun/proto"
)

// dund's half of the engine link (docs/engine-adoption.md, the engine's
// half is cmd/dun/enginelink.go). An engine started with --listen serves
// its events and commands on its own unix socket; dund dials it. The
// engine then outlives dund's restart, and the next dund adopts it
// (Service.New, adoptEngines).

// Variables so tests can shorten them.
var (
	// linkDialWait bounds how long a new engine may take to listen.
	linkDialWait = 30 * time.Second
	// dund acks what it took (and exactly on detach) every linkAckEvery
	// events or linkAckBytes bytes, whichever first, and linkAckIdle after
	// the last event it has not acked: the engine waits on a full ring
	// while attached, so an ack that never comes would hang the turn.
	linkAckEvery uint64 = 64
	linkAckBytes        = 1 << 20
	linkAckIdle         = 200 * time.Millisecond
	// adoptDial bounds the dial and the hello of an engine at adoption.
	adoptDial = 3 * time.Second
	// engineLogKeep: logs of engines long gone are pruned after this.
	engineLogKeep = 7 * 24 * time.Hour
)

// maxLinkSock is the longest engine socket path (cmd/dun/enginelink.go).
const maxLinkSock = 103

// errDetached is an engine's Wait after dund let go of it: it runs on.
var errDetached = errors.New("engine detached")

// errLinkBusy: another dund is attached to the engine (link_busy).
var errLinkBusy = errors.New("another dund is attached to it")

// proc is the engine's process: a child dund started, or one it adopted.
type proc interface {
	wait() error
	kill()
}

// cmdProc is a child dund started: waited for by its exec.Cmd.
type cmdProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startProc(cmd *exec.Cmd) *cmdProc {
	p := &cmdProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p
}

func (p *cmdProc) wait() error { <-p.done; return p.err }
func (p *cmdProc) kill()       { killGroup(p.cmd) }

// pidProc is an adopted engine, known by its pid only: still dund's child
// after an in-place exec (reaped), else polled.
type pidProc struct{ pid int }

func (p pidProc) wait() error { return waitPid(p.pid) }
func (p pidProc) kill()       { killPidGroup(p.pid) }

// adoptProc makes the proc of an adopted engine (a variable: a test's
// fake engines are no processes).
var adoptProc = func(pid int) proc { return pidProc{pid} }

// linkEngine is an Engine over the link: commands go out on the socket,
// events come in seq-numbered and are handed on as plain lines (Stdout).
type linkEngine struct {
	pid   int
	proc  proc
	conn  net.Conn
	hello proto.LinkHello
	out   *io.PipeReader
	outW  *io.PipeWriter
	wmu   sync.Mutex // writes on conn

	mu        sync.Mutex
	delivered uint64 // the last event handed to Stdout
	acked     uint64
	unacked   int // bytes taken since the last ack
	ackTimer  *time.Timer
	detaching bool
	readDone  chan struct{}
}

// PID is the engine's process (dun ps).
func (e *linkEngine) PID() int { return e.pid }

// newLinkEngine sends the resume (with the engine's token) and starts
// reading once the engine took it: errLinkBusy when another dund is
// attached.
func newLinkEngine(conn net.Conn, sc *bufio.Scanner, hello proto.LinkHello, p proc, res proto.LinkResume) (*linkEngine, error) {
	e := &linkEngine{pid: hello.Pid, proc: p, conn: conn, hello: hello, readDone: make(chan struct{}),
		acked: res.After, delivered: res.After}
	e.out, e.outW = io.Pipe()
	res.Type = proto.LinkResumeType
	if err := e.writeJSON(res); err != nil {
		return nil, fmt.Errorf("engine link: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(adoptDial))
	if !sc.Scan() {
		return nil, errors.New("engine link: the engine refused the resume (a wrong token?)")
	}
	_ = conn.SetReadDeadline(time.Time{})
	switch sc.Text() {
	case proto.LinkOkLine:
	case proto.LinkBusyLine:
		return nil, errLinkBusy
	default:
		return nil, fmt.Errorf("engine link: %.80q is not link_ok", sc.Text())
	}
	go e.read(sc)
	return e, nil
}

func (e *linkEngine) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	e.wmu.Lock()
	defer e.wmu.Unlock()
	_, err = e.conn.Write(append(b, '\n'))
	return err
}

// read hands the engine's events on until the socket ends, or a detach.
// The link's markers go on in band (readEvents): a gap, the end of the
// replay.
func (e *linkEngine) read(sc *bufio.Scanner) {
	defer close(e.readDone)
	defer e.outW.Close()
	for sc.Scan() {
		line := sc.Bytes()
		e.mu.Lock()
		stop := e.detaching
		e.mu.Unlock()
		if stop {
			return // not taken: the next dund gets it (not acked)
		}
		if len(line) > 0 && line[0] == '{' {
			switch {
			case bytes.HasPrefix(line, []byte(`{"type":"link_gap"`)):
				line = []byte(linkGapMark)
			case string(line) == proto.LinkLiveLine:
			default:
				continue
			}
			if _, err := e.outW.Write(append(line, '\n')); err != nil {
				return
			}
			continue
		}
		n, ev, ok := bytes.Cut(line, []byte(" "))
		if !ok {
			continue
		}
		seq, err := strconv.ParseUint(string(n), 10, 64)
		if err != nil {
			continue
		}
		if _, err := e.outW.Write(append(ev, '\n')); err != nil {
			return
		}
		if seq == 0 { // a state event resent on an adoption
			continue
		}
		e.mu.Lock()
		e.delivered = seq
		e.unacked += len(line)
		due := seq-e.acked >= linkAckEvery || e.unacked >= linkAckBytes
		if !due {
			if e.ackTimer == nil {
				e.ackTimer = time.AfterFunc(linkAckIdle, e.ackNow)
			} else {
				e.ackTimer.Reset(linkAckIdle)
			}
		}
		e.mu.Unlock()
		if due {
			e.ackNow()
		}
	}
}

// ackNow acks what was taken, if anything since the last ack.
func (e *linkEngine) ackNow() {
	e.mu.Lock()
	seq := e.delivered
	if e.detaching || seq <= e.acked {
		e.mu.Unlock()
		return
	}
	e.acked, e.unacked = seq, 0
	e.mu.Unlock()
	_ = e.writeJSON(proto.LinkAck{Type: proto.LinkAckType, Seq: seq})
}

// linkGapMark is what Stdout carries for a link_gap: readEvents sends the
// conversation again (history, reason reattach).
const linkGapMark = `{"type":"link_gap"}`

type linkWriter struct{ e *linkEngine }

func (w linkWriter) Write(p []byte) (int, error) {
	w.e.mu.Lock()
	gone := w.e.detaching
	w.e.mu.Unlock()
	if gone {
		return 0, errDetached
	}
	w.e.wmu.Lock()
	defer w.e.wmu.Unlock()
	return w.e.conn.Write(p)
}

func (e *linkEngine) Stdin() io.Writer  { return linkWriter{e} }
func (e *linkEngine) Stdout() io.Reader { return e.out }

// Wait is the socket's end, then the process's.
func (e *linkEngine) Wait() error {
	<-e.readDone
	e.mu.Lock()
	gone := e.detaching
	e.mu.Unlock()
	if gone {
		return errDetached
	}
	_ = e.conn.Close()
	return e.proc.wait()
}

func (e *linkEngine) Kill() {
	_ = e.conn.Close()
	e.proc.kill()
}

// stopReading is a detach's first half: no event is taken after it. The
// session's reader then appends what it was handed and ends.
func (e *linkEngine) stopReading() {
	e.mu.Lock()
	e.detaching = true
	if e.ackTimer != nil {
		e.ackTimer.Stop()
	}
	e.mu.Unlock()
	_ = e.conn.SetReadDeadline(time.Now())
	<-e.readDone
}

// finishDetach acks exactly what was taken and lets go: the next dund
// resumes after it.
func (e *linkEngine) finishDetach() {
	e.mu.Lock()
	seq := e.delivered
	e.mu.Unlock()
	_ = e.writeJSON(proto.LinkAck{Type: proto.LinkAckType, Seq: seq})
	_ = e.conn.Close()
}

// dialLink dials an engine's socket and reads its hello.
func dialLink(sock string, timeout time.Duration) (net.Conn, *bufio.Scanner, proto.LinkHello, error) {
	c, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		return nil, nil, proto.LinkHello{}, err
	}
	sc := newScanner(c)
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	if !sc.Scan() {
		c.Close()
		if err := sc.Err(); err != nil {
			return nil, nil, proto.LinkHello{}, fmt.Errorf("no hello: %w", err)
		}
		return nil, nil, proto.LinkHello{}, errors.New("no hello")
	}
	_ = c.SetReadDeadline(time.Time{})
	if sc.Text() == proto.LinkBusyLine {
		c.Close()
		return nil, nil, proto.LinkHello{}, errLinkBusy
	}
	var h proto.LinkHello
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil || h.Type != proto.LinkHelloType {
		c.Close()
		return nil, nil, proto.LinkHello{}, fmt.Errorf("not a hello: %.80q", sc.Text())
	}
	return c, sc, h, nil
}

// connectNew dials a new engine's socket until it listens or exits; the
// token is in <pid>.json beside the socket, written before it listens.
func connectNew(sock string, p *cmdProc, logPath string) (*linkEngine, error) {
	deadline := time.Now().Add(linkDialWait)
	delay := 5 * time.Millisecond
	for {
		select {
		case <-p.done:
			return nil, fmt.Errorf("engine exited before it listened (%v)%s", p.err, logTail(logPath))
		default:
		}
		c, sc, h, err := dialLink(sock, time.Second)
		if err == nil {
			info, ierr := readEngineInfo(strings.TrimSuffix(sock, ".sock") + ".json")
			if ierr != nil {
				c.Close()
				p.kill()
				return nil, fmt.Errorf("engine link: %w", ierr)
			}
			e, err := newLinkEngine(c, sc, h, p, proto.LinkResume{After: 0, Token: info.Token})
			if err != nil {
				c.Close()
				p.kill()
			}
			return e, err
		}
		if time.Now().After(deadline) {
			p.kill()
			return nil, fmt.Errorf("engine did not listen on %s within %s: %v", sock, linkDialWait, err)
		}
		time.Sleep(delay)
		delay = min(delay*2, 100*time.Millisecond)
	}
}

// logTail is the end of an engine's log, for an error.
func logTail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	if len(b) > 600 {
		b = b[len(b)-600:]
	}
	return ": " + strings.TrimSpace(string(b))
}

// spawnLinked starts an engine on the link: stdin /dev/null, its output to
// <dir>/<pid>.log, under systemd in a scope of its own (Scope).
func (s ExecSpawner) spawnLinked(workspace string, args []string) (Engine, error) {
	dir := s.EnginesDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}
	logf, err := os.CreateTemp(dir, "spawn-*.log")
	if err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}
	name, argv := s.linkedCommand(workspace, args)
	cmd := exec.Command(name, argv...)
	cmd.Env = append(append(os.Environ(), "DUN_CHILD=1", "DUN_DUND=1"), s.Env...)
	cmd.Stdout, cmd.Stderr = logf, logf
	ownGroup(cmd)
	err = cmd.Start()
	logf.Close() // the engine has its own copy
	if err != nil {
		os.Remove(logf.Name())
		return nil, fmt.Errorf("spawn %s: %w", name, err)
	}
	pid := cmd.Process.Pid
	logPath := filepath.Join(dir, strconv.Itoa(pid)+".log")
	if os.Rename(logf.Name(), logPath) != nil {
		logPath = logf.Name()
	}
	return connectNew(filepath.Join(dir, strconv.Itoa(pid)+".sock"), startProc(cmd), logPath)
}

// linkedCommand is the command a linked engine starts with: dun --engine
// --listen, inside systemd-run's scope when Scope and it works.
func (s ExecSpawner) linkedCommand(workspace string, args []string) (string, []string) {
	argv := append([]string{"--engine", "--workspace", workspace, "--listen", s.EnginesDir}, args...)
	if s.Scope {
		if pre := scopeArgs(); pre != nil {
			return pre[0], append(append(slices.Clone(pre[1:]), s.Bin), argv...)
		}
	}
	return s.Bin, argv
}

// linkable reports whether dir can hold engine sockets: their path must
// fit in a sun_path.
func linkable(dir string) bool {
	return dir != "" && len(filepath.Join(dir, "4194304.sock")) <= maxLinkSock
}

// The systemd scope (USER 2026-11-17): each engine in a transient scope of
// its own, so the service's cgroup — which systemd ends when dund exits —
// does not hold it. Probed once; without systemd-run or a user manager
// engines start plainly, and a handover ends them (the fallback resumes).
var (
	scopeOnce sync.Once
	scopePre  []string
	// scopeProbe runs the probe (a variable for tests).
	scopeProbe = func(args ...string) error { return exec.Command(args[0], args[1:]...).Run() }
)

// scopeArgs is the systemd-run prefix, nil when none works.
func scopeArgs() []string {
	scopeOnce.Do(func() {
		base := []string{"systemd-run", "--user", "--scope", "--quiet", "--collect"}
		// systemd 254+ expands $VAR in the command line unless told not to;
		// older ones do not, and refuse the flag.
		for _, pre := range [][]string{append(base, "--expand-environment=no"), base} {
			if scopeProbe(append(pre, "--", "true")...) == nil {
				scopePre = append(pre, "--")
				return
			}
		}
		log.Printf("dund: systemd-run --user --scope does not work here: engines start in dund's own cgroup, so a service restart ends them (their sessions resume from the record)")
	})
	return scopePre
}

// Adoption is an engine an earlier dund left running, dialed and heard:
// Accept makes it a session's engine.
type Adoption struct {
	Info  proto.EngineInfo
	Hello proto.LinkHello
	conn  net.Conn
	sc    *bufio.Scanner
	dir   string
}

// Accept resumes the engine after what the last dund took, its state
// first. "Busy" right after a restart is usually the last dund's
// connection, which the engine has not seen close yet (an in-place exec is
// quick): it is dialed again for adoptDial.
func (a *Adoption) Accept() (Engine, error) {
	deadline := time.Now().Add(adoptDial)
	for {
		e, err := newLinkEngine(a.conn, a.sc, a.Hello, adoptProc(a.Hello.Pid),
			proto.LinkResume{After: a.Hello.Acked, State: true, Token: a.Info.Token})
		if err == nil {
			return e, nil
		}
		a.conn.Close()
		if !errors.Is(err, errLinkBusy) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
		c, sc, h, derr := dialLink(engineSock(a.dir, a.Info), adoptDial)
		if derr != nil {
			return nil, derr
		}
		if h.Pid != a.Info.Pid || h.Session != a.Info.Session {
			c.Close()
			return nil, fmt.Errorf("answered as pid %d of session %q", h.Pid, h.Session)
		}
		a.conn, a.sc, a.Hello = c, sc, h
	}
}

// AdoptFailure is an engine an earlier dund left that is not adopted, and
// why. It may still run: endEngine ends it, provably, before its session
// starts another engine (never two on one transcript).
type AdoptFailure struct {
	Info proto.EngineInfo
	Why  string
	Dir  string // the engines directory
}

// Adopter is a Spawner that can hand back the engines an earlier dund left.
// EngineBuild is the build it starts engines on ("" when unknown): an
// adopted engine of another build moves to it at its next idle.
type Adopter interface {
	Adopt() ([]*Adoption, []AdoptFailure)
	EngineBuild() string
}

// EngineBuild is Version.
func (s ExecSpawner) EngineBuild() string { return s.Version }

// Adopt is ExecSpawner's: the engines in its EnginesDir.
func (s ExecSpawner) Adopt() ([]*Adoption, []AdoptFailure) {
	if s.EnginesDir == "" {
		return nil, nil
	}
	reapExited()
	return adoptEngines(s.EnginesDir)
}

func engineSock(dir string, info proto.EngineInfo) string {
	if info.Sock != "" {
		return info.Sock
	}
	return filepath.Join(dir, strconv.Itoa(info.Pid)+".sock")
}

func readEngineInfo(path string) (proto.EngineInfo, error) {
	var info proto.EngineInfo
	b, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil || info.Pid <= 0 {
		return info, fmt.Errorf("%s: not an engine entry", path)
	}
	return info, nil
}

// adoptEngines dials every engine in dir that an earlier dund left: the
// ones fit to adopt, and the others with why. It ends none.
func adoptEngines(dir string) ([]*Adoption, []AdoptFailure) {
	pruneEngineLogs(dir)
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var ok []*Adoption
	var failed []AdoptFailure
	for _, p := range paths {
		info, err := readEngineInfo(p)
		if err != nil {
			log.Printf("dund: %v; removed", err)
			os.Remove(p)
			continue
		}
		fail := func(why string) { failed = append(failed, AdoptFailure{Info: info, Why: why, Dir: dir}) }
		c, sc, h, err := dialLink(engineSock(dir, info), adoptDial)
		switch {
		case err != nil:
			fail(err.Error())
			continue
		case h.Pid != info.Pid || h.Session != info.Session:
			c.Close()
			fail(fmt.Sprintf("answered as pid %d of session %q", h.Pid, h.Session))
			continue
		case h.Link != proto.LinkVersion || h.Proto != proto.Version:
			c.Close()
			fail(fmt.Sprintf("link %d proto %d, this dund speaks link %d proto %d", h.Link, h.Proto, proto.LinkVersion, proto.Version))
			continue
		case h.Session == "":
			c.Close()
			fail("it never named a session")
			continue
		}
		ok = append(ok, &Adoption{Info: info, Hello: h, conn: c, sc: sc, dir: dir})
	}
	return ok, failed
}

// endEngine ends an engine that is not adopted and reports whether it is
// gone: not running, or a pid since reused by another program, or killed
// and seen to exit. false: it may still run (it could not be proven an
// engine, or did not exit), and its session must not start another. A
// variable: the tests' engines are no processes.
var endEngine = func(dir string, info proto.EngineInfo, why string) bool {
	pid := info.Pid
	if !pidAlive(pid) {
		removeEngineFiles(dir, pid)
		return true
	}
	switch verifyEngine(pid, dir, info.Started) {
	case engineNot:
		log.Printf("dund: engine %d (session %q) not adopted (%s): the pid is another program now", pid, info.Session, why)
		removeEngineFiles(dir, pid)
		return true
	case engineUnknown:
		log.Printf("dund: engine %d (session %q) not adopted (%s), and it cannot be told whether pid %d is still that engine: left alone, its session not resumed", pid, info.Session, why, pid)
		return false
	}
	log.Printf("dund: engine %d (session %q) not adopted (%s): killed", pid, info.Session, why)
	killPidGroup(pid)
	done := make(chan struct{})
	go func() { _ = waitPid(pid); close(done) }()
	select {
	case <-done:
		removeEngineFiles(dir, pid)
		return true
	case <-time.After(5 * time.Second):
		log.Printf("dund: engine %d did not exit after SIGKILL: its session is not resumed", pid)
		return false
	}
}

func removeEngineFiles(dir string, pid int) {
	n := strconv.Itoa(pid)
	_ = os.Remove(filepath.Join(dir, n+".json"))
	_ = os.Remove(filepath.Join(dir, n+".sock"))
}

// pruneEngineLogs removes the logs of engines long gone.
func pruneEngineLogs(dir string) {
	logs, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	for _, p := range logs {
		if _, err := os.Stat(strings.TrimSuffix(p, ".log") + ".json"); err == nil {
			continue
		}
		if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > engineLogKeep {
			_ = os.Remove(p)
		}
	}
}
