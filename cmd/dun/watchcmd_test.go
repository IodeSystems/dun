package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// fakeDund answers a list and a subscribe the way dund does, then sends the
// session's events and closes.
func fakeDund(t *testing.T, items []proto.SessionSummary, events []any) func() (net.Conn, error) {
	return func() (net.Conn, error) {
		cli, srv := net.Pipe()
		go func() {
			defer srv.Close()
			// Whole lines: net.Pipe's Write returns only when every byte is
			// read, and a json.Decoder stops before the newline, which leaves
			// the client's write stuck while this side answers it.
			sc, enc := bufio.NewScanner(srv), json.NewEncoder(srv)
			seq := uint64(40)
			for sc.Scan() {
				var cmd proto.Command
				if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
					return
				}
				switch cmd.Payload.(type) {
				case *proto.ListSessions:
					ev := proto.NewEvent(&proto.SessionList{Items: items})
					ev.Re = cmd.CID
					_ = enc.Encode(ev)
				case *proto.Subscribe:
					ev := proto.NewEvent(&proto.Ack{})
					ev.Re = cmd.CID
					_ = enc.Encode(ev)
					for _, p := range events {
						seq++
						e := proto.NewEvent(p)
						e.Session, e.Seq = "s1", seq
						if err := enc.Encode(e); err != nil {
							return
						}
					}
					return
				}
			}
		}()
		return cli, nil
	}
}

// One line per turn end — done with the reply's first line, asking with the
// question and options — and the watch ends when the engine exits.
func TestWatch_LinePerTurnEnd(t *testing.T) {
	dial := fakeDund(t, []proto.SessionSummary{{ID: "s1", Running: true, LastSeq: 40}}, []any{
		&proto.ToolCall{Tool: "exec"},
		&proto.Message{Role: "assistant", Content: "\nFixed the flaky test.\nDetails follow."},
		&proto.Done{},
		&proto.Ask{Question: "Which branch?", Options: []string{"main", "dev"}},
		&proto.Error{Error: "endpoint\nwent away"},
		&proto.Done{Reason: "interrupted"},
		&proto.Exit{},
	})
	var out, errw bytes.Buffer
	if code := watchWith([]string{"s1"}, &out, &errw, dial); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errw.String())
	}
	want := strings.Join([]string{
		"s1 · done · event 43 · Fixed the flaky test.",
		"s1 · asking · event 44 · Which branch? [main | dev]",
		"s1 · error · event 45 · endpoint went away",
		"s1 · interrupted · event 46",
		"s1 · exited · event 47",
	}, "\n") + "\n"
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

// --once stops at the first line; a dropped connection is said on stdout,
// where a Monitor reads, and is a failure.
func TestWatch_OnceAndDrop(t *testing.T) {
	run := []proto.SessionSummary{{ID: "s1", Running: true, LastSeq: 40}}
	dial := fakeDund(t, run, []any{&proto.Done{}, &proto.Done{}})
	var out, errw bytes.Buffer
	if code := watchWith([]string{"s1", "--after", "40", "--once"}, &out, &errw, dial); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if out.String() != "s1 · done · event 41\n" {
		t.Errorf("--once: %q", out.String())
	}

	out.Reset()
	dial = fakeDund(t, run, []any{&proto.Done{}})
	if code := watchWith([]string{"s1", "--after=40"}, &out, &errw, dial); code != 1 {
		t.Fatalf("a dropped connection must fail: exit %d", code)
	}
	if !strings.Contains(out.String(), "s1 · watch failed · dund closed the connection") {
		t.Errorf("the failure must reach stdout: %q", out.String())
	}

	out.Reset()
	dial = fakeDund(t, []proto.SessionSummary{{ID: "other", Running: true}}, nil)
	if code := watchWith([]string{"s1"}, &out, &errw, dial); code != 1 || !strings.Contains(out.String(), "no session s1") {
		t.Errorf("an unknown session: exit %d, %q", code, out.String())
	}
}

// dun_sessions lists only sessions whose engine is connected to dund.
func TestMCP_SessionsOnlyRunning(t *testing.T) {
	s := &mcpServer{dial: fakeDund(t, []proto.SessionSummary{
		{ID: "a", Workspace: "/w", Running: true, LastSeq: 7},
		{ID: "b", Workspace: "/w", Running: false},
		{ID: "c", Workspace: "/w", Running: false},
	}, nil)}
	got, err := s.sessions()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "a  /w  0 attached · last event 7") || strings.Contains(got, "b  ") ||
		!strings.Contains(got, "(2 stopped, not listed)") {
		t.Errorf("sessions:\n%s", got)
	}

	s.dial = fakeDund(t, []proto.SessionSummary{{ID: "b"}}, nil)
	if got, _ := s.sessions(); !strings.Contains(got, "no dun session is running (1 stopped)") {
		t.Errorf("none running: %q", got)
	}
}

// The watch hint lives in the server's instructions, not in each result.
func TestMCP_WorkingResultIsShort(t *testing.T) {
	r := mcpReply{session: "s1", seq: 41}
	for _, end := range []string{"sent", "working"} {
		if out := r.render(end); strings.Contains(out, "dun watch") || len(out) > 100 {
			t.Errorf("%s:\n%s", end, out)
		}
	}
	s := &mcpServer{}
	if in := s.mcpInstructions(); !strings.Contains(in, "dun watch <id> --after N") || !strings.Contains(in, "dun_send") {
		t.Errorf("instructions:\n%s", in)
	}
}

// snapshotEvents is what dund sends for a subscribe from far behind: a
// snapshot (history, state), not the events, so the Done is not among them.
func snapshotEvents(phase string, ask *proto.Ask) []any {
	ts := proto.NewEvent(&proto.TurnState{Phase: phase, Ask: ask})
	return []any{&proto.Snapshot{
		After: 292,
		Items: []proto.HistoryItem{
			{Kind: "user", Content: "an earlier turn"},
			{Kind: "assistant", Content: "earlier reply"},
			{Kind: "user", Content: "do it"},
			{Kind: "tool_call", Tool: "exec"},
			{Kind: "assistant", Content: "did it"},
		},
		State: []proto.Event{ts},
	}}
}

// dun_wait and dun watch with an old "after": dund answers with a snapshot
// and the finished turn inside it must be reported, not waited out.
func TestMCP_SnapshotReportsFinishedTurn(t *testing.T) {
	items := []proto.SessionSummary{{ID: "s1", Running: true, LastSeq: 292}}
	s := &mcpServer{dial: fakeDund(t, items, snapshotEvents(proto.PhaseIdle, nil))}
	got, err := s.wait("s1", 3, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"s1 · done · event 292", "did it", "tools: exec ×1"} {
		if !strings.Contains(got, want) {
			t.Errorf("wait lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "earlier reply") {
		t.Errorf("an earlier turn's reply leaked in:\n%s", got)
	}

	var out, errw bytes.Buffer
	dial := fakeDund(t, items, snapshotEvents(proto.PhaseIdle, nil))
	if code := watchWith([]string{"s1", "--after", "3", "--once"}, &out, &errw, dial); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errw.String())
	}
	if out.String() != "s1 · done · event 292 · did it\n" {
		t.Errorf("watch: %q", out.String())
	}

	// A turn still running is not over; the question it waits on is asked.
	s.dial = fakeDund(t, items, append(snapshotEvents(proto.PhaseWorking, nil), &proto.Done{}))
	if got, _ := s.wait("s1", 3, "", 5*time.Second); !strings.Contains(got, "· done · event") || !strings.Contains(got, "did it") {
		t.Errorf("working snapshot, then done:\n%s", got)
	}
	s.dial = fakeDund(t, items, snapshotEvents(proto.PhaseWaitingAnswer, &proto.Ask{Question: "which?"}))
	if got, _ := s.wait("s1", 3, "", 5*time.Second); !strings.Contains(got, "asking") || !strings.Contains(got, "question: which?") {
		t.Errorf("asking snapshot:\n%s", got)
	}
}

func TestMCP_InterruptUnknownSession(t *testing.T) {
	s := &mcpServer{dial: fakeDund(t, []proto.SessionSummary{{ID: "s1", Running: true}}, nil)}
	if _, err := s.call("dun_interrupt", map[string]any{"session": "nope"}); err == nil || !strings.Contains(err.Error(), "no session nope") {
		t.Errorf("unknown session: %v", err)
	}
}

func TestMCP_Groups(t *testing.T) {
	names := func(pos, neg []string) string {
		g, err := mcpGroups(pos, neg)
		if err != nil {
			return "error"
		}
		s := &mcpServer{groups: g}
		var n []string
		for _, t := range s.tools() {
			n = append(n, strings.TrimPrefix(t["name"].(string), "dun_"))
		}
		return strings.Join(n, " ")
	}
	for _, c := range []struct {
		pos, neg []string
		want     string
	}{
		{nil, nil, "sessions start send wait answer interrupt stop usage"},
		{[]string{"msg"}, nil, "sessions send"},
		{[]string{"agent"}, nil, "sessions start wait answer interrupt stop usage"},
		{nil, []string{"agent"}, "sessions send"},
		{nil, []string{"msg"}, "sessions start wait answer interrupt stop usage"},
		{[]string{"agent", "msg"}, []string{"agent"}, "sessions send"},
		{nil, []string{"agent", "msg"}, "error"},
		{[]string{"agent"}, []string{"agent"}, "error"},
		{[]string{"msg"}, []string{"agent"}, "sessions send"},
	} {
		if got := names(c.pos, c.neg); got != c.want {
			t.Errorf("%v %v: %q, want %q", c.pos, c.neg, got, c.want)
		}
	}
	g, _ := mcpGroups([]string{"msg"}, nil)
	s := &mcpServer{groups: g}
	if _, err := s.call("dun_start", map[string]any{"prompt": "x"}); err == nil {
		t.Error("a tool of a group that is off must be refused")
	}
	if in := s.mcpInstructions(); strings.Contains(in, "dun_start") || !strings.Contains(in, "dun_send") {
		t.Errorf("instructions follow the groups:\n%s", in)
	}
	var out, errw bytes.Buffer
	if code := mcpCmd([]string{"--agent", "--no-agent"}, strings.NewReader(""), &out, &errw); code != 2 {
		t.Errorf("no group left: exit %d", code)
	}
	if code := mcpCmd([]string{"--bogus"}, strings.NewReader(""), &out, &errw); code != 2 {
		t.Errorf("unknown flag: exit %d", code)
	}
}

// dun_start passes --settle-delay=-1s unless settle is asked for: an MCP
// client never signals presence, so settle would wake every worker unasked.
func TestMCP_StartSettleOffByDefault(t *testing.T) {
	var mu sync.Mutex
	var created [][]string
	dial := func() (net.Conn, error) {
		cli, srv := net.Pipe()
		go func() {
			defer srv.Close()
			sc, enc := bufio.NewScanner(srv), json.NewEncoder(srv)
			for sc.Scan() {
				var cmd proto.Command
				if json.Unmarshal(sc.Bytes(), &cmd) != nil {
					return
				}
				var ev proto.Event
				switch p := cmd.Payload.(type) {
				case *proto.Create:
					mu.Lock()
					created = append(created, p.Args)
					mu.Unlock()
					ev = proto.NewEvent(&proto.Ack{})
					ev.Session = "s1"
				case *proto.ListSessions:
					ev = proto.NewEvent(&proto.SessionList{Items: []proto.SessionSummary{{ID: "s1", Running: true}}})
				case *proto.Subscribe:
					ev = proto.NewEvent(&proto.Ack{})
				default:
					continue
				}
				ev.Re = cmd.CID
				_ = enc.Encode(ev)
			}
		}()
		return cli, nil
	}
	s := &mcpServer{dial: dial}
	for _, settle := range []bool{false, true} {
		if _, err := s.start(".", "go", "", "", settle, 0); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != 2 || strings.Join(created[0], " ") != "--worker --settle-delay=-1s" || strings.Join(created[1], " ") != "--worker" {
		t.Errorf("create args %q", created)
	}
}

// A snapshot that is not the caller's finished turn must not read as done:
// a message still last in the history, or queued, has had no turn yet.
func TestMCP_SnapshotNotDoneForPendingMessage(t *testing.T) {
	items := []proto.SessionSummary{{ID: "s1", Running: true, LastSeq: 292}}
	ts := proto.NewEvent(&proto.TurnState{Phase: proto.PhaseIdle})
	cases := map[string]*proto.Snapshot{
		"user last": {After: 292, State: []proto.Event{ts}, Items: []proto.HistoryItem{
			{Kind: "user", Content: "a"}, {Kind: "assistant", Content: "r"}, {Kind: "user", Content: "b"}}},
		"queued last": {After: 292, State: []proto.Event{ts}, Items: []proto.HistoryItem{
			{Kind: "user", Content: "a"}, {Kind: "assistant", Content: "r"}, {Kind: "user", Content: "b", Queued: true}}},
		"state queue": {After: 292, Items: []proto.HistoryItem{{Kind: "user", Content: "a"}, {Kind: "assistant", Content: "r"}},
			State: []proto.Event{proto.NewEvent(&proto.TurnState{Phase: proto.PhaseIdle, Queued: []proto.QueuedMsg{{ID: "q", Text: "b"}}})}},
	}
	for name, sn := range cases {
		s := &mcpServer{dial: fakeDund(t, items, []any{sn})}
		got, err := s.wait("s1", 3, "", 300*time.Millisecond)
		if strings.Contains(got, "· done ·") { // the fake closes after its events: EOF is the wait going on
			t.Errorf("%s: read as done: %q %v", name, got, err)
		}
	}
}

// An after past the log's end (a restarted dund) is an error, not the old
// turn's end; a stopped session is not read, which would restart it.
func TestMCP_WaitPastEndAndStopped(t *testing.T) {
	items := []proto.SessionSummary{{ID: "s1", Running: true, LastSeq: 10}, {ID: "z", Running: false, LastSeq: 5}}
	s := &mcpServer{dial: fakeDund(t, items, snapshotEvents(proto.PhaseIdle, nil))}
	if _, err := s.wait("s1", 99, "", time.Second); err == nil || !strings.Contains(err.Error(), "event 99 is past the session's log (last 10)") {
		t.Errorf("past end: %v", err)
	}
	var out, errw bytes.Buffer
	if code := watchWith([]string{"s1", "--after", "99"}, &out, &errw, s.dial); code != 1 || !strings.Contains(out.String(), "past the session's log") {
		t.Errorf("watch past end: %d %q", code, out.String())
	}
	for name, fn := range map[string]func() (string, error){
		"wait":  func() (string, error) { return s.wait("z", 1, "", time.Second) },
		"usage": func() (string, error) { return s.usage("z") },
	} {
		if _, err := fn(); err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Errorf("%s of a stopped session: %v", name, err)
		}
	}
}

func TestMCP_BoolArgsStrict(t *testing.T) {
	s := &mcpServer{dial: fakeDund(t, nil, nil)}
	for _, c := range []struct{ tool, k string }{{"dun_start", "settle"}, {"dun_stop", "forget"}} {
		args := map[string]any{"prompt": "x", "session": "s1", c.k: "yes"}
		if _, err := s.call(c.tool, args); err == nil || !strings.Contains(err.Error(), c.k+" must be true or false") {
			t.Errorf("%s %s=\"yes\": %v", c.tool, c.k, err)
		}
	}
}
