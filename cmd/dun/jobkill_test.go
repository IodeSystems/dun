package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

var (
	kX = runesKey([]rune("x"))
	kY = runesKey([]rune("y"))
)

// jobsVtui is a session with two jobs (one running, one done) and a record of
// what the TUI writes to the engine.
func jobsVtui(t *testing.T) (*vtui, *bytes.Buffer) {
	t.Helper()
	v := newVtui(100, 24)
	rec := &bytes.Buffer{}
	v.m.proc = &dunProc{stdin: bufCloser{rec}}
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.event(map[string]any{"type": "commands", "items": []any{
		map[string]any{"name": "jobs", "usage": "[kill <n>|kill all]", "description": "background jobs"},
	}})
	now := float64(time.Now().Unix())
	v.event(map[string]any{"type": "jobs", "jobs": []any{
		map[string]any{"id": 3.0, "command": "go test ./...", "state": "running", "started": now - 120},
		map[string]any{"id": 4.0, "command": "echo hi", "state": "ok", "started": now - 5, "ended": now - 4},
	}})
	rec.Reset()
	return v, rec
}

// x on a running job asks "kill job N? (y/n)" on the status line; y sends
// /jobs kill N, anything else sends nothing.
func TestTUI_XKillsSelectedJobAfterConfirm(t *testing.T) {
	v, rec := jobsVtui(t)
	v.m.setFocus(focusActivity) // focusing opens the table; the first row is job 3
	if !strings.Contains(v.view(), "x kill job") {
		t.Errorf("the pane's hints should name x:\n%s", v.view())
	}
	v.m = key(v.m, kX)
	if !strings.Contains(v.view(), "kill job 3? (y/n)") {
		t.Fatalf("x should ask first:\n%s", v.view())
	}
	if rec.Len() != 0 {
		t.Fatalf("nothing may be sent before the answer: %q", rec.String())
	}
	v.m = key(v.m, kN)
	if rec.Len() != 0 || v.m.confirm.q != "" {
		t.Fatalf("n must cancel: sent %q, pending %q", rec.String(), v.m.confirm.q)
	}

	v.m = key(v.m, kX)
	v.m = key(v.m, kY)
	if got := rec.String(); !strings.Contains(got, `"id":"slash"`) || !strings.Contains(got, `"action":"jobs kill 3"`) {
		t.Fatalf("y should send /jobs kill 3, sent %q", got)
	}
	if v.m.confirm.q != "" {
		t.Error("the question must close after the answer")
	}

	// A finished job is not killed, and nothing is asked.
	rec.Reset()
	v.m = key(v.m, kDown)
	v.m = key(v.m, kX)
	if v.m.confirm.q != "" || rec.Len() != 0 {
		t.Errorf("x on a finished job: pending %q, sent %q", v.m.confirm.q, rec.String())
	}
	if !strings.Contains(v.convo(), "job 4 already finished") {
		t.Errorf("x on a finished job should say so:\n%s", v.convo())
	}
}

// /jobs kill all asks first; /jobs kill 3 does not.
func TestTUI_JobsKillAllConfirms(t *testing.T) {
	v, rec := jobsVtui(t)
	v.m = typeStr(v.m, "/jobs kill all")
	v.m = key(v.m, kEnter)
	if !strings.Contains(v.view(), "kill every running job? (y/n)") || rec.Len() != 0 {
		t.Fatalf("kill all must ask first; sent %q\n%s", rec.String(), v.view())
	}
	v.m = key(v.m, kY)
	if !strings.Contains(rec.String(), `"action":"jobs kill all"`) {
		t.Fatalf("y should send /jobs kill all, sent %q", rec.String())
	}

	rec.Reset()
	v.m = typeStr(v.m, "/jobs kill 3")
	v.m = key(v.m, kEnter)
	if v.m.confirm.q != "" || !strings.Contains(rec.String(), `"action":"jobs kill 3"`) {
		t.Errorf("/jobs kill 3: pending %q, sent %q", v.m.confirm.q, rec.String())
	}
}

// The interrupted row is followed by the engine's list of jobs still running.
func TestTUI_InterruptedRowListsRunningJobs(t *testing.T) {
	v, _ := busyVtui(t)
	detail := "still running: #3 go test ./... (2m) — /jobs kill 3 to stop it"
	v.event(map[string]any{"type": "done", "reason": "interrupted", "detail": detail})
	c := v.convo()
	if !strings.Contains(c, interruptedRow) || !strings.Contains(c, detail) {
		t.Errorf("want the interrupted row and the jobs line:\n%s", c)
	}
}
