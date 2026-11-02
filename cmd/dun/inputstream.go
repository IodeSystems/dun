package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/iodesystems/dun/proto"
)

// inputStream reads JSON events from stdin in a goroutine and routes them:
// user/stop → users, answer → answers. Decoupling the scanner from the turn loop
// lets an ask_user (blocked mid-turn) receive an answer.
//
// quit distinguishes the two ways users closes: an explicit stop/quit event
// closes it too, plain EOF does not. runProgrammatic drains background jobs on
// EOF but not on an explicit stop.
type inputStream struct {
	users   chan userMsg
	answers chan string
	quit    chan struct{}

	// mid routes a user message that arrived while a turn was in flight. The turn
	// loop is not selecting on users then, so without this the scanner blocks —
	// and blocking it also stalls the `answer` events an ask_user needs. Guarded
	// because it is set after the harness exists, while the scanner already runs.
	mu  sync.Mutex
	mid func(userMsg) bool
	// held is the messages with images that arrived during a turn (see
	// hold); heldCh says one is waiting.
	held   []userMsg
	heldCh chan struct{}
	// srv handles a `server` event (/rag, /lsp) inline, for the same reason as
	// mid: it must work while a turn is running.
	srv func(alias, action string)
	// srvPending holds server commands that arrived before the handler was
	// installed. The scanner starts before the harness exists, so a client that
	// writes its first line immediately would otherwise have it dropped.
	srvPending [][2]string
	// ctrl handles a `control` event (/docker, /worktree) inline.
	ctrl func(id, action string)
	// ctrlPending holds control commands that arrived before the handler was
	// installed.
	ctrlPending [][2]string
	// resetCb handles a `reset` event (clear the session store).
	resetCb func()
	// interruptCb handles an `interrupt` event (stop the turn in flight).
	interruptCb func(reason string)
	// agentFn handles an `agent` event: the human viewing a child's transcript
	// or telling it something. Same install-after-the-harness-exists problem as
	// srv and ctrl, so it queues the same way.
	agentFn      func(id, action, content string)
	agentPending [][3]string
	// peerFn handles dund's peer frames (peers.go): a reply to this engine's
	// dun_list/dun_msg, or another session's message. Inline: a reply is
	// what a running tool call waits on. Queued until installed, like the
	// others.
	peerFn      func(typ string, line []byte)
	peerPending []peerFrame
}
type peerFrame struct {
	typ  string
	line []byte
}

// peerCmd runs the installed peer handler, queueing until one exists.
func (s *inputStream) peerCmd(typ string, line []byte) {
	s.mu.Lock()
	fn := s.peerFn
	if fn == nil {
		s.peerPending = append(s.peerPending, peerFrame{typ, line})
	}
	s.mu.Unlock()
	if fn != nil {
		fn(typ, line)
	}
}

// setPeerFn installs the handler and drains whatever arrived first.
func (s *inputStream) setPeerFn(fn func(typ string, line []byte)) {
	s.mu.Lock()
	s.peerFn = fn
	pending := s.peerPending
	s.peerPending = nil
	s.mu.Unlock()
	for _, p := range pending {
		fn(p.typ, p.line)
	}
}

// setMidTurn installs the mid-turn router (see inputStream.mid).
func (s *inputStream) setMidTurn(f func(userMsg) bool) {
	s.mu.Lock()
	s.mid = f
	s.mu.Unlock()
}

// setServerCmd installs the server-command handler (see inputStream.srv) and
// replays anything that arrived before it existed.
func (s *inputStream) setServerCmd(f func(alias, action string)) {
	s.mu.Lock()
	s.srv = f
	queued := s.srvPending
	s.srvPending = nil
	s.mu.Unlock()
	for _, c := range queued {
		f(c[0], c[1])
	}
}

// setCtrlCmd installs the control-command handler (see inputStream.ctrl) and
// replays anything that arrived before it existed.
func (s *inputStream) setCtrlCmd(f func(id, action string)) {
	s.mu.Lock()
	s.ctrl = f
	queued := s.ctrlPending
	s.ctrlPending = nil
	s.mu.Unlock()
	for _, c := range queued {
		f(c[0], c[1])
	}
}

// setInterrupt installs the interrupt handler.
func (s *inputStream) setInterrupt(f func(reason string)) {
	s.mu.Lock()
	s.interruptCb = f
	s.mu.Unlock()
}

// setResetCb installs the session-reset handler.
func (s *inputStream) setResetCb(f func()) {
	s.mu.Lock()
	s.resetCb = f
	s.mu.Unlock()
}

// agentCmd runs the installed agent handler, queueing until one exists.
func (s *inputStream) agentCmd(id, action, content string) {
	s.mu.Lock()
	fn := s.agentFn
	if fn == nil {
		s.agentPending = append(s.agentPending, [3]string{id, action, content})
	}
	s.mu.Unlock()
	if fn != nil {
		fn(id, action, content)
	}
}

// setAgentFn installs the handler and drains whatever arrived first.
func (s *inputStream) setAgentFn(fn func(id, action, content string)) {
	s.mu.Lock()
	s.agentFn = fn
	pending := s.agentPending
	s.agentPending = nil
	s.mu.Unlock()
	for _, p := range pending {
		fn(p[0], p[1], p[2])
	}
}

// serverCmd runs the installed handler, queueing until one exists.
func (s *inputStream) serverCmd(alias, action string) {
	s.mu.Lock()
	f := s.srv
	if f == nil {
		s.srvPending = append(s.srvPending, [2]string{alias, action})
	}
	s.mu.Unlock()
	if f != nil {
		f(alias, action)
	}
}

// ctrlCmd runs the installed control handler, queueing until one exists.
func (s *inputStream) ctrlCmd(id, action string) {
	s.mu.Lock()
	f := s.ctrl
	if f == nil {
		s.ctrlPending = append(s.ctrlPending, [2]string{id, action})
	}
	s.mu.Unlock()
	if f != nil {
		f(id, action)
	}
}

// midTurn offers text to the router, reporting whether it took it.
func (s *inputStream) midTurn(msg userMsg) bool {
	s.mu.Lock()
	f := s.mid
	s.mu.Unlock()
	return f != nil && f(msg)
}

// userMsg is one user message off the input: its text and attached images.
type userMsg struct {
	content string
	images  []proto.Image
}

// hold keeps a message for the turn after the running one. A message with
// images cannot be lifted into a tool result, and the scanner cannot wait
// for the loop to take it (the running turn's answers and interrupts come
// through the scanner).
func (s *inputStream) hold(msg userMsg) {
	s.mu.Lock()
	s.held = append(s.held, msg)
	s.mu.Unlock()
	select {
	case s.heldCh <- struct{}{}:
	default:
	}
}

// takeHeld returns the oldest held message.
func (s *inputStream) takeHeld() (userMsg, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.held) == 0 {
		return userMsg{}, false
	}
	m := s.held[0]
	s.held = s.held[1:]
	return m, true
}

// stopped reports whether users closed because of an explicit stop/quit event
// rather than EOF. Safe to call only after users is closed: the scanner closes
// quit first, so the happens-before is via that close.
func (s *inputStream) stopped() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}
func newInputStream() *inputStream { return newInputStreamFrom(os.Stdin) }

// newInputStreamFrom is newInputStream over an arbitrary reader, so a test can
// drive the event parsing and the EOF-vs-stop distinction without a real stdin.
func newInputStreamFrom(r io.Reader) *inputStream {
	s := &inputStream{users: make(chan userMsg), answers: make(chan string), quit: make(chan struct{}),
		heldCh: make(chan struct{}, 1)}
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var ev struct {
				Type    string `json:"type"`
				Content string `json:"content"`
				Value   string `json:"value"`
				ID      string `json:"id"`     // server/control: which target
				Action  string `json:"action"` // server/control: action to take
				Reason  string `json:"reason"` // interrupt: what stopped the turn ("" = the user)
				// user: attached images (images.go)
				Images []proto.Image `json:"images"`
			}
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			switch ev.Type {
			case "server":
				// Handled inline (see inputStream.srv): a turn may be running,
				// and this loop is also the only reader of `answer` events.
				s.serverCmd(ev.ID, ev.Action)
			case "control":
				// Handled inline (see inputStream.ctrl): docker/worktree toggles.
				s.ctrlCmd(ev.ID, ev.Action)
			case "agent":
				// The human looking at, or steering, a sub-agent. Inline for the
				// same reason as the others: a turn may be running, and this
				// loop is the only reader of `answer` events.
				s.agentCmd(ev.ID, ev.Action, ev.Content)
			case "user":
				msg := userMsg{content: ev.Content, images: ev.Images}
				if s.midTurn(msg) {
					continue // buffered into the running turn, or held for the next
				}
				s.users <- msg
			case "answer":
				s.answers <- ev.Value
			case "peer", "peer_reply", "servers", "service_notice":
				// Never an answer: a peer message cannot close an open
				// ask_user, whatever it says.
				s.peerCmd(ev.Type, []byte(line))
			case "interrupt":
				// Inline: the turn it stops is what blocks the loop.
				s.mu.Lock()
				cb := s.interruptCb
				s.mu.Unlock()
				if cb != nil {
					cb(ev.Reason)
				}
			case "reset":
				s.mu.Lock()
				cb := s.resetCb
				s.mu.Unlock()
				if cb != nil {
					cb()
				}
			case "stop", "quit":
				close(s.quit) // before users, so stopped() is visible to the reader
				close(s.users)
				return
			}
		}
		close(s.users) // EOF: runProgrammatic drains background jobs before exiting
	}()
	return s
}
