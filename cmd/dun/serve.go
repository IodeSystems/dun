package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// dun serve — a WEB client of the `-p` protocol. It spawns ONE `dun -p` engine
// and bridges it to the browser: SSE (/events) streams the engine's
// line-delimited JSON events downstream; POST /input forwards a
// {type:user|answer|stop,...} event to the engine's stdin. This is the exact
// seam the Bubble Tea TUI uses (startDunProc) — the browser is just a second
// client, so the engine is untouched. Single session per server: dun is a
// local, single-user tool. A (re)connecting browser replays the event history,
// so a reload rebuilds the whole transcript.

//go:embed serve.html
var serveHTML []byte

type serveHub struct {
	stdin io.Writer
	mu    sync.Mutex // guards stdin writes, subs, and hist together
	subs  map[chan string]struct{}
	hist  []string // every event so far, replayed to a new subscriber
}

func runServe(o tuiOpts, addr string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, procArgs(o, "-p")...)
	cmd.Env = append(os.Environ(), "DUN_CHILD=1") // spawned engine never self-rebuilds
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// Engine stderr (mcp startup logs) → a temp file so it doesn't mix with ours.
	if f, ferr := os.CreateTemp("", "dun-serve-*.log"); ferr == nil {
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = stdin.(io.Closer).Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	hub := &serveHub{stdin: stdin, subs: map[chan string]struct{}{}}
	go hub.pump(stdout)

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
	mux := serveMux(hub)     // option A: web-native over the -p event stream
	addTermRoutes(mux, o)     // option B: the real TUI via xterm.js + PTY at /term
	return http.Serve(ln, mux)
}

// serveMux wires the three routes (page, SSE, input) — shared by `dun serve` and
// the TUI's embedded `/web` server.
func serveMux(hub *serveHub) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(serveHTML)
	})
	mux.HandleFunc("/events", hub.events)
	mux.HandleFunc("/input", hub.input)
	return mux
}

// lockedWriter serializes writes to a shared engine stdin (the TUI and the
// embedded web hub both write to the same pipe).
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// startEmbeddedWeb starts a server (for the TUI's `/web`) that mirrors the LIVE
// session: `write` is the engine stdin (caller lock-guards it). The caller wires
// the returned hub's broadcast to the engine's event stream (proc.tap).
func startEmbeddedWeb(addr string, write io.Writer) (*serveHub, string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}
	hub := &serveHub{stdin: write, subs: map[chan string]struct{}{}}
	go func() { _ = http.Serve(ln, serveMux(hub)) }()
	return hub, ln.Addr().String(), nil
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

// pump scans the engine's stdout and fans each JSON line to subscribers + hist.
// A final synthetic {"type":"eof"} tells the browser the engine exited.
func (h *serveHub) pump(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if line := sc.Text(); strings.TrimSpace(line) != "" {
			h.broadcast(line)
		}
	}
	h.broadcast(`{"type":"eof"}`)
}

func (h *serveHub) broadcast(line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hist = append(h.hist, line)
	for ch := range h.subs {
		select {
		case ch <- line:
		default:
			// A wedged/slow client: drop it rather than stall the engine. It can
			// reconnect and replay hist.
			delete(h.subs, ch)
			close(ch)
		}
	}
}

func (h *serveHub) subscribe() (chan string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan string, 1024)
	h.subs[ch] = struct{}{}
	return ch, append([]string(nil), h.hist...)
}

func (h *serveHub) unsubscribe(ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

// events is the SSE endpoint: replay the history, then stream live events.
func (h *serveHub) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, hist := h.subscribe()
	defer h.unsubscribe(ch)
	for _, l := range hist {
		fmt.Fprintf(w, "data: %s\n\n", l)
	}
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case l, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", l)
			fl.Flush()
		}
	}
}

// input forwards a browser event to the engine's stdin as one JSON line. Only
// the IN event types the engine understands are accepted.
func (h *serveHub) input(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(body, &ev) != nil ||
		(ev.Type != "user" && ev.Type != "answer" && ev.Type != "stop") {
		http.Error(w, `expected {"type":"user|answer|stop",...}`, http.StatusBadRequest)
		return
	}
	// Compact to a single line (the engine's stdin scanner is line-delimited).
	var line bytes.Buffer
	if err := json.Compact(&line, body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	line.WriteByte('\n')

	h.mu.Lock()
	_, err = h.stdin.Write(line.Bytes())
	h.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
