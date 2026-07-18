package main

import (
	"embed"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// dun serve — the bubbletea TUI in the browser via xterm.js. On a WebSocket
// connect the server spawns `dun -tui` in a pseudo-terminal and pipes raw bytes
// both ways, so the browser shows the REAL terminal: a resize sends the new size
// over the socket, the PTY delivers SIGWINCH, and bubbletea reflows exactly as
// in a terminal. Each browser gets its own session (use --continue to resume).
//
// Framing (browser → server, binary): first byte tags the message —
//   0x00 + bytes  = keystrokes → written to the PTY
//   0x01 + 4 bytes = resize (cols hi/lo, rows hi/lo) → pty.Setsize
// Server → browser: raw PTY output as binary frames.

//go:embed serveterm.html
var serveTermHTML []byte

//go:embed web
var termAssetsFS embed.FS

var wsUpgrader = websocket.Upgrader{
	// Local single-user dev tool; the listen address is the access control.
	CheckOrigin: func(*http.Request) bool { return true },
}

func runServe(o tuiOpts, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	for _, u := range reachableURLs(ln.Addr().String()) {
		log.Printf("dun serve → %s", u)
	}
	if !loopbackOnly(addr) {
		log.Print("⚠ bound to a non-loopback address — no auth; anyone who can reach it can drive this agent")
	}
	mux := http.NewServeMux()
	page := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(serveTermHTML)
	}
	mux.HandleFunc("/", page)
	mux.HandleFunc("/term", page)
	sub, _ := fs.Sub(termAssetsFS, "web")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	mux.HandleFunc("/term/ws", func(w http.ResponseWriter, r *http.Request) { termWS(w, r, o) })
	return http.Serve(ln, mux)
}

func termWS(w http.ResponseWriter, r *http.Request, o tuiOpts) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	exe, err := os.Executable()
	if err != nil {
		return
	}
	// Web sessions disable ctrl+c/esc quit — you leave by closing the tab (which
	// drops the socket and reaps the process group). Avoids an accidental ctrl+c
	// killing the session.
	o.disableExit = true
	cmd := exec.Command(exe, procArgs(o, "-tui")...)
	cmd.Env = append(os.Environ(), "DUN_CHILD=1") // spawned TUI never self-rebuilds
	ptmx, err := pty.Start(cmd) // pty.Start makes the child a session leader
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("pty error: "+err.Error()))
		return
	}
	// Kill the whole process group (dun -tui AND its dun -p child) on disconnect.
	defer func() {
		_ = ptmx.Close()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()

	// PTY → browser.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if conn.WriteMessage(websocket.BinaryMessage, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				_ = conn.Close() // unblock the reader below
				return
			}
		}
	}()

	// Browser → PTY (keystrokes + resize).
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case 0: // keystrokes
			if _, err := ptmx.Write(data[1:]); err != nil {
				return
			}
		case 1: // resize: cols hi/lo, rows hi/lo → SIGWINCH → bubbletea reflow
			if len(data) >= 5 {
				cols := uint16(data[1])<<8 | uint16(data[2])
				rows := uint16(data[3])<<8 | uint16(data[4])
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
			}
		}
	}
}

// reachableURLs turns a bound address into the URL(s) to open. For an all-
// interfaces bind (0.0.0.0 / :port / [::]) it lists the loopback plus each LAN
// IPv4, so a browser on another host knows where to point.
func reachableURLs(addr string) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return []string{"http://" + addr}
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{"http://" + net.JoinHostPort(host, port)}
	}
	urls := []string{"http://127.0.0.1:" + port}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualIface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip := ipn.IP.To4(); ip != nil && !ip.IsLoopback() {
					urls = append(urls, "http://"+ip.String()+":"+port)
				}
			}
		}
	}
	return urls
}

// virtualIface skips docker/bridge/vpn/virtual NICs, so the URL list is the
// real LAN address(es) a browser on another host would use — not a wall of
// 172.x bridge IPs.
func virtualIface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "tap", "tun", "cni", "flannel", "kube", "vmnet", "utun", "zt"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// loopbackOnly reports whether addr binds only the loopback interface.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
