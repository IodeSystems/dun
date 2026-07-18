package main

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// Terminal view (option B) — the ACTUAL bubbletea TUI in the browser via
// xterm.js. `dun serve` also serves this at /term: on a WebSocket connect it
// spawns `dun -tui` in a pseudo-terminal and pipes raw bytes both ways. Unlike
// the web-native view (option A, HTML over the -p event stream), this is the
// real terminal grid — so a browser resize sends the new size over the socket,
// the PTY delivers SIGWINCH, and bubbletea REFLOWS exactly as in a terminal.
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

// addTermRoutes mounts the xterm page, its assets, and the PTY WebSocket. o is
// the shared flags so the spawned `dun -tui` matches `dun serve`'s workspace/
// model/etc.
func addTermRoutes(mux *http.ServeMux, o tuiOpts) {
	mux.HandleFunc("/term", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(serveTermHTML)
	})
	sub, _ := fs.Sub(termAssetsFS, "web")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	mux.HandleFunc("/term/ws", func(w http.ResponseWriter, r *http.Request) { termWS(w, r, o) })
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
	cmd := exec.Command(exe, procArgs(o, "-tui")...)
	cmd.Env = os.Environ()
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
