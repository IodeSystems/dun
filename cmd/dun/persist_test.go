package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
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

// The ready line names a handful of tools, not hundreds.
func TestReadyLineCapsTheToolNames(t *testing.T) {
	few := []string{"a", "b", "c"}
	if got := readyLine(few); got != "ready — 3 tools: a, b, c" {
		t.Errorf("few: %q", got)
	}
	var many []string
	for i := 0; i < 172; i++ {
		many = append(many, fmt.Sprintf("t%d", i))
	}
	got := readyLine(many)
	if !strings.HasPrefix(got, "ready — 172 tools: t0, t1, t2, t3, t4, t5, t6, t7 … +164 more") || strings.Contains(got, "t8,") {
		t.Errorf("many: %q", got)
	}
}
