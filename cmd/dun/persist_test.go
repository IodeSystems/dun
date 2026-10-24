package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/iodesystems/dun"
)

// /exit tells dund this client is leaving; /persist sends its mode; the
// answer reads as the user asked for it.
func TestPersistAndExit(t *testing.T) {
	var out bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&out}}, "/ws")
	m = resized(m, 80, 24)

	for _, c := range []struct{ line, want string }{
		{"/persist", `"mode":""`}, {"/persist off", `"mode":"off"`}, {"/persist status", `"mode":"status"`},
	} {
		out.Reset()
		m.runSlash(c.line)
		if !strings.Contains(out.String(), `"type":"persist"`) || !strings.Contains(out.String(), c.want) {
			t.Errorf("%s sent %s", c.line, out.String())
		}
	}
	out.Reset()
	m.runSlash("/persist maybe")
	if out.Len() != 0 {
		t.Errorf("a bad mode was sent: %s", out.String())
	}

	for _, c := range []struct {
		ev   map[string]any
		want string
	}{
		{map[string]any{"type": "persist", "on": true, "changed": true}, "/persist off to undo"},
		{map[string]any{"type": "persist", "on": true}, "/persist off to let /exit stop it"},
		{map[string]any{"type": "persist", "on": false}, "/persist to keep it running"},
	} {
		m = m.handleEvent(evMsg(c.ev))
		if !strings.Contains(convoText(m), c.want) {
			t.Errorf("%v: no %q in %q", c.ev, c.want, stripANSI(convoText(m)))
		}
	}

	out.Reset()
	if cmd := m.runSlash("/exit"); cmd == nil || !m.quitting {
		t.Fatal("/exit did not quit")
	}
	if !strings.Contains(out.String(), `"type":"leave"`) {
		t.Errorf("/exit sent %q, want a leave", out.String())
	}
}

// Ready leaves no tool list in the conversation, and the header is the
// folder and the model only: the tool count is in the splash and /status
// (with its label), and /mcp lists the tools.
func TestReadyNamesNoTools(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.url, m.model = "http://llm/v1", "m1"
	var tools []any
	for i := 0; i < 172; i++ {
		tools = append(tools, fmt.Sprintf("t%d", i))
	}
	m = m.handleEvent(evMsg{"type": "ready", "tools": tools})
	if txt := convoText(m); strings.Contains(txt, "t0") || strings.Contains(txt, "ready") {
		t.Errorf("ready wrote:\n%s", txt)
	}
	if h := stripANSI(m.headView()); h != "dun · ws · m1" {
		t.Errorf("header %q", h)
	}
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	if s := stripANSI(m.splashView()); !strings.Contains(s, "tools      1") {
		t.Errorf("splash:\n%s", s)
	}
}

// dund unreachable on the first attach: the conversation being resumed is
// shown from disk, read-only, and dund's later replay of it is skipped.
func TestOfflineShowsTheSavedConversation(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	dir := dun.RootDir(ws)
	os.MkdirAll(dir, 0o755)
	id := "20261022-120000"
	os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(`{"Kind":"user","Content":"what broke the build"}`+"\n"+
		`{"Kind":"assistant","Content":"the overlay is read first"}`+"\n"), 0o644)

	m := newTUIModel(nil, ws)
	m.opts = tuiOpts{workspace: ws, resume: id}
	m = resized(m, 80, 24)
	nm, _ := m.Update(engineUpMsg{err: errors.New("dund closed the connection")})
	m = nm.(tuiModel)
	out := stripANSI(convoText(m))
	if !strings.Contains(out, "what broke the build") || !strings.Contains(out, "read-only from disk") {
		t.Fatalf("saved conversation not shown:\n%s", out)
	}
	// A second failure does not show it twice.
	nm, _ = m.Update(engineUpMsg{err: errors.New("again")})
	m = nm.(tuiModel)
	if n := strings.Count(stripANSI(convoText(m)), "what broke the build"); n != 1 {
		t.Fatalf("shown %d times", n)
	}
	// dund comes back and replays the same history: skipped.
	m = m.handleEvent(evMsg{"type": "history", "items": []any{map[string]any{"kind": "user", "content": "what broke the build"}}})
	if n := strings.Count(stripANSI(convoText(m)), "what broke the build"); n != 1 {
		t.Fatalf("after dund's replay: shown %d times", n)
	}
}

// tab from the input lands on the newest block every time, not where the last
// visit left off. (← used to be the way in; arrows no longer change the zone,
// USER 2026-10-23.)
func TestTabFromInputSelectsTheNewest(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready"})
	for _, msg := range []string{"one", "two", "three"} {
		v.send(msg)
		v.event(map[string]any{"type": "done"})
	}
	newest := len(v.m.convo) - 1
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyTab})
	first := v.m.sel
	if v.m.focus != focusConvo || first < 0 {
		t.Fatalf("tab did not enter the conversation: focus=%d sel=%d", v.m.focus, first)
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyUp})
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyUp})
	if v.m.sel >= first {
		t.Fatalf("↑ did not move up: %d → %d", first, v.m.sel)
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyTab}) // back to the input
	if v.m.focus == focusConvo {
		v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyTab})
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyTab})
	if v.m.sel != first || first > newest {
		t.Errorf("second tab selected %d, want the newest (%d)", v.m.sel, first)
	}
}
