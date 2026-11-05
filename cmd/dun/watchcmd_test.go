package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"

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
	dial := fakeDund(t, nil, []any{&proto.Done{}, &proto.Done{}})
	var out, errw bytes.Buffer
	if code := watchWith([]string{"s1", "--after", "40", "--once"}, &out, &errw, dial); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if out.String() != "s1 · done · event 41\n" {
		t.Errorf("--once: %q", out.String())
	}

	out.Reset()
	dial = fakeDund(t, nil, []any{&proto.Done{}})
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

// A result that leaves the session working names the watch command.
func TestMCP_WorkingNamesTheWatch(t *testing.T) {
	r := mcpReply{session: "s1", seq: 41}
	for _, end := range []string{"sent", "working"} {
		if out := r.render(end); !strings.Contains(out, "Monitor `dun watch s1 --after 41`") {
			t.Errorf("%s:\n%s", end, out)
		}
	}
	if out := r.render("done"); strings.Contains(out, "dun watch") {
		t.Errorf("a finished turn needs no watch:\n%s", out)
	}
}
