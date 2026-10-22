package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/proto"
)

// The TUI is a client of dund, the session service: it never runs an engine
// itself. It attaches to a session over dund's socket — starting dund if none
// is running — and reads the session's events from there, so the session
// outlives the TUI and any number of clients can follow it.

const (
	dundStartWait = 10 * time.Second
	ackWait       = 4 * time.Minute // a create waits for a new engine to name its session
)

// dialDund connects to the service, starting it when nothing listens.
func dialDund() (net.Conn, error) {
	sock := dundcli.Socket(dunHome())
	if c, err := net.Dial("unix", sock); err == nil {
		return c, nil
	}
	if err := startDund(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(dundStartWait)
	for {
		c, err := net.Dial("unix", sock)
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("dund did not come up at %s (see %s): %w", sock, dundLog(), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func dundLog() string { return dunHome() + "/dund.log" }

// startDund starts the service: the installed one (a systemd unit, or a
// launchd agent on macOS) when there is one for this DUN_HOME, so its flags,
// --http included, apply; else `dun dund serve` from this binary, in its own
// session so it outlives the TUI, output to dund.log.
func startDund() error {
	if dundcli.ServiceServes(dundcli.ServiceUnit, dunHome()) {
		return dundcli.StartService(dundcli.ServiceUnit)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dunHome(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(dundLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "dund", "serve", "--dun", exe)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(os.Environ(), "DUN_CHILD=1")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start dund: %w", err)
	}
	go func() { _ = cmd.Wait() }() // reap it if it exits while we run
	return nil
}

// engineArgs are the engine flags a session is created with: procArgs less
// what the service sets itself (workspace, resume, continue) and what it
// refuses (the key — the engine reads the saved one).
func engineArgs(o tuiOpts) []string {
	all := procArgs(o, "-p")[3:] // drop -p --workspace <ws>
	var out []string
	for i := 0; i < len(all); i++ {
		switch all[i] {
		case "--key", "--resume":
			i++
		case "--continue", "--disable-exit":
		default:
			out = append(out, all[i])
		}
	}
	return out
}

// attachSession attaches to the session o names, or creates one, and follows
// it from after seq after (0: from the start).
func attachSession(o tuiOpts, after uint64) (*dunProc, error) {
	c, err := dialDund()
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	ac := &attachConn{c: c}
	id := o.resume
	if id == "" && o.cont {
		id = dun.LatestSession(o.workspace)
	}
	if id == "" {
		ack, err := ac.call(sc, "", &proto.Create{Workspace: o.workspace, Args: engineArgs(o)})
		if err != nil {
			c.Close()
			return nil, err
		}
		id, after = ack.Session, 0
	}
	ac.session = id
	if _, err := ac.call(sc, id, &proto.Subscribe{After: after}); err != nil {
		if !strings.Contains(err.Error(), "no such session") {
			c.Close()
			return nil, err
		}
		// A conversation from before the service, or one it forgot: adopt it.
		if _, err := ac.call(sc, "", &proto.Create{Workspace: o.workspace, Args: engineArgs(o), Resume: id}); err != nil {
			c.Close()
			return nil, err
		}
		if _, err := ac.call(sc, id, &proto.Subscribe{After: 0}); err != nil {
			c.Close()
			return nil, err
		}
	}

	ch := make(chan tea.Msg, 256)
	tw := newTraceWriter() // DUN_TRACE=path — see replay.go
	p := &dunProc{stdin: ac, ch: ch, trace: tw, session: id, lastSeq: &atomic.Uint64{}}
	p.lastSeq.Store(after)
	// What arrived while waiting for the acks is the start of the stream.
	early := ac.early
	ac.early = nil
	go func() {
		defer tw.close()
		deliver := func(line []byte) bool {
			var ev map[string]any
			if json.Unmarshal(line, &ev) != nil {
				return true
			}
			if ev["type"] == "ack" {
				if e := str(ev["error"]); e != "" {
					if strings.HasPrefix(e, "dropped:") {
						return false // resubscribe from the last seq (engineGone)
					}
					ch <- evMsg{"type": "notification", "text": "dund: " + e}
				}
				return true
			}
			if s, _ := ev["session"].(string); s != "" && s != id {
				return true
			}
			if seq, ok := ev["seq"].(float64); ok {
				p.lastSeq.Store(uint64(seq))
			}
			tw.write(line)
			ch <- evMsg(ev)
			return true
		}
		for _, line := range early {
			if !deliver(line) {
				c.Close()
				ch <- eofMsg{proc: p}
				return
			}
		}
		for sc.Scan() {
			if !deliver(sc.Bytes()) {
				break
			}
		}
		c.Close()
		ch <- eofMsg{proc: p}
	}()
	return p, nil
}

// attachConn is the TUI's writer to the service. The TUI writes engine
// commands as JSON lines; each is sent with the session named.
type attachConn struct {
	c       net.Conn
	session string
	mu      sync.Mutex
	n       int
	early   [][]byte      // events read while waiting for an answer
	wait    time.Duration // how long call waits; 0 = ackWait
}

// Write takes one JSON command per call (json.Encoder writes one per
// Encode) and sends it framed for the session.
func (a *attachConn) Write(b []byte) (int, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, err
	}
	m["v"], m["session"] = proto.Version, a.session
	out, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.c.Write(append(out, '\n')); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (a *attachConn) Close() error { return a.c.Close() }

// call sends a command and reads until the event that answers it (an ack,
// or a reply such as a session list), keeping the events read on the way for
// the stream.
func (a *attachConn) call(sc *bufio.Scanner, session string, payload any) (proto.Event, error) {
	a.mu.Lock()
	a.n++
	cid := fmt.Sprintf("tui%d", a.n)
	cmd := proto.NewCommand(cid, payload)
	cmd.Session = session
	err := json.NewEncoder(a.c).Encode(cmd)
	a.mu.Unlock()
	if err != nil {
		return proto.Event{}, err
	}
	wait := a.wait
	if wait == 0 {
		wait = ackWait
	}
	_ = a.c.SetReadDeadline(time.Now().Add(wait))
	defer a.c.SetReadDeadline(time.Time{})
	for sc.Scan() {
		var ev proto.Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Re == cid {
			if ack, ok := ev.Payload.(*proto.Ack); ok && ack.Error != "" {
				return ev, errors.New(ack.Error)
			}
			return ev, nil
		}
		a.early = append(a.early, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		return proto.Event{}, err
	}
	return proto.Event{}, errors.New("dund closed the connection")
}

// restartWith asks the service to restart this session's engine with new
// flags (a model switch).
func (p *dunProc) restartWith(o tuiOpts) bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]any{"type": "restart", "args": engineArgs(o)}) == nil
}

// leave tells the service this client is exiting (/exit): it stops the
// engine unless the session persists or another client is attached.
func (p *dunProc) leave() bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]any{"type": "leave"}) == nil
}

// persist sets or asks the session's persist setting ("on", "off",
// "status", or "" — on if off, else status); a persist event answers.
func (p *dunProc) persist(mode string) bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]any{"type": "persist", "mode": mode}) == nil
}

// forget asks the service to drop this session (after /close deleted it).
func (p *dunProc) forget() bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]any{"type": "forget"}) == nil
}

// UI state: what this user's clients share in a session, kept by dund per
// member — the sent-input history (ctrl+↑/↓) and an unsent draft. Other
// members never see it.

const uiHistoryMax = 200

// uiGet asks for this user's UI state; it comes back as a ui_state event.
func (p *dunProc) uiGet() {
	if p == nil {
		return
	}
	_ = json.NewEncoder(p.stdin).Encode(map[string]any{"type": "ui_get"})
}

// saveUI stores the draft and the sent history.
func (m tuiModel) saveUI() {
	if m.proc == nil || m.replaying {
		return
	}
	hist := m.history
	if len(hist) > uiHistoryMax {
		hist = hist[len(hist)-uiHistoryMax:]
	}
	_ = json.NewEncoder(m.proc.stdin).Encode(map[string]any{"type": "ui_put",
		"state": map[string]any{"history": hist, "draft": m.input.Value()}})
}

// applyUI takes in UI state another of this user's clients saved: the
// history replaces ours when ours is empty or shorter, and the draft fills
// an empty input — never what is being typed.
func (m tuiModel) applyUI(v any) tuiModel {
	st, _ := v.(map[string]any)
	if hs, ok := st["history"].([]any); ok && len(hs) >= len(m.history) {
		m.history = make([]string, 0, len(hs)) // not [:0]: model copies share the array
		for _, h := range hs {
			if s, ok := h.(string); ok {
				m.history = append(m.history, s)
			}
		}
		m.histIdx = len(m.history)
	}
	if d, _ := st["draft"].(string); d != "" && strings.TrimSpace(m.input.Value()) == "" {
		m.input.SetValue(d)
	}
	return m
}

// dundSessions lists this user's sessions in dund, every workspace. It does
// not start dund: no service means no sessions beyond the files.
func dundSessions() ([]proto.SessionSummary, error) {
	c, err := net.DialTimeout("unix", dundcli.Socket(dunHome()), time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	ac := &attachConn{c: c, wait: 3 * time.Second}
	ev, err := ac.call(sc, "", &proto.ListSessions{})
	if err != nil {
		return nil, err
	}
	l, ok := ev.Payload.(*proto.SessionList)
	if !ok {
		return nil, fmt.Errorf("dund answered sessions with %s", ev.Type)
	}
	return l.Items, nil
}
