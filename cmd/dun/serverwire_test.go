package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/iodesystems/dun"
)

// What /mcp's table and detail read reaches the TUI through the wire: the
// detail once read fields the event never carried, and its test called the
// renderer with a hand-made state.
func TestServerState_ThroughTheWire(t *testing.T) {
	at := time.UnixMilli(time.Now().UnixMilli())
	in := dun.ServerState{ID: "vay", Running: true, Hosted: true, Tools: 3, Cost: 1200, Standalone: 900,
		Description: "the vay server", Title: "Vay", Version: "1.2.0", Instructions: "use it for vay",
		Starting: true, StartingSeconds: 2,
		Log: []dun.ServerLogLine{{At: at, Text: "running vay"}, {At: at, Text: "warming up", Stderr: true}}}
	b, err := json.Marshal(serversProto([]dun.ServerState{in}))
	if err != nil {
		t.Fatal(err)
	}
	var list []any
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	got, _ := serverStates(list)
	if len(got) != 1 {
		t.Fatalf("%d states", len(got))
	}
	g := got[0]
	if g.Hosted != in.Hosted || g.Cost != in.Cost || g.Standalone != in.Standalone || g.Description != in.Description ||
		g.Title != in.Title || g.Version != in.Version || g.Instructions != in.Instructions || !g.Starting {
		t.Errorf("lost on the wire:\n got %+v\nwant %+v", g, in)
	}
	if len(g.Log) != 2 || !g.Log[0].At.Equal(at) || g.Log[1].Text != "warming up" || !g.Log[1].Stderr {
		t.Errorf("log: %+v", g.Log)
	}
}

// A row's mark: the spinner while it starts, a green dot once connected, a
// yellow hollow circle when not (USER 2026-10-25).
func TestServerView_ConnMark(t *testing.T) {
	v := &mcpServersView{spin: "⣾ "} // spinner.Dot frames end in a space
	for _, tc := range []struct {
		st   dun.ServerState
		want string
	}{
		{dun.ServerState{Starting: true, Running: true}, "⣾"},
		{dun.ServerState{Running: true}, stOK.Render("●")},
		{dun.ServerState{Hosted: true}, stOK.Render("●")},
		{dun.ServerState{}, stDisconnected.Render("○")},
	} {
		if got := v.connMark(tc.st); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.st, got, tc.want)
		}
	}
}

// The detail shows the newest log lines, stderr marked.
func TestServerBody_Activity(t *testing.T) {
	var log []dun.ServerLogLine
	for i := range 12 {
		log = append(log, dun.ServerLogLine{At: time.Now(), Text: "step " + string(rune('a'+i))})
	}
	log = append(log, dun.ServerLogLine{At: time.Now(), Text: "boom", Stderr: true})
	body := ansi.Strip(serverBody(dun.ServerState{ID: "x", Log: log}, ""))
	if !strings.Contains(body, "activity:") || !strings.Contains(body, "stderr: boom") || strings.Contains(body, "step a") {
		t.Errorf("body:\n%s", body)
	}
}
