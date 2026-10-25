package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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

// /mcp is a screen: the table over the selected server's log, the input
// gone, the rows the terminal has; tab moves to the log, esc there pops it
// and the input is back (USER 2026-10-25).
func TestMCPScreen_TableOverTheLog(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	at := time.UnixMilli(time.Date(2026, 10, 25, 1, 53, 43, 0, time.Local).UnixMilli())
	list := []any{map[string]any{"id": "sh", "running": false, "starting": true, "log": []any{
		map[string]any{"at": float64(at.UnixMilli()), "text": "running sh slow.sh"},
		map[string]any{"at": float64(at.UnixMilli()), "text": "slow: loading config", "stderr": true},
	}}}
	m.mcpServersWant = true
	if !m.openServerTable(list) {
		t.Fatal("the table did not open")
	}
	view := m.drawView()
	plain := ansi.Strip(view)
	for _, want := range []string{"mcp servers", "activity · sh", "01:53:43 running sh slow.sh", "stderr: slow: loading config"} {
		if !strings.Contains(plain, want) {
			t.Errorf("screen lacks %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "ask dun to do something") {
		t.Errorf("the input is still there:\n%s", plain)
	}
	if n := strings.Count(view, "\n") + 1; n != 24 {
		t.Errorf("%d rows, want 24", n)
	}
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = nm.(tuiModel)
	if m.scr == nil || m.scr.focus != 1 || !strings.Contains(ansi.Strip(m.scr.hint(&m)), "end follows") {
		t.Fatalf("tab did not move to the log")
	}
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = nm.(tuiModel)
	if m.activeScreen() != nil || m.paging {
		t.Fatal("esc in the log did not pop the screen")
	}
	if !strings.Contains(ansi.Strip(m.drawView()), "ask dun to do something") {
		t.Error("the input did not come back")
	}
}

func TestScreen_Heights(t *testing.T) {
	s := mcpScreen()
	for _, rows := range []int{5, 8, 23, 60} {
		hs := s.heights(rows)
		if rows >= 8 && hs[0]+hs[1]+1 != rows {
			t.Errorf("%d rows: %v and a divider", rows, hs)
		}
		if hs[0] < 4 || hs[1] < 3 {
			t.Errorf("%d rows: %v under the minimums", rows, hs)
		}
	}
}
