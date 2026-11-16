package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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

  dun_sessions   your running dun sessions: id, workspace, last event
  dun_start      start a session in a workspace with a first message;
                 optional model and provider (a provider you logged in to
                 with /provider login); waits for the reply
                 settle true lets dun wake the idle worker after a tool-using
                 turn (a paid turn you did not ask for; default off)
  dun_send       send a message to a session; waits for its reply
  dun_wait       wait for a session's next reply, from an event number
  dun_answer     answer a question the session asked (ask_user)
  dun_interrupt  stop the session's running turn
  dun_stop       end the session's engine; with forget, delete the session
  dun_usage      the session's tokens and, when the provider reports it, cost

A reply ends one of four ways: done (the reply), asking (a question and
its options: answer with dun_answer), error, or still working when the
wait ran out (timeout_s, default 600): call dun_wait with the event
number the result gives, and nothing is lost.

To be told when a session ends its turn or asks, without holding a call
open: send with timeout_s 0 and run "dun watch <session> --after N" (the
result names it) under Claude Code's Monitor, or with --once in the
background. dun watch --help has the details.

Tool groups: agent (dun_start, dun_wait, dun_answer, dun_interrupt, dun_stop,
dun_usage, dun_sessions) starts and drives workers; msg (dun_sessions,
dun_send) messages sessions that exist. No group flag: all groups. Group
flags add: --agent --msg give only those; --no-agent --no-msg remove from
all; both kinds give the positives minus the negatives. The server's
instructions to the client cover only the groups on.

Flags:

  --agent        the agent group
  --msg          the msg group
  --no-agent     all groups but agent
  --no-msg       all groups but msg
  -h, --help     this text

The provider keys stay with dun: a client sees provider names, never keys.
`

func mcpCmd(args []string, in io.Reader, out, errw io.Writer) int {
	var pos, neg []string
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			fmt.Fprint(out, mcpHelp)
			return 0
		case "--agent", "--msg":
			pos = append(pos, a[2:])
		case "--no-agent", "--no-msg":
			neg = append(neg, a[5:])
		default:
			fmt.Fprintf(errw, "dun mcp: no flag %q\n\n%s", a, mcpHelp)
			return 2
		}
	}
	groups, err := mcpGroups(pos, neg)
	if err != nil {
		fmt.Fprintf(errw, "dun mcp: %v\n\n%s", err, mcpHelp)
		return 2
	}
	s := &mcpServer{out: out, dial: tui.DialDund, groups: groups}
	if err := s.serve(in); err != nil {
		fmt.Fprintln(errw, "dun mcp:", err)
		return 1
	}
	return 0
}

// mcpGroupNames are the tool groups.
var mcpGroupNames = []string{"agent", "msg"}

// mcpGroups is the groups on: the positives (all when none) minus the
// negatives.
func mcpGroups(pos, neg []string) (map[string]bool, error) {
	on := map[string]bool{}
	for _, g := range mcpGroupNames {
		on[g] = len(pos) == 0
	}
	for _, g := range pos {
		on[g] = true
	}
	for _, g := range neg {
		on[g] = false
	}
	for _, g := range mcpGroupNames {
		if on[g] {
			return on, nil
		}
	}
	return nil, errors.New("these flags leave no tool group on")
}

// mcpServer answers one client on stdio.
type mcpServer struct {
	out  io.Writer
	wmu  sync.Mutex
	dial func() (net.Conn, error)
	wg   sync.WaitGroup
	// groups are the tool groups on (mcpGroups); nil is all.
	groups map[string]bool
}

func (s *mcpServer) on(group string) bool { return s.groups == nil || s.groups[group] }

// mcpInstructions is the server's guidance to the client, one short section
// per group on. Every word costs the client's context in every session.
func (s *mcpServer) mcpInstructions() string {
	var parts []string
	if s.on("agent") {
		t := "dun runs worker sessions on cheaper models. dun_start with timeout_s 0; then run " +
			"`dun watch <id> --after N` (N: the result's event) under Monitor instead of holding dun_wait; " +
			"when it prints a line, dun_wait {session, after: N} returns the reply. " +
			"Tell the worker to end with a short final report: results stay small. " +
			"Pick model and provider per task: cheap for mechanical work. Workers do not self-wake (settle off)."
		var names []string
		for _, p := range dunconfig.LoadConfig().ProviderList() {
			names = append(names, p.Name)
		}
		if len(names) > 0 {
			t += " Providers: " + strings.Join(names, ", ") + "."
		}
		parts = append(parts, t+" dun_usage shows tokens and cost; dun_stop ends a session.")
	}
	if s.on("msg") {
		parts = append(parts, "dun_sessions lists running sessions; dun_send messages one and waits for its reply (timeout_s 0: send only).")
	}
	return strings.Join(parts, "\n")
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
			"instructions":    s.mcpInstructions(),
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools()}, nil
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

// mcpToolGroups is the groups a tool belongs to; a tool is on when any is.
var mcpToolGroups = map[string][]string{
	"dun_sessions": {"agent", "msg"}, "dun_send": {"msg"},
	"dun_start": {"agent"}, "dun_wait": {"agent"}, "dun_answer": {"agent"},
	"dun_interrupt": {"agent"}, "dun_stop": {"agent"}, "dun_usage": {"agent"},
}

func (s *mcpServer) toolOn(name string) bool {
	for _, g := range mcpToolGroups[name] {
		if s.on(g) {
			return true
		}
	}
	return false
}

func (s *mcpServer) tools() []map[string]any {
	out := []map[string]any{}
	for _, t := range mcpTools {
		if s.toolOn(t["name"].(string)) {
			out = append(out, t)
		}
	}
	return out
}

var mcpTools = []map[string]any{
	{"name": "dun_sessions", "description": "List your running dun sessions (engine connected to dund): id, workspace, clients attached, and its last event number.",
		"inputSchema": mcpSchema(nil, map[string]any{})},
	{"name": "dun_start", "description": "Start a dun session in a workspace and send it its first message. Waits for the reply. The session runs on the given model and provider, else dun's saved default.",
		"inputSchema": mcpSchema([]string{"prompt"}, map[string]any{
			"prompt":    map[string]any{"type": "string", "description": "the task"},
			"workspace": map[string]any{"type": "string", "description": "the directory to work in (default: where dun mcp runs)"},
			"model":     map[string]any{"type": "string", "description": "the model id (default: dun's saved model)"},
			"provider":  map[string]any{"type": "string", "description": "a provider dun is logged in to, by name (default: dun's saved one)"},
			"settle":    map[string]any{"type": "boolean", "description": "let dun wake the idle worker 2 minutes after a tool-using turn, a paid turn you did not ask for (default false)"},
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
	{"name": "dun_stop", "description": "End a dun session's engine; the session stays resumable. With forget, delete the session too.",
		"inputSchema": mcpSchema([]string{"session"}, map[string]any{
			"session": propSession, "forget": map[string]any{"type": "boolean", "description": "also delete the session and its log"},
		})},
	{"name": "dun_usage", "description": "A running dun session's tokens and, when its provider reports it, cost.",
		"inputSchema": mcpSchema([]string{"session"}, map[string]any{"session": propSession})},
}

// boolArg is an optional boolean argument: absent is false, another type an
// error rather than a silent false.
func boolArg(a map[string]any, k string) (bool, error) {
	v, ok := a[k]
	if !ok || v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be true or false", k)
	}
	return b, nil
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
	if !s.toolOn(name) {
		return "", fmt.Errorf("no tool %q", name)
	}
	switch name {
	case "dun_sessions":
		return s.sessions()
	case "dun_start":
		if str("prompt") == "" {
			return "", errors.New("prompt is required")
		}
		settle, err := boolArg(a, "settle")
		if err != nil {
			return "", err
		}
		return s.start(str("workspace"), str("prompt"), str("model"), str("provider"), settle, wait)
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
		if str("session") == "" || strings.TrimSpace(str("value")) == "" {
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
		// The engine does not ack an interrupt: an unknown session is caught here.
		if _, err := s.lastSeq(c, str("session")); err != nil {
			return "", err
		}
		cmd := proto.NewCommand("", &proto.Interrupt{Reason: "dun mcp"})
		cmd.Session = str("session")
		if err := c.enc.Encode(cmd); err != nil {
			return "", err
		}
		return "interrupted " + str("session"), nil
	case "dun_stop":
		if str("session") == "" {
			return "", errors.New("session is required")
		}
		c, err := s.connect()
		if err != nil {
			return "", err
		}
		defer c.Close()
		var payload any = &proto.Stop{}
		verb := "stopped"
		forget, err := boolArg(a, "forget")
		if err != nil {
			return "", err
		}
		if forget {
			payload, verb = &proto.Forget{}, "deleted"
		}
		cmd := proto.NewCommand("stop", payload)
		cmd.Session = str("session")
		if _, err := c.roundTrip(cmd); err != nil {
			return "", err
		}
		return verb + " " + str("session"), nil
	case "dun_usage":
		if str("session") == "" {
			return "", errors.New("session is required")
		}
		return s.usage(str("session"))
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
	// Only sessions whose engine is connected to dund (USER 2026-11-05): a
	// stopped one is history, and the list of every session ever made buries
	// the few a caller can talk to.
	live := items[:0]
	for _, it := range items {
		if it.Running {
			live = append(live, it)
		}
	}
	stopped := len(items) - len(live)
	if len(live) == 0 {
		return fmt.Sprintf("no dun session is running (%d stopped) — dun_start makes one", stopped), nil
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID > live[j].ID })
	var b strings.Builder
	for _, it := range live {
		fmt.Fprintf(&b, "%s  %s  %d attached · last event %d\n", it.ID, it.Workspace, it.Attached, it.LastSeq)
	}
	if stopped > 0 {
		fmt.Fprintf(&b, "(%d stopped, not listed)\n", stopped)
	}
	return b.String(), nil
}

func (s *mcpServer) start(ws, prompt, model, provider string, settle bool, wait time.Duration) (string, error) {
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
	// Settle wakes an idle session after a tool-using turn, expecting a person
	// to come back; an MCP client never signals one, so each worker would get
	// an unrequested paid turn.
	if !settle {
		args = append(args, "--settle-delay=-1s")
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
	after, err := s.lastSeq(c, session)
	c.Close()
	if err != nil {
		return "", err
	}
	cmd.Session = session
	return s.follow(session, after, &cmd, echo, wait)
}

// lastSeq is the session's last event, or an error when dund has no such
// session.
func (s *mcpServer) lastSeq(c *dundConn, session string) (uint64, error) {
	items, err := s.list(c)
	if err != nil {
		return 0, err
	}
	for _, it := range items {
		if it.ID == session {
			return it.LastSeq, nil
		}
	}
	return 0, fmt.Errorf("no session %s (dun_sessions lists them)", session)
}

// live is the session's summary, or an error when dund has no such session
// or its engine is stopped: reading a stopped session would restart it, so a
// read never subscribes to one.
func (s *mcpServer) live(c *dundConn, session string) (proto.SessionSummary, error) {
	items, err := s.list(c)
	if err != nil {
		return proto.SessionSummary{}, err
	}
	for _, it := range items {
		if it.ID == session {
			if !it.Running {
				return it, fmt.Errorf("session %s is stopped; reading it would restart it (dun --resume %s)", session, session)
			}
			return it, nil
		}
	}
	return proto.SessionSummary{}, fmt.Errorf("no session %s (dun_sessions lists them)", session)
}

// pastEnd is the error for an event number beyond the session's log: dund
// answers such a subscribe with a snapshot of the old, finished turn.
func pastEnd(after uint64, it proto.SessionSummary) error {
	return fmt.Errorf("event %d is past the session's log (last %d); call dun_sessions", after, it.LastSeq)
}

// usage reads a session's accounting from the state a snapshot carries:
// subscribing past its last event is a gap dund answers with one.
func (s *mcpServer) usage(session string) (string, error) {
	c, err := s.connect()
	if err != nil {
		return "", err
	}
	defer c.Close()
	if _, err := s.live(c, session); err != nil {
		return "", fmt.Errorf("usage unavailable: %w", err)
	}
	sub := proto.NewCommand("sub", &proto.Subscribe{After: math.MaxUint64})
	sub.Session = session
	if _, err := c.roundTrip(sub); err != nil {
		return "", err
	}
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		var ev proto.Event
		if err := c.dec.Decode(&ev); err != nil {
			return "", fmt.Errorf("no usage from %s: %w", session, err)
		}
		snap, ok := ev.Payload.(*proto.Snapshot)
		if !ok || ev.Session != session {
			continue
		}
		for _, st := range snap.State {
			if u, ok := st.Payload.(*proto.Usage); ok {
				return mcpUsageText(session, u), nil
			}
		}
		return "session " + session + ": no turn has ended yet, so no usage\n", nil
	}
}

func mcpUsageText(session string, u *proto.Usage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session %s · %d rounds · %d tokens (%d processed, %d cached, %d generated)\n",
		session, u.Turns, u.Total, u.Processed, u.Cached, u.Generated)
	if sp := u.Spend; sp != nil && sp.Session != nil {
		cur := sp.Currency
		if cur == "" {
			cur = "units"
		}
		fmt.Fprintf(&b, "cost %.4f %s over %d of %d calls that reported one\n", *sp.Session, cur, sp.CostedCalls, sp.Calls)
	} else {
		b.WriteString("cost: the provider reported none\n")
	}
	return b.String()
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
	if cmd == nil { // a read, not a send: send's after is the log's end
		it, err := s.live(c, session)
		if err != nil {
			return "", err
		}
		if after > it.LastSeq {
			return "", pastEnd(after, it)
		}
	}
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
		if sn, ok := ev.Payload.(*proto.Snapshot); ok && sn.After > r.seq {
			r.seq = sn.After
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
	case *proto.Snapshot:
		return r.fromSnapshot(p)
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

// fromSnapshot reads the turn from a snapshot, which dund sends in place of
// a replay it cannot give (a subscribe from far back, or past 256 events): the
// events the caller waits for are inside it. The reply is what the history
// holds after its last user message; the turn is over when the state says
// idle, asking when it waits for an answer, else its live events follow.
func (r *mcpReply) fromSnapshot(sn *proto.Snapshot) string {
	last := -1
	for i, it := range sn.Items {
		if it.Kind == "user" && !it.Queued {
			last = i
		}
	}
	// A message is last in the history, or waits in the queue: the turn for
	// it has not run, and the idle state is the one before it.
	pending := len(sn.Items) > 0 && sn.Items[len(sn.Items)-1].Kind == "user"
	if last < 0 {
		return ""
	}
	r.reply, r.tools = nil, nil
	for _, it := range sn.Items[last+1:] {
		switch it.Kind {
		case "assistant":
			if strings.TrimSpace(it.Content) != "" {
				r.reply = append(r.reply, it.Content)
			}
		case "tool_call":
			if r.tools == nil {
				r.tools = map[string]int{}
			}
			r.tools[it.Tool]++
		}
	}
	for _, st := range sn.State {
		if ts, ok := st.Payload.(*proto.TurnState); ok {
			switch {
			case ts.Phase == proto.PhaseWaitingAnswer && ts.Ask != nil:
				r.ask = ts.Ask
				return "asking"
			case ts.Phase == proto.PhaseIdle && !pending && len(ts.Queued) == 0:
				return "done"
			}
		}
	}
	return ""
}

func (r mcpReply) render(end string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session %s · %s · event %d\n", r.session, end, r.seq)
	switch end {
	case "working":
		b.WriteString("still working: dun_wait from this event to read on\n")
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
