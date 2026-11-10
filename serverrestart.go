package dun

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/dun/internal/update"
)

// A local stdio server that exits during a call stays dead for the rest of
// the session unless something starts it again: every later call fails at
// once with "transport closed" (a macOS session, dun v0.2.8: an mcpshell
// that died in one eval made every eval after it fail, even 1+1, and the
// model gave up on eval). dun restarts such a server once per failed call,
// and does NOT repeat the call: it may be what killed the server. The
// model is told the server exited, that it was restarted, that its state
// is gone, and what the server said on stderr last.
//
// A server that keeps exiting is not restarted forever: past restartLimit
// in restartWindow, dun says so, names the binary and its version, and
// stops it.

// restartLimit restarts of one server within restartWindow, then no more.
// Vars for the tests.
var (
	restartLimit  = 3
	restartWindow = 10 * time.Minute
	// stderrSettle is how long a restart waits for the dead process's
	// stderr to arrive (it races the transport error).
	stderrSettle = 300 * time.Millisecond
)

// restartLog is a harness's restarts, by server id.
type restartLog struct {
	mu     sync.Mutex
	at     map[string][]time.Time // restarts within restartWindow
	done   map[string]time.Time   // when the last restart (or give-up) finished
	last   map[string]string      // what the last restart told the model
	gaveUp map[string]string      // the message for a server no longer restarted
}

// regain answers a failed call (mcpDispatcher): a shared server the
// gateway dropped is started again and the call repeated; a local one that
// exited is restarted and the model told (no repeat).
func (h *Harness) regain(ctx context.Context, id string, err error) (bool, string) {
	if h.regainShared(ctx, id, err) {
		return true, ""
	}
	return false, h.restartExited(ctx, id, err)
}

// serverExited reports whether a call failed because the server's process
// is gone: agentkit's stdio transport says "mcp: transport closed" once its
// read loop hit EOF, and a write to the dead process's stdin fails with a
// broken or closed pipe. agentkit's error is unexported, so it is matched
// by its text.
func serverExited(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if strings.Contains(s, "mcp: transport closed") {
		return true
	}
	return strings.Contains(s, "write request:") &&
		(strings.Contains(s, "broken pipe") || strings.Contains(s, "closed pipe") || strings.Contains(s, "file already closed"))
}

// restartOwner is the harness that runs server id: a child's own instance
// is the child's; every other server, a child's included, the root's (a
// child's manager is its parent's, down to the root).
func (h *Harness) restartOwner(id string) *Harness {
	if h.ownMgr != nil && h.own[id] {
		return h
	}
	r := h
	for r.parent != nil {
		r = r.parent
	}
	return r
}

// restartExited restarts the local stdio server id after a call that failed
// because its process exited, and returns what the model reads; "" when the
// failure is not that (the caller reports the error as it is).
func (h *Harness) restartExited(ctx context.Context, id string, callErr error) string {
	if callErr == nil || ctx.Err() != nil {
		return ""
	}
	at := time.Now()
	o := h.restartOwner(id)
	o.srvMu.Lock()
	s, known := o.spec(id)
	_, shared := o.shared[id]
	o.srvMu.Unlock()
	if !known || shared || s.URL != "" || s.Via == ViaShell || s.Command == "" {
		return ""
	}
	r := &o.restarts
	r.mu.Lock()
	defer r.mu.Unlock()
	if msg, ok := r.gaveUp[id]; ok && (serverExited(callErr) || strings.Contains(callErr.Error(), "not connected")) {
		return msg
	}
	if !serverExited(callErr) {
		return ""
	}
	// A call that failed on the same dead process while another call's
	// restart ran: that restart replaced it.
	if d, ok := r.done[id]; ok && d.After(at) {
		return r.last[id]
	}
	mgr := o.mgrFor(id)
	if !mgr.ServerStarted(id) {
		return "" // stopped on purpose (StopServer) while the call ran
	}
	if r.at == nil {
		r.at, r.done, r.last, r.gaveUp = map[string][]time.Time{}, map[string]time.Time{}, map[string]string{}, map[string]string{}
	}
	var recent []time.Time
	for _, t := range r.at[id] {
		if at.Sub(t) < restartWindow {
			recent = append(recent, t)
		}
	}
	tail := stderrTail(mgr, id)
	if len(recent) >= restartLimit {
		path, version := serverBinary(mgr, id, s.Command)
		msg := fmt.Sprintf("ERROR: the %s server exited during this call. It exited %d times in %s, so dun no longer restarts it. "+
			"Binary: %s, version %s.", id, len(recent)+1, restartWindow, path, version)
		if tail != "" {
			msg += "\nserver stderr (last lines):\n" + tail
		}
		r.gaveUp[id], r.done[id], r.last[id] = msg, time.Now(), msg
		r.at[id] = recent
		mgr.StopServer(id)
		o.srvMu.Lock()
		o.lastErr[id] = fmt.Sprintf("exited %d times in %s; not restarted (%s, %s)", len(recent)+1, restartWindow, path, version)
		o.srvMu.Unlock()
		o.noteServer(id, "exited during a call again (%d times in %s): not restarting it", len(recent)+1, restartWindow)
		log.Printf("dun: %s exited during a call again (%d times in %s); not restarting it (%s, %s)", id, len(recent)+1, restartWindow, path, version)
		o.applyTools()
		return msg
	}
	r.at[id] = append(recent, at)
	n := len(r.at[id])
	o.noteServer(id, "exited during a call: restarting it (restart %d of %d in %s)", n, restartLimit, restartWindow)
	log.Printf("dun: %s exited during a call; restarting it (restart %d of %d in %s)", id, n, restartLimit, restartWindow)
	mgr.StopServer(id)
	state := "Its state from earlier calls is gone."
	if canonicalServerID(id) == ServerShell {
		state = "Its state from earlier calls is gone (session variables, loaded toolkits)."
	}
	var msg string
	if err := o.startServer(ctx, s); err != nil {
		msg = fmt.Sprintf("ERROR: the %s server exited during this call, and restarting it failed: %s", id, oneLine(err.Error(), 300))
	} else {
		msg = fmt.Sprintf("ERROR: the %s server exited during this call. dun restarted it; the call was not run again. %s "+
			"Retry, in smaller pieces if the call was large.", id, state)
	}
	if tail != "" {
		msg += "\nserver stderr (last lines):\n" + tail
	}
	r.done[id], r.last[id] = time.Now(), msg
	return msg
}

// stderrTail is the last few lines the dead server id wrote to stderr,
// once they stop arriving (at most stderrSettle).
func stderrTail(mgr *mcpmgr.Manager, id string) string {
	const lines, width = 5, 200
	deadline := time.Now().Add(stderrSettle)
	prev := mgr.ServerStderr(id)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		cur := mgr.ServerStderr(id)
		if cur == prev && cur != "" {
			break
		}
		prev = cur
	}
	all := strings.Split(strings.TrimSpace(prev), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	for i, l := range all {
		all[i] = oneLine(l, width)
	}
	return strings.TrimSpace(strings.Join(all, "\n"))
}

// serverBinary is the path and version of the binary server id runs: the
// version it gave in the handshake, else its build info's.
func serverBinary(mgr *mcpmgr.Manager, id, command string) (path, version string) {
	path, err := FindCommand(command)
	if err != nil {
		path = command
	}
	if info, ok := mgr.ServerInfo(id); ok && info.Version != "" {
		return path, info.Version
	}
	if o, err := update.OfFile(path); err == nil && o.Version != "" {
		return path, o.Version
	}
	return path, "unknown"
}
