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
	// linkAckEvery: dund acks this many events at a time (and exactly on
	// detach), so the engine's ring stays small.
	linkAckEvery uint64 = 64
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
	detaching bool
	readDone  chan struct{}
}

// newLinkEngine sends the resume and starts reading.
func newLinkEngine(conn net.Conn, sc *bufio.Scanner, hello proto.LinkHello, p proc, res proto.LinkResume) (*linkEngine, error) {
	e := &linkEngine{pid: hello.Pid, proc: p, conn: conn, hello: hello, readDone: make(chan struct{}),
		acked: res.After, delivered: res.After}
	e.out, e.outW = io.Pipe()
	res.Type = proto.LinkResumeType
	if err := e.writeJSON(res); err != nil {
		return nil, fmt.Errorf("engine link: %w", err)
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
		due := seq-e.acked >= linkAckEvery
		if due {
			e.acked = seq
		}
		e.mu.Unlock()
		if due {
			_ = e.writeJSON(proto.LinkAck{Type: proto.LinkAckType, Seq: seq})
		}
	}
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

// connectNew dials a new engine's socket until it listens or exits.
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
			e, err := newLinkEngine(c, sc, h, p, proto.LinkResume{After: 0})
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
// Accept makes it a session's engine, Refuse ends it.
type Adoption struct {
	Info  proto.EngineInfo
	Hello proto.LinkHello
	conn  net.Conn
	sc    *bufio.Scanner
	dir   string
}

// Accept resumes the engine after what the last dund took, its state first.
func (a *Adoption) Accept() (Engine, error) {
	e, err := newLinkEngine(a.conn, a.sc, a.Hello, adoptProc(a.Hello.Pid),
		proto.LinkResume{After: a.Hello.Acked, State: true})
	if err != nil {
		a.conn.Close()
		return nil, err
	}
	return e, nil
}

// Refuse ends an engine no session takes: it is ours (it answered on its
// socket with its pid), so it is killed and its files removed.
func (a *Adoption) Refuse(why string) {
	log.Printf("dund: engine %d (session %q): %s: killed", a.Hello.Pid, a.Hello.Session, why)
	a.conn.Close()
	adoptProc(a.Hello.Pid).kill()
	removeEngineFiles(a.dir, a.Info.Pid)
}

// Adopter is a Spawner that can hand back the engines an earlier dund left.
// EngineBuild is the build it starts engines on ("" when unknown): an
// adopted engine of another build moves to it at its next idle.
type Adopter interface {
	Adopt() []*Adoption
	EngineBuild() string
}

// EngineBuild is Version.
func (s ExecSpawner) EngineBuild() string { return s.Version }

// Adopt is ExecSpawner's: the engines in its EnginesDir.
func (s ExecSpawner) Adopt() []*Adoption {
	if s.EnginesDir == "" {
		return nil
	}
	reapExited()
	return adoptEngines(s.EnginesDir)
}

// adoptEngines dials every engine in dir that an earlier dund left and
// keeps those fit to adopt. An engine of another link or proto version is
// killed; a dead socket's files are removed; one whose hello does not
// match its file is left alone. Each refusal is logged.
func adoptEngines(dir string) []*Adoption {
	pruneEngineLogs(dir)
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []*Adoption
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var info proto.EngineInfo
		if json.Unmarshal(b, &info) != nil || info.Pid <= 0 {
			log.Printf("dund: %s: not an engine entry; removed", p)
			os.Remove(p)
			continue
		}
		sock := info.Sock
		if sock == "" {
			sock = filepath.Join(dir, strconv.Itoa(info.Pid)+".sock")
		}
		c, sc, h, err := dialAdopt(sock)
		if err != nil {
			log.Printf("dund: engine %d (session %q) not adopted: %v; its session resumes from the record", info.Pid, info.Session, err)
			if !errors.Is(err, errLinkBusy) {
				removeEngineFiles(dir, info.Pid)
			}
			continue
		}
		a := &Adoption{Info: info, Hello: h, conn: c, sc: sc, dir: dir}
		switch {
		case h.Pid != info.Pid || h.Session != info.Session:
			log.Printf("dund: engine %d answered as pid %d of session %q, not session %q: left alone", info.Pid, h.Pid, h.Session, info.Session)
			c.Close()
			continue
		case h.Link != proto.LinkVersion || h.Proto != proto.Version:
			a.Refuse(fmt.Sprintf("link %d proto %d, this dund speaks link %d proto %d", h.Link, h.Proto, proto.LinkVersion, proto.Version))
			continue
		case h.Session == "":
			a.Refuse("it never named a session")
			continue
		}
		out = append(out, a)
	}
	return out
}

// dialAdopt is dialLink for an adoption. "Busy" right after a restart is
// usually the last dund's connection, which the engine has not seen close
// yet (an in-place exec is quick): tried again for adoptDial.
func dialAdopt(sock string) (net.Conn, *bufio.Scanner, proto.LinkHello, error) {
	deadline := time.Now().Add(adoptDial)
	for {
		c, sc, h, err := dialLink(sock, adoptDial)
		if !errors.Is(err, errLinkBusy) || time.Now().After(deadline) {
			return c, sc, h, err
		}
		time.Sleep(20 * time.Millisecond)
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
