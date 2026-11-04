package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/tui"
	"github.com/iodesystems/dun/proto"
)

// `dun mcp`: dun's sessions as an MCP server on stdio (USER 2026-11-04:
// "interface claude<->dun for making things cheaper"). Another agent —
// Claude Code, or anything that speaks MCP — starts dun sessions, sends
// them work, and reads their replies, through dund's unix socket, as the
// TUI does. The work runs on the session's model, not the caller's.
//
// The protocol is MCP's JSON-RPC 2.0, one message per line: initialize,
// tools/list, tools/call, ping. Written here rather than taken from a
// library: these four methods are all of it.

const mcpHelp = `dun mcp — dun's sessions as an MCP server, on stdin/stdout

Another agent (Claude Code, or any MCP client) starts dun sessions, gives
them work and reads the replies. The work runs on the dun session's model
and provider, so a costly agent can hand the long, cheap part of a job to
dun. It talks to dund over its unix socket, as the TUI does, and starts
dund when nothing is listening. Sessions it starts are ordinary dun
sessions: dun --resume <id> or the session list opens them.

Add it to Claude Code:

  claude mcp add dun -- dun mcp

or in an MCP client's config:

  {"mcpServers": {"dun": {"command": "dun", "args": ["mcp"]}}}

Tools:

  dun_sessions   your dun sessions: id, workspace, running, last event
  dun_start      start a session in a workspace with a first message;
                 optional model and provider (a provider you logged in to
                 with /provider login); waits for the reply
  dun_send       send a message to a session; waits for its reply
  dun_wait       wait for a session's next reply, from an event number
  dun_answer     answer a question the session asked (ask_user)
  dun_interrupt  stop the session's running turn

A reply ends one of four ways: done (the reply), asking (a question and
its options: answer with dun_answer), error, or still working when the
wait ran out (timeout_s, default 600): call dun_wait with the event
number the result gives, and nothing is lost.

Flags:

  -h, --help     this text

The provider keys stay with dun: a client sees provider names, never keys.
`

func mcpCmd(args []string, in io.Reader, out, errw io.Writer) int {
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			fmt.Fprint(out, mcpHelp)
			return 0
		default:
			fmt.Fprintf(errw, "dun mcp: no flag %q\n\n%s", a, mcpHelp)
			return 2
		}
	}
	s := &mcpServer{out: out, dial: tui.DialDund}
	if err := s.serve(in); err != nil {
		fmt.Fprintln(errw, "dun mcp:", err)
		return 1
	}
	return 0
}

// mcpServer answers one client on stdio.
type mcpServer struct {
	out  io.Writer
	wmu  sync.Mutex
	dial func() (net.Conn, error)
	wg   sync.WaitGroup
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *mcpServer) serve(in io.Reader) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m rpcMsg
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			s.write(map[string]any{"jsonrpc": "2.0", "id": nil, "error": rpcErr{-32700, "parse error: " + err.Error()}})
			continue
		}
		if len(m.ID) == 0 {
			continue // a notification (initialized, cancelled): nothing to answer
		}
		// A tool call can wait minutes for a reply; the others answer at
		// once. Each in its own goroutine, so a ping is never stuck behind
		// a wait.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			res, err := s.handle(m)
			if err != nil {
				s.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": err})
				return
			}
			s.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": res})
		}()
	}
	s.wg.Wait()
	return sc.Err()
}

func (s *mcpServer) write(v any) {
	b, _ := json.Marshal(v)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = s.out.Write(append(b, '\n'))
}

func (s *mcpServer) handle(m rpcMsg) (any, *rpcErr) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		v := p.ProtocolVersion
		if v == "" {
			v = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "dun", "version": buildinfo.Version},
			"instructions": "dun runs coding-agent sessions on the models and providers its user logged in to. " +
				"Hand it work that does not need you: dun_start a session in a workspace with the task, " +
				"read the reply, follow up with dun_send. A result that is still working gives an event " +
				"number: dun_wait from it.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, &rpcErr{-32602, err.Error()}
		}
		text, err := s.call(p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": err.Error()}}}, nil
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}, nil
	}
	return nil, &rpcErr{-32601, "no method " + m.Method}
}

// ── the tools ──

func mcpSchema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var (
	propSession = map[string]any{"type": "string", "description": "the session's id (dun_sessions lists them)"}
	propTimeout = map[string]any{"type": "integer", "description": "seconds to wait for the reply (default 600); 0 sends and returns at once"}
)

var mcpTools = []map[string]any{
	{"name": "dun_sessions", "description": "List your dun sessions: id, workspace, whether its engine runs, clients attached, and its last event number.",
		"inputSchema": mcpSchema(nil, map[string]any{})},
	{"name": "dun_start", "description": "Start a dun session in a workspace and send it its first message. Waits for the reply. The session runs on the given model and provider, else dun's saved default.",
		"inputSchema": mcpSchema([]string{"prompt"}, map[string]any{
			"prompt":    map[string]any{"type": "string", "description": "the task"},
			"workspace": map[string]any{"type": "string", "description": "the directory to work in (default: where dun mcp runs)"},
			"model":     map[string]any{"type": "string", "description": "the model id (default: dun's saved model)"},
			"provider":  map[string]any{"type": "string", "description": "a provider dun is logged in to, by name (default: dun's saved one)"},
			"timeout_s": propTimeout,
		})},
	{"name": "dun_send", "description": "Send a message to a dun session and wait for its reply.",
		"inputSchema": mcpSchema([]string{"session", "text"}, map[string]any{
			"session": propSession, "text": map[string]any{"type": "string"}, "timeout_s": propTimeout,
		})},
	{"name": "dun_wait", "description": "Wait for a dun session's next reply, reading its events after an event number (from an earlier result).",
		"inputSchema": mcpSchema([]string{"session", "after"}, map[string]any{
			"session": propSession, "after": map[string]any{"type": "integer", "description": "the event number to read after"},
			"timeout_s": propTimeout,
		})},
	{"name": "dun_answer", "description": "Answer the question a dun session asked, and wait for its reply.",
		"inputSchema": mcpSchema([]string{"session", "value"}, map[string]any{
			"session": propSession, "value": map[string]any{"type": "string", "description": "the option chosen, or your own answer"},
			"timeout_s": propTimeout,
		})},
	{"name": "dun_interrupt", "description": "Stop a dun session's running turn. What it did so far stays.",
		"inputSchema": mcpSchema([]string{"session"}, map[string]any{"session": propSession})},
}

func (s *mcpServer) call(name string, a map[string]any) (string, error) {
	str := func(k string) string { v, _ := a[k].(string); return strings.TrimSpace(v) }
	num := func(k string, dflt int) int {
		switch v := a[k].(type) {
		case float64:
			return int(v)
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return dflt
	}
	wait := time.Duration(num("timeout_s", 600)) * time.Second
	switch name {
	case "dun_sessions":
		return s.sessions()
	case "dun_start":
		if str("prompt") == "" {
			return "", errors.New("prompt is required")
		}
		return s.start(str("workspace"), str("prompt"), str("model"), str("provider"), wait)
	case "dun_send":
		if str("session") == "" || str("text") == "" {
			return "", errors.New("session and text are required")
		}
		return s.send(str("session"), proto.NewCommand("", &proto.User{Content: str("text")}), str("text"), wait)
	case "dun_wait":
		if str("session") == "" {
			return "", errors.New("session is required")
		}
		return s.wait(str("session"), uint64(max(0, num("after", 0))), "", wait)
	case "dun_answer":
		if str("session") == "" {
			return "", errors.New("session and value are required")
		}
		return s.send(str("session"), proto.NewCommand("", &proto.Answer{Value: str("value")}), "", wait)
	case "dun_interrupt":
		if str("session") == "" {
			return "", errors.New("session is required")
		}
		c, err := s.connect()
		if err != nil {
			return "", err
		}
		defer c.Close()
		// Forwarded to the session's engine, which does not ack it.
		cmd := proto.NewCommand("", &proto.Interrupt{Reason: "dun mcp"})
		cmd.Session = str("session")
		if err := c.enc.Encode(cmd); err != nil {
			return "", err
		}
		return "interrupted " + str("session"), nil
	}
	return "", fmt.Errorf("no tool %q", name)
}

// ── dund ──

// dundConn is one connection to dund: commands out, events in.
type dundConn struct {
	net.Conn
	enc *json.Encoder
	dec *json.Decoder
}

func (s *mcpServer) connect() (*dundConn, error) {
	c, err := s.dial()
	if err != nil {
		return nil, fmt.Errorf("dund: %w", err)
	}
	return &dundConn{Conn: c, enc: json.NewEncoder(c), dec: json.NewDecoder(c)}, nil
}

// roundTrip sends cmd and returns the event answering it (by its cid):
// an ack's error is the error.
func (c *dundConn) roundTrip(cmd proto.Command) (proto.Event, error) {
	_ = c.SetDeadline(time.Now().Add(5 * time.Minute)) // a create waits for an engine
	defer c.SetDeadline(time.Time{})
	if err := c.enc.Encode(cmd); err != nil {
		return proto.Event{}, err
	}
	for {
		var ev proto.Event
		if err := c.dec.Decode(&ev); err != nil {
			return ev, err
		}
		if ev.Re != cmd.CID {
			continue
		}
		if a, ok := ev.Payload.(*proto.Ack); ok && a.Error != "" {
			return ev, errors.New(a.Error)
		}
		return ev, nil
	}
}

func (s *mcpServer) list(c *dundConn) ([]proto.SessionSummary, error) {
	ev, err := c.roundTrip(proto.NewCommand("ls", &proto.ListSessions{}))
	if err != nil {
		return nil, err
	}
	l, ok := ev.Payload.(*proto.SessionList)
	if !ok {
		return nil, fmt.Errorf("dund answered %s to a session list", ev.Type)
	}
	return l.Items, nil
}

func (s *mcpServer) sessions() (string, error) {
	c, err := s.connect()
	if err != nil {
		return "", err
	}
	defer c.Close()
	items, err := s.list(c)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "no dun sessions — dun_start makes one", nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })
	var b strings.Builder
	for _, it := range items {
		state := "stopped"
		if it.Running {
			state = "running"
		}
		fmt.Fprintf(&b, "%s  %s  %s · %d attached · last event %d\n", it.ID, it.Workspace, state, it.Attached, it.LastSeq)
	}
	return b.String(), nil
}

func (s *mcpServer) start(ws, prompt, model, provider string, wait time.Duration) (string, error) {
	if ws == "" {
		ws = "."
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return "", fmt.Errorf("workspace %s is not a directory", abs)
	}
	var args []string
	if provider != "" {
		p, ok := dunconfig.LoadConfig().Named(provider)
		if !ok {
			return "", fmt.Errorf("dun is not logged in to a provider named %q (dun's /provider lists them)", provider)
		}
		args = append(args, "--url", p.URL)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	c, err := s.connect()
	if err != nil {
		return "", err
	}
	ev, err := c.roundTrip(proto.NewCommand("create", &proto.Create{Workspace: abs, Args: args}))
	c.Close()
	if err != nil {
		return "", fmt.Errorf("start a session in %s: %w", abs, err)
	}
	return s.send(ev.Session, proto.NewCommand("", &proto.User{Content: prompt}), prompt, wait)
}

// send subscribes to the session from its last event, sends cmd, and waits
// for the reply. echo, when set, is the user message the reply follows: an
// earlier turn's end is not this one's.
func (s *mcpServer) send(session string, cmd proto.Command, echo string, wait time.Duration) (string, error) {
	c, err := s.connect()
	if err != nil {
		return "", err
	}
	items, err := s.list(c)
	c.Close()
	if err != nil {
		return "", err
	}
	var after uint64
	found := false
	for _, it := range items {
		if it.ID == session {
			after, found = it.LastSeq, true
		}
	}
	if !found {
		return "", fmt.Errorf("no session %s (dun_sessions lists them)", session)
	}
	cmd.Session = session
	return s.follow(session, after, &cmd, echo, wait)
}

func (s *mcpServer) wait(session string, after uint64, echo string, wait time.Duration) (string, error) {
	return s.follow(session, after, nil, echo, wait)
}

// follow reads the session's events after seq — sending cmd first, once
// subscribed — until the turn ends, the session asks, or wait runs out.
func (s *mcpServer) follow(session string, after uint64, cmd *proto.Command, echo string, wait time.Duration) (string, error) {
	c, err := s.connect()
	if err != nil {
		return "", err
	}
	defer c.Close()
	sub := proto.NewCommand("sub", &proto.Subscribe{After: after})
	sub.Session = session
	if _, err := c.roundTrip(sub); err != nil {
		return "", err
	}
	if cmd != nil {
		if err := c.enc.Encode(*cmd); err != nil {
			return "", err
		}
	}
	r := mcpReply{session: session, seq: after, mine: echo == ""}
	if wait <= 0 {
		return r.render("sent"), nil
	}
	_ = c.SetReadDeadline(time.Now().Add(wait))
	for {
		var ev proto.Event
		if err := c.dec.Decode(&ev); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return r.render("working"), nil
			}
			return "", err
		}
		if ev.Session != session {
			continue
		}
		if ev.Seq > r.seq {
			r.seq = ev.Seq
		}
		if end := r.take(ev, echo); end != "" {
			return r.render(end), nil
		}
	}
}

// mcpReply gathers one turn's events into the text a caller reads.
type mcpReply struct {
	session string
	seq     uint64
	mine    bool // the turn under way is the one this call waits for
	reply   []string
	tools   map[string]int
	ask     *proto.Ask
	err     string
}

// take reads one event; it returns how the wait ends, or "".
func (r *mcpReply) take(ev proto.Event, echo string) string {
	switch p := ev.Payload.(type) {
	case *proto.UserEcho:
		if !r.mine && strings.TrimSpace(p.Content) == strings.TrimSpace(echo) {
			r.mine = true
		}
	case *proto.Ack:
		if p.Error != "" && ev.Re == "" {
			r.err = p.Error
			return "error"
		}
	}
	if !r.mine {
		return ""
	}
	switch p := ev.Payload.(type) {
	case *proto.Message:
		if p.Role == "assistant" && strings.TrimSpace(p.Content) != "" {
			r.reply = append(r.reply, p.Content)
		}
	case *proto.ToolCall:
		if r.tools == nil {
			r.tools = map[string]int{}
		}
		r.tools[p.Tool]++
	case *proto.Ask:
		r.ask = p
		return "asking"
	case *proto.Error:
		r.err = p.Error
		return "error"
	case *proto.Done:
		if p.Reason == "interrupted" {
			return "interrupted"
		}
		return "done"
	case *proto.Exit:
		return "exited"
	}
	return ""
}

func (r mcpReply) render(end string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session %s · %s · event %d\n", r.session, end, r.seq)
	switch end {
	case "working":
		fmt.Fprintf(&b, "still working: dun_wait {\"session\": %q, \"after\": %d} to read on\n", r.session, r.seq)
	case "sent":
		fmt.Fprintf(&b, "sent: dun_wait {\"session\": %q, \"after\": %d} for the reply\n", r.session, r.seq)
	case "asking":
		fmt.Fprintf(&b, "question: %s\n", r.ask.Question)
		if len(r.ask.Options) > 0 {
			fmt.Fprintf(&b, "options: %s\n", strings.Join(r.ask.Options, " | "))
		}
		b.WriteString("answer with dun_answer\n")
	case "error":
		fmt.Fprintf(&b, "error: %s\n", r.err)
	}
	if len(r.reply) > 0 {
		b.WriteString("\n" + strings.Join(r.reply, "\n\n") + "\n")
	}
	if len(r.tools) > 0 {
		names := make([]string, 0, len(r.tools))
		for n := range r.tools {
			names = append(names, n)
		}
		sort.Strings(names)
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = fmt.Sprintf("%s ×%d", n, r.tools[n])
		}
		fmt.Fprintf(&b, "\ntools: %s\n", strings.Join(parts, ", "))
	}
	return b.String()
}
