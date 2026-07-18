package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

// The launcher (`dun -d`) — a thin, long-lived supervisor. It does NOT own the
// sessions' engines or MCP servers (each session keeps its own worktree +
// tools); instead it holds a live REGISTRY of sessions and owns the source
// watch + rebuild CENTRALLY: one watcher/build for the whole machine instead of
// every `dun` self-checking. When it produces a fresh build it pushes a "reload"
// to registered sessions, which surface it and re-exec on demand. Idle-exits
// after idleTTL with no sessions, so it's lazy-started and self-cleaning.

const (
	idleTTL       = 10 * time.Minute
	watchInterval = 2 * time.Second
)

type regSession struct {
	meta    sessMeta
	conn    net.Conn
	started time.Time
}

type launcher struct {
	srcDir string
	exe    string

	mu       sync.Mutex
	sessions map[string]*regSession
	seq      int
	curVer   string
	lastSeen time.Time // last time a session was registered/present (for idle-exit)
}

// runLauncher is `dun -d`: bind the socket (or exit if one's already up), then
// serve until idle. srcDir/exe let it rebuild centrally ("" srcDir = a release
// build → no watch, just registry).
func runLauncher() error {
	sock := launcherSocket()
	// If a launcher is already listening, don't double-start.
	if c, err := net.Dial("unix", sock); err == nil {
		_ = c.Close()
		return fmt.Errorf("launcher already running at %s", sock)
	}
	_ = os.Remove(sock) // stale socket from a crash
	if err := os.MkdirAll(dunHome(), 0o700); err != nil {
		return err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close(); _ = os.Remove(sock) }()

	exe, _ := os.Executable()
	l := &launcher{
		srcDir:   srcDir,
		exe:      exe,
		sessions: map[string]*regSession{},
		curVer:   version,
		lastSeen: time.Now(),
	}
	l.buildIfStale() // start from a fresh build
	log.Printf("dun -d launcher up (%s) → %s", l.curVer, sock)

	go l.watch()
	go l.reap()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil // listener closed (idle-exit)
		}
		go l.handle(conn)
	}
}

// handle reads one request line and dispatches. A "register" keeps the conn.
func (l *launcher) handle(conn net.Conn) {
	dec := json.NewDecoder(bufio.NewReader(conn))
	var r req
	if err := dec.Decode(&r); err != nil {
		_ = conn.Close()
		return
	}
	switch r.Op {
	case "register":
		l.register(conn, r) // keeps conn open until the session leaves
	case "status":
		l.writeResp(conn, l.status())
		_ = conn.Close()
	case "reload":
		l.buildIfStale()
		l.writeResp(conn, resp{OK: true, Version: l.version()})
		_ = conn.Close()
	case "shutdown":
		l.shutdown(conn, r.Force)
	default:
		l.writeResp(conn, resp{Err: "unknown op " + r.Op})
		_ = conn.Close()
	}
}

func (l *launcher) writeResp(conn net.Conn, r resp) {
	b, _ := json.Marshal(r)
	_, _ = conn.Write(append(b, '\n'))
}

func (l *launcher) register(conn net.Conn, r req) {
	l.mu.Lock()
	l.seq++
	id := "s" + strconv.Itoa(l.seq)
	l.sessions[id] = &regSession{
		meta:    sessMeta{ID: id, Kind: r.Kind, PID: r.PID, Workspace: r.Workspace, Version: r.Version},
		conn:    conn,
		started: time.Now(),
	}
	l.lastSeen = time.Now()
	cur := l.curVer
	l.mu.Unlock()

	l.writeResp(conn, resp{OK: true, ID: id, Version: cur})

	// Block until the session's conn closes (that's its "I left"), then dereg.
	_, _ = bufio.NewReader(conn).ReadString('\n')
	l.mu.Lock()
	delete(l.sessions, id)
	l.mu.Unlock()
	_ = conn.Close()
}

func (l *launcher) status() resp {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]sessMeta, 0, len(l.sessions))
	for _, s := range l.sessions {
		m := s.meta
		m.AgeSec = int(time.Since(s.started).Seconds())
		out = append(out, m)
	}
	return resp{OK: true, Version: l.curVer, Sessions: out}
}

// shutdown refuses while sessions are attached unless forced; on go, closes the
// listener path by removing the socket and exiting (deferred cleanup in runLauncher
// won't fire on os.Exit, so remove here).
func (l *launcher) shutdown(conn net.Conn, force bool) {
	l.mu.Lock()
	n := len(l.sessions)
	web := 0
	for _, s := range l.sessions {
		if s.meta.Kind == "web" {
			web++
		}
	}
	l.mu.Unlock()
	if n > 0 && !force {
		l.writeResp(conn, resp{OK: false, Attached: n, Web: web})
		_ = conn.Close()
		return
	}
	l.writeResp(conn, resp{OK: true, Attached: n, Web: web})
	_ = conn.Close()
	_ = os.Remove(launcherSocket())
	os.Exit(0)
}

func (l *launcher) version() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.curVer
}

// buildIfStale rebuilds the shared binary when the source tree is newer, and on
// a new version pushes "reload" to every registered session. No-op for a release
// build (srcDir == "").
func (l *launcher) buildIfStale() {
	if l.srcDir == "" || l.exe == "" {
		return
	}
	st, err := os.Stat(l.exe)
	if err != nil || !sourceNewerThan(l.srcDir, st.ModTime()) {
		return
	}
	ver, err := rebuildDun(l.srcDir, l.exe)
	if err != nil {
		log.Printf("dun -d: rebuild failed: %v", err)
		return
	}
	l.mu.Lock()
	l.curVer = ver
	conns := make([]net.Conn, 0, len(l.sessions))
	for _, s := range l.sessions {
		conns = append(conns, s.conn)
	}
	l.mu.Unlock()
	log.Printf("dun -d: rebuilt → %s (notifying %d sessions)", ver, len(conns))
	msg, _ := json.Marshal(push{Event: "reload", Version: ver})
	for _, c := range conns {
		_, _ = c.Write(append(msg, '\n'))
	}
}

func (l *launcher) watch() {
	for range time.Tick(watchInterval) {
		l.buildIfStale()
	}
}

// reap exits the launcher once it's been idle (no sessions) for idleTTL, so it's
// self-cleaning after lazy auto-start.
func (l *launcher) reap() {
	for range time.Tick(time.Minute) {
		l.mu.Lock()
		if len(l.sessions) > 0 {
			l.lastSeen = time.Now()
		}
		idle := time.Since(l.lastSeen)
		l.mu.Unlock()
		if idle > idleTTL {
			log.Printf("dun -d: idle %s → shutting down", idle.Round(time.Second))
			_ = os.Remove(launcherSocket())
			os.Exit(0)
		}
	}
}
