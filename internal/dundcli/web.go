package dundcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/internal/dundapi"
	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/internal/webterm"
	"github.com/iodesystems/dun/proto"
)

// dund's HTTP listener, started and stopped while dund runs (USER
// 2026-11-17): `dun dund enable|disable web` and `enable|disable tui` reach
// the running dund over its socket (proto.WebSet), and the result is saved in
// <home>/dund/web.json. The saved state wins: once web.json exists it decides
// at every start, and --web-experimental (or --http) only seeds it while it
// does not. One listener serves the chooser, /web, /tui and the API
// (dundapi front.go).

// defaultWebAddr is where the listener goes when nothing names an address.
const defaultWebAddr = "127.0.0.1:20003"

// webState is web.json.
type webState struct {
	Enabled bool   `json:"enabled"`
	Addr    string `json:"addr"`
	TUI     bool   `json:"tui"`
}

func webStatePath(home string) string { return filepath.Join(home, "dund", "web.json") }

// loadWebState is web.json, and whether it exists.
func loadWebState(home string) (webState, bool, error) {
	b, err := os.ReadFile(webStatePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return webState{Addr: defaultWebAddr}, false, nil
	}
	if err != nil {
		return webState{}, false, err
	}
	var st webState
	if err := json.Unmarshal(b, &st); err != nil {
		return webState{}, false, fmt.Errorf("%s: %w", webStatePath(home), err)
	}
	if st.Addr == "" {
		st.Addr = defaultWebAddr
	}
	return st, true, nil
}

func saveWebState(home string, st webState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(webStatePath(home), append(b, '\n'), 0o600)
}

// effectiveWeb is the listener a start gets: web.json when it exists, else
// the flag's address (on), else off. seeded: it came from the flag, and is
// to be saved so that later starts follow web.json.
func effectiveWeb(home, flagAddr string) (st webState, seeded bool, err error) {
	st, ok, err := loadWebState(home)
	if err != nil || ok {
		return st, false, err
	}
	if flagAddr != "" {
		return webState{Enabled: true, Addr: flagAddr}, true, nil
	}
	return st, false, nil
}

// loopbackOnly reports whether addr binds only the loopback interface.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// webURL is where a browser on this machine opens addr.
func webURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func tuiWarning(st webState) string {
	if st.Enabled && st.TUI && !loopbackOnly(st.Addr) {
		return "the TUI is served on " + st.Addr + ", which is not loopback-only: anyone who reaches it and signs in gets a shell as this user"
	}
	return ""
}

// webCtl owns the listener: one handler, built at start, served by a new
// http.Server each time the listener starts.
type webCtl struct {
	home  string
	h     http.Handler
	term  *webterm.Handler
	tuiOn atomic.Bool

	mu   sync.Mutex
	st   webState
	hs   *http.Server
	addr string // where hs listens; "" when it does not
}

// newWebCtl builds the HTTP surface for svc. The TUI at /tui runs bin, the
// dun engines run (`dun --tui` in $HOME: plain dun, attached to this dund
// through DUN_HOME; /resume opens its session picker).
func newWebCtl(home, bin string, svc *dund.Service, o dundapi.Options) (*webCtl, error) {
	c := &webCtl{home: home}
	userHome, _ := os.UserHomeDir()
	c.term = webterm.New(webterm.Config{
		Bin:  bin,
		Args: []string{"--tui", "--workspace", userHome, "--disable-exit"},
		Env:  tuiEnv(os.Environ(), home),
		Dir:  userHome,
		Back: "/?choose=1",
	})
	o.Owner = dundapi.Owner(os.Getuid())
	o.Term, o.TUIOn = c.term, c.tuiOn.Load
	h, err := dundapi.Handler(svc, o)
	if err != nil {
		return nil, err
	}
	c.h = h
	return c, nil
}

// tuiEnv is the TUI's environment: the service's, less its service manager
// and terminal (webterm.CleanEnv), with this dund's home, a terminal xterm.js
// is, and DUN_CHILD (a child never rebuilds itself).
func tuiEnv(env []string, home string) []string {
	out := []string{}
	for _, kv := range webterm.CleanEnv(env) {
		k, _, _ := strings.Cut(kv, "=")
		if k == "TERM" || k == "DUN_HOME" || k == "DUN_CHILD" || k == "COLORTERM" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "DUN_HOME="+home, "TERM=xterm-256color", "COLORTERM=truecolor", "DUN_CHILD=1")
}

// start applies st at dund's start. A listener that cannot start is said, not
// fatal: the saved state is changed with `dun dund enable web --addr`, which
// needs this dund running.
func (c *webCtl) start(st webState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.apply(st); err != nil {
		fmt.Fprintf(os.Stderr, "dund: web: %v\n", err)
	}
	c.st = st
}

// apply makes the listener match st; on an error it stays as it was. c.mu
// is held.
func (c *webCtl) apply(st webState) error {
	c.tuiOn.Store(st.TUI)
	if !st.TUI {
		c.term.KillAll()
	}
	if st.Enabled && c.hs != nil && st.Addr == c.addr {
		c.said(st)
		return nil
	}
	var hs *http.Server
	if st.Enabled { // the new one first: a port in use leaves the old one serving
		ln, err := net.Listen("tcp", st.Addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", st.Addr, err)
		}
		hs = &http.Server{Handler: c.h, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "dund: web: %v\n", err)
			}
		}()
	}
	if c.hs != nil {
		c.shutdown()
		fmt.Fprintf(os.Stderr, "dund: HTTP on %s stopped\n", c.addr)
	}
	c.hs, c.addr = hs, ""
	if hs != nil {
		c.addr = st.Addr
		fmt.Fprintf(os.Stderr, "dund: HTTP on %s\n", st.Addr)
	}
	c.said(st)
	return nil
}

func (c *webCtl) said(st webState) {
	if w := tuiWarning(st); w != "" {
		fmt.Fprintln(os.Stderr, "dund: warning:", w)
	}
}

// shutdown stops c.hs. Shutdown does not close hijacked connections, and
// /tui's WebSockets are hijacked: the ptys go first. c.mu is held.
func (c *webCtl) shutdown() {
	c.term.KillAll()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = c.hs.Shutdown(ctx)
	cancel()
}

// stop ends the listener and every pty for good: before dund exits, re-execs
// or hands over.
func (c *webCtl) stop() {
	c.term.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hs != nil {
		c.shutdown()
		c.hs, c.addr = nil, ""
	}
}

// set answers proto.WebSet: the change is applied, then saved.
func (c *webCtl) set(ws proto.WebSet) (proto.WebState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ws == (proto.WebSet{}) { // a question: nothing changes, nothing is saved
		return c.state(), nil
	}
	st, err := applyWebSet(c.st, ws)
	if err != nil {
		return proto.WebState{}, err
	}
	if err := c.apply(st); err != nil {
		return proto.WebState{}, err
	}
	c.st = st
	if err := saveWebState(c.home, st); err != nil {
		return proto.WebState{}, fmt.Errorf("applied, but not saved: %w", err)
	}
	return c.state(), nil
}

func (c *webCtl) state() proto.WebState {
	return wireState(c.st, c.hs != nil)
}

func wireState(st webState, listening bool) proto.WebState {
	out := proto.WebState{Enabled: st.Enabled, Addr: st.Addr, TUI: st.TUI, Listening: listening, Warning: tuiWarning(st)}
	if listening {
		out.URL = webURL(st.Addr)
	}
	return out
}

// applyWebSet is st with ws's changes.
func applyWebSet(st webState, ws proto.WebSet) (webState, error) {
	if ws.Addr != "" {
		if _, _, err := net.SplitHostPort(ws.Addr); err != nil {
			return st, fmt.Errorf("address %q: want host:port (e.g. %s)", ws.Addr, defaultWebAddr)
		}
		st.Addr = ws.Addr
	}
	if ws.Enable != nil {
		st.Enabled = *ws.Enable
	}
	if ws.TUI != nil {
		st.TUI = *ws.TUI
	}
	if st.Addr == "" {
		st.Addr = defaultWebAddr
	}
	return st, nil
}
