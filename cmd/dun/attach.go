package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
		case "--continue", "--disable-exit", "--new":
		default:
			out = append(out, all[i])
		}
	}
	return out
}

// resumeAt is where a client is in a session's events: the last seq it saw
// and the epoch that numbered it (see proto.Event). After dund restarts its
// seqs start again from 1 under a new epoch, so a seq alone could land a
// reattach in the middle of the new numbering.
type resumeAt struct {
	epoch string
	seq   uint64
}

// attachSession attaches to the session o names, or creates one, and follows
// it from after after (the zero value: from the start).
func attachSession(o tuiOpts, after resumeAt) (*dunProc, error) {
	c, err := dialDund()
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	ac := &attachConn{c: c}
	notes := checkDundBuild(ac, sc)
	id := o.resume
	if id == "" && o.cont {
		id = dun.LatestSession(o.workspace)
	}
	reused := false
	if id == "" {
		// A plain dun takes the workspace's newest session nobody is
		// attached to, so sessions opened and left do not pile up; dund
		// picks, so two TUIs starting at once do not share one.
		ack, err := ac.call(sc, "", &proto.Create{Workspace: o.workspace, Args: engineArgs(o), ReuseIdle: !o.fresh})
		if err != nil {
			c.Close()
			return nil, err
		}
		id, after = ack.Session, resumeAt{}
		if a, ok := ack.Payload.(*proto.Ack); ok {
			reused = a.Reused
		}
	}
	ac.session = id
	if _, err := ac.call(sc, id, &proto.Subscribe{After: after.seq, Epoch: after.epoch}); err != nil {
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
		after = resumeAt{}
	}

	ch := make(chan tea.Msg, 256)
	tw := newTraceWriter() // DUN_TRACE=path — see replay.go
	p := &dunProc{stdin: ac, ch: ch, trace: tw, session: id}
	p.at.Store(&after)
	for _, n := range notes {
		ch <- evMsg{"type": "notification", "text": n}
	}
	if reused {
		ch <- evMsg{"type": "reused", "text": reusedLine(o.workspace, id)}
	}
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
			// A snapshot's after is the seq it brings this client up to.
			if seq, ok := ev["seq"].(float64); ok {
				p.at.Store(&resumeAt{epoch: str(ev["epoch"]), seq: uint64(seq)})
			} else if a, ok := ev["after"].(float64); ok && ev["type"] == "snapshot" {
				p.at.Store(&resumeAt{epoch: str(ev["epoch"]), seq: uint64(a)})
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

// reusedLine says which session a plain dun resumed, and how to get a new
// one instead.
func reusedLine(workspace, id string) string {
	n := 0
	for _, it := range dun.SessionHistory(dun.SessionFile(workspace, id)) {
		if it.Kind == "user" || it.Kind == "assistant" {
			n++
		}
	}
	age := "nothing saved yet"
	if st, err := os.Stat(dun.SessionFile(workspace, id)); err == nil {
		age = relTime(st.ModTime())
	}
	unit := "messages"
	if n == 1 {
		unit = "message"
	}
	return fmt.Sprintf("resumed %s (%s, %d %s) — /new for a fresh session", id, age, n, unit)
}

// checkDundBuild compares dund's build with this one. A client-started dund
// of another build is asked to restart into this binary (it does once nothing
// is busy, and the reconnect carries this TUI across); a service is only
// reported; a dund too old to answer gets the command that replaces it.
func checkDundBuild(ac *attachConn, sc *bufio.Scanner) []string {
	ev, err := ac.call(sc, "", &proto.Hello{Version: version})
	if err != nil {
		return []string{"dund is an older build that cannot be upgraded from here — " +
			"stop it (kill $(cat " + filepath.Join(dunHome(), "dund.pid") + ")) and start dun again; sessions are kept"}
	}
	r, ok := ev.Payload.(*proto.HelloReply)
	if !ok || r.Version == version || r.Version == "dev" || version == "dev" {
		return nil
	}
	if !r.Upgradable {
		return []string{"dund (a service) runs build " + r.Version + ", this is " + version + " — dun dund service restart"}
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return []string{"dund runs build " + r.Version + ", this is " + version + "; cannot name this binary to upgrade it: " + err.Error()}
	}
	// Once per build pair: a dund that comes back as the same old build after
	// an upgrade was handed a binary that is not this build, and asking again
	// is a loop of restarts. Say what was tried instead.
	pair := r.Version + "→" + version
	if upgradeAsked[pair] && r.Busy {
		return nil // asked already; it is waiting for the running work to end
	}
	if upgradeAsked[pair] {
		return []string{"dund still runs build " + r.Version + " after restarting into " + exe +
			" — that file is not build " + version + " (a stale copy?): stop dund (kill $(cat " +
			filepath.Join(dunHome(), "dund.pid") + ")) and run dun from the build you want"}
	}
	upgradeAsked[pair] = true
	if _, err := ac.call(sc, "", &proto.Upgrade{Exe: exe}); err != nil {
		return []string{"dund runs build " + r.Version + ", this is " + version + "; it would not upgrade: " + err.Error()}
	}
	if r.Busy {
		return []string{"dund runs build " + r.Version + " and is busy (a turn, a question or a job is running): it restarts into " +
			version + " when that ends — ctrl+k interrupts a turn, /jobs kills a job"}
	}
	return []string{"dund ran build " + r.Version + " — restarting it into " + exe + " (" + version + "); this reconnects"}
}

// upgradeAsked records the build pairs this TUI already asked dund to move
// between, so a failed upgrade is reported instead of retried forever.
var upgradeAsked = map[string]bool{}

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
		// An error answering no command is dund saying why it is about to
		// hang up (e.g. it cannot tell who is connecting).
		if ack, ok := ev.Payload.(*proto.Ack); ok && ev.Re == "" && ack.Error != "" {
			return ev, errors.New(ack.Error)
		}
		a.early = append(a.early, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		return proto.Event{}, err
	}
	return proto.Event{}, errors.New("dund closed the connection (if it crashed, the reason is in " + dundLog() + ")")
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
