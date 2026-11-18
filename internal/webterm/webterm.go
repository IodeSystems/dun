// Package webterm is a terminal program in a browser tab: xterm.js on the
// page, a pty on the server, raw bytes both ways. A resize sends the new size
// over the socket, the pty delivers SIGWINCH, and the program (the dun TUI)
// reflows as it does in a terminal. Each WebSocket gets its own process;
// closing the tab kills its process group.
//
// The handler serves, relative to where it is mounted:
//
//	/           the page (term.html)
//	/static/... xterm.js and its stylesheet
//	/ws         the WebSocket
//
// Framing (browser → server, binary): the first byte tags the message —
//
//	0x00 + bytes   keystrokes, written to the pty
//	0x01 + 4 bytes resize (cols hi/lo, rows hi/lo), pty.Setsize
//
// Server → browser: raw pty output as binary frames.
package webterm

import (
	"embed"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

//go:embed term.html
var page string

//go:embed static
var assets embed.FS

// Config is the program each connection runs, and how the page and the
// socket behave.
type Config struct {
	Bin  string
	Args []string
	Env  []string // the whole environment; see CleanEnv
	Dir  string   // working directory; "" is the server's
	// Back, when set, is a link on the page back to where the user came from
	// (dund's chooser).
	Back string
	// Allow, when set, is asked again as each pty registers, under the
	// lock KillAll takes: a connection that passed the caller's own gate
	// just before it closed (and KillAll ran) is killed rather than missed.
	Allow func() bool
	// CheckOrigin decides whether a WebSocket upgrade is from this site. Nil
	// is SameOrigin. Never a function that answers true: any page the user
	// visits could then open a shell here (cross-site WebSocket hijacking).
	CheckOrigin func(*http.Request) bool
}

// Handler serves the page, its assets and the socket, and keeps the live
// ptys so they can be killed together.
type Handler struct {
	cfg  Config
	up   websocket.Upgrader
	mux  *http.ServeMux
	page []byte

	mu     sync.Mutex
	live   map[*term]struct{}
	closed bool
}

type term struct {
	cmd  *exec.Cmd
	conn *websocket.Conn
	once sync.Once
}

// kill ends the process group and closes the socket. A hijacked connection
// is not closed by http.Server.Shutdown, so this is what ends it.
func (t *term) kill() {
	t.once.Do(func() {
		if t.cmd.Process != nil {
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = t.conn.Close()
	})
}

// New makes the handler for cfg.
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), live: map[*term]struct{}{}}
	check := cfg.CheckOrigin
	if check == nil {
		check = SameOrigin
	}
	h.up = websocket.Upgrader{CheckOrigin: check}
	back := ""
	if cfg.Back != "" {
		back = ` · <a href="` + html.EscapeString(cfg.Back) + `">back</a>`
	}
	h.page = []byte(strings.Replace(page, "{{BACK}}", back, 1))
	sub, _ := fs.Sub(assets, "static")
	h.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	h.mux.HandleFunc("/ws", h.ws)
	h.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(h.page)
	})
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// Active is how many ptys are live.
func (h *Handler) Active() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.live)
}

// KillAll kills every live pty's process group and closes its socket. New
// connections are still taken.
//
// Callers that gate connections themselves turn the gate off first and set
// Config.Allow to it: a pty registers under h.mu after asking Allow, so it
// is either in the set taken here or refused at registration.
func (h *Handler) KillAll() {
	h.mu.Lock()
	all := make([]*term, 0, len(h.live))
	for t := range h.live {
		all = append(all, t)
	}
	h.mu.Unlock()
	for _, t := range all {
		t.kill()
	}
}

// Close is KillAll, and refuses connections from now on: before the server
// stops or re-execs.
func (h *Handler) Close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.KillAll()
}

// SameOrigin reports whether r's Origin names the host r was sent to. A
// request with no Origin is refused: every browser sends one on a
// WebSocket. Behind a proxy the caller passes its own check
// (dundapi.OriginCheck, which believes X-Forwarded-Host from trusted peers
// only).
func SameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// CleanEnv is env without what belongs to the server's own terminal or
// service manager: tmux and terminal-program variables (the browser's
// terminal is neither, so the TUI marks no tmux window and sends no iTerm2
// escapes), and systemd's and launchd's markers (a TUI that thinks a service
// manager started it behaves as a service would).
func CleanEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "TMUX", "TMUX_PANE", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "LC_TERMINAL", "LC_TERMINAL_VERSION",
			"INVOCATION_ID", "JOURNAL_STREAM", "NOTIFY_SOCKET", "LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES",
			"MANAGERPID", "SYSTEMD_EXEC_PID", "XPC_SERVICE_NAME", "XPC_FLAGS":
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (h *Handler) ws(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		http.Error(w, "stopping", http.StatusServiceUnavailable)
		return
	}
	conn, err := h.up.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader wrote the refusal (403 for a foreign origin)
	}
	cmd := exec.Command(h.cfg.Bin, h.cfg.Args...)
	cmd.Env, cmd.Dir = h.cfg.Env, h.cfg.Dir
	ptmx, err := pty.Start(cmd) // the child is a session leader: its pid is its group
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("pty error: "+err.Error()))
		_ = conn.Close()
		return
	}
	t := &term{cmd: cmd, conn: conn}
	h.mu.Lock()
	if h.closed || h.cfg.Allow != nil && !h.cfg.Allow() {
		h.mu.Unlock()
		t.kill()
		_ = ptmx.Close()
		_ = cmd.Wait()
		return
	}
	h.live[t] = struct{}{}
	h.mu.Unlock()
	defer func() {
		t.kill()
		_ = ptmx.Close()
		_ = cmd.Wait()
		h.mu.Lock()
		delete(h.live, t)
		h.mu.Unlock()
	}()

	go func() { // pty → browser
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 && conn.WriteMessage(websocket.BinaryMessage, buf[:n]) != nil {
				break
			}
			if err != nil {
				break
			}
		}
		_ = conn.Close() // unblocks the reader below
	}()

	for { // browser → pty
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case 0:
			if _, err := ptmx.Write(data[1:]); err != nil {
				return
			}
		case 1:
			if len(data) >= 5 {
				cols := uint16(data[1])<<8 | uint16(data[2])
				rows := uint16(data[3])<<8 | uint16(data[4])
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
			}
		}
	}
}
