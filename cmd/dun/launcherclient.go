package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Client side of the launcher: lazy auto-start, session registration (a
// long-lived connection whose close = "this session left"), and the
// status/shutdown queries behind `dun -d status|shutdown`.

// launcherConn is a session's live registration. Close it when the session ends.
type launcherConn struct {
	conn        net.Conn
	id          string
	launcherVer string      // the launcher's build version at register time
	reload      chan string // a new build's version, pushed by the launcher
}

func (lc *launcherConn) close() {
	if lc != nil && lc.conn != nil {
		_ = lc.conn.Close()
	}
}

// ensureLauncher connects to the launcher, lazily starting it (detached) if
// absent. Returns false if it couldn't be reached/started.
func ensureLauncher() bool {
	sock := launcherSocket()
	if dialOK(sock) {
		return true
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	cmd := exec.Command(exe, "-d")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detached: outlives the client
	// The launcher rebuilds the shared binary itself; don't let it self-re-exec.
	cmd.Env = append(os.Environ(), "DUN_NO_AUTOBUILD=1")
	if f, e := os.OpenFile(filepath.Join(dunHome(), "launcher.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); e == nil {
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	_ = cmd.Process.Release()
	for i := 0; i < 30; i++ { // ~3s for the socket to appear
		if dialOK(sock) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func dialOK(sock string) bool {
	c, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// registerSession registers this process with the launcher and starts reading
// reload pushes. Returns nil if the launcher is unavailable (dun still runs —
// the launcher is an optional supervisor).
func registerSession(kind, workspace string) *launcherConn {
	if !ensureLauncher() {
		return nil
	}
	conn, err := net.Dial("unix", launcherSocket())
	if err != nil {
		return nil
	}
	if err := json.NewEncoder(conn).Encode(req{
		Op: "register", Kind: kind, PID: os.Getpid(), Workspace: workspace, Version: version,
	}); err != nil {
		_ = conn.Close()
		return nil
	}
	dec := json.NewDecoder(conn)
	var r resp
	if err := dec.Decode(&r); err != nil || !r.OK {
		_ = conn.Close()
		return nil
	}
	lc := &launcherConn{conn: conn, id: r.ID, launcherVer: r.Version, reload: make(chan string, 4)}
	go func() {
		for {
			var p push
			if err := dec.Decode(&p); err != nil {
				close(lc.reload)
				return
			}
			if p.Event == "reload" {
				select {
				case lc.reload <- p.Version:
				default:
				}
			}
		}
	}()
	return lc
}

// printLauncherStatus implements `dun -d status`.
func printLauncherStatus() {
	r, ok := query(req{Op: "status"})
	if !ok {
		fmt.Fprintln(os.Stderr, "dun: no launcher running")
		return
	}
	fmt.Printf("launcher %s · %d session(s)\n", r.Version, len(r.Sessions))
	for _, s := range r.Sessions {
		fmt.Printf("  %-4s %-5s pid=%-7d %-12s %ds  %s\n", s.ID, s.Kind, s.PID, s.Version, s.AgeSec, s.Workspace)
	}
}

// shutdownLauncher implements `dun -d shutdown` (refuses with attached sessions
// unless --force — the kick-warning).
func shutdownLauncher(force bool) {
	r, ok := query(req{Op: "shutdown", Force: force})
	if !ok {
		fmt.Fprintln(os.Stderr, "dun: no launcher running")
		return
	}
	if !r.OK {
		fmt.Fprintf(os.Stderr, "dun: %d session(s) attached (%d web) — re-run `dun -d shutdown --force` to kill them\n", r.Attached, r.Web)
		os.Exit(1)
	}
	fmt.Println("launcher shut down")
}

// query sends one request on a fresh connection and returns the response.
func query(r req) (resp, bool) {
	if !dialOK(launcherSocket()) {
		return resp{}, false
	}
	conn, err := net.Dial("unix", launcherSocket())
	if err != nil {
		return resp{}, false
	}
	defer conn.Close()
	if json.NewEncoder(conn).Encode(r) != nil {
		return resp{}, false
	}
	var out resp
	if json.NewDecoder(conn).Decode(&out) != nil {
		return resp{}, false
	}
	return out, true
}
