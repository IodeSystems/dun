package main

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/iodesystems/dun/proto"
)

// The MCP handshake and the tool list need no dund.
func TestMCP_InitializeAndToolList(t *testing.T) {
	var out bytes.Buffer
	s := &mcpServer{out: &out, dial: func() (net.Conn, error) { t.Fatal("no dund needed"); return nil, nil }}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"nope"}`,
	}, "\n")
	if err := s.serve(strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	got := map[float64]map[string]any{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		got[m["id"].(float64)] = m
	}
	if len(got) != 3 {
		t.Fatalf("want 3 answers (a notification gets none): %s", out.String())
	}
	if v := got[1]["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Errorf("protocol version %v; want the client's", v)
	}
	var names []string
	for _, tl := range got[2]["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, " ") != "dun_sessions dun_start dun_send dun_wait dun_answer dun_interrupt" {
		t.Errorf("tools %v", names)
	}
	if got[3]["error"] == nil {
		t.Error("an unknown method must be an error")
	}
}

// A turn's events become the reply: only after this call's own message,
// the assistant's text, the tools used, and how it ended.
func TestMCP_ReplyFromEvents(t *testing.T) {
	r := mcpReply{session: "s1"}
	evs := []any{
		&proto.Message{Role: "assistant", Content: "an earlier turn"},
		&proto.Done{},
		&proto.UserEcho{Content: "do it"},
		&proto.ToolCall{Tool: "exec"}, &proto.ToolCall{Tool: "exec"}, &proto.ToolCall{Tool: "file_read"},
		&proto.Message{Role: "assistant", Content: "did it"},
		&proto.Done{},
	}
	end := ""
	for i, p := range evs {
		ev := proto.NewEvent(p)
		ev.Session, ev.Seq = "s1", uint64(10+i)
		r.seq = ev.Seq
		if end = r.take(ev, "do it"); end != "" {
			break
		}
	}
	out := r.render(end)
	for _, want := range []string{"session s1 · done · event 17", "did it", "tools: exec ×2, file_read ×1"} {
		if !strings.Contains(out, want) {
			t.Errorf("reply lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "an earlier turn") {
		t.Errorf("an earlier turn's reply leaked in:\n%s", out)
	}

	a := mcpReply{session: "s1", mine: true}
	if e := a.take(proto.NewEvent(&proto.Ask{Question: "which?", Options: []string{"a", "b"}}), ""); e != "asking" {
		t.Fatalf("an ask ends the wait: %q", e)
	}
	if out := a.render("asking"); !strings.Contains(out, "question: which?") || !strings.Contains(out, "options: a | b") {
		t.Errorf("ask render:\n%s", out)
	}
}
