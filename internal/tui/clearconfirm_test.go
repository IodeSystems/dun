package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// clearVtui is a session that records what the TUI writes to the engine.
func clearVtui(t *testing.T) (*vtui, *bytes.Buffer) {
	t.Helper()
	v := newVtui(120, 30)
	rec := &bytes.Buffer{}
	v.m.proc = &dunProc{stdin: bufCloser{rec}}
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	rec.Reset()
	return v, rec
}

func clearConfirmEv(ts string, jobs, plan bool) map[string]any {
	ev := map[string]any{"type": "clear_confirm", "ts": ts, "message": "not cleared: …"}
	if jobs {
		ev["jobs"] = []any{map[string]any{"id": 3.0, "command": "go test ./...", "state": "running", "started": float64(time.Now().Unix() - 120)}}
	}
	if plan {
		ev["plan"] = []any{
			map[string]any{"kind": "ask", "id": "2", "title": "fix the flaky test"},
			map[string]any{"kind": "node", "id": "n4", "title": "write the docs"},
		}
	}
	return ev
}

// Jobs and plan: the list is shown, then one question per category; the
// answers go back as one slash line.
func TestClearConfirm_AsksBothThenSends(t *testing.T) {
	v, rec := clearVtui(t)
	v.event(clearConfirmEv(nowTS(), true, true))
	view := v.view()
	for _, want := range []string{"job #3 go test ./... (2m)", "ask 2 fix the flaky test", "plan n4 write the docs",
		"/clear: 1 background job running — s stop · k keep · esc cancel"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	v.m = key(v.m, runesKey([]rune("s")))
	if rec.Len() != 0 {
		t.Fatalf("sent before the last answer: %q", rec.String())
	}
	if !strings.Contains(v.view(), "/clear: 2 open ask/plan items — d drop · k keep · esc cancel") {
		t.Fatalf("the plan question should follow:\n%s", v.view())
	}
	v.m = key(v.m, runesKey([]rune("k")))
	if got := rec.String(); !strings.Contains(got, `"id":"slash"`) || !strings.Contains(got, `"action":"clear jobs=stop plan=keep"`) {
		t.Fatalf("sent %q, want /clear jobs=stop plan=keep", got)
	}
	if v.m.confirm.q != "" {
		t.Error("the question must close after the answer")
	}
}

// Plan only: one question; esc cancels and sends nothing.
func TestClearConfirm_PlanOnlyAndCancel(t *testing.T) {
	v, rec := clearVtui(t)
	v.event(clearConfirmEv(nowTS(), false, true))
	if !strings.Contains(v.view(), "/clear: 2 open ask/plan items — d drop") {
		t.Fatalf("plan question missing:\n%s", v.view())
	}
	v.m = key(v.m, kEsc)
	if rec.Len() != 0 || v.m.confirm.q != "" {
		t.Fatalf("esc must cancel: sent %q, pending %q", rec.String(), v.m.confirm.q)
	}
	if !strings.Contains(v.convo(), "not done: /clear") {
		t.Errorf("the cancel should say so:\n%s", v.convo())
	}

	v.event(clearConfirmEv(nowTS(), false, true))
	v.m = key(v.m, runesKey([]rune("d")))
	if got := rec.String(); !strings.Contains(got, `"action":"clear plan=drop"`) {
		t.Fatalf("sent %q, want /clear plan=drop", got)
	}
}

// A replayed question (an attach after the fact) is not asked again.
func TestClearConfirm_ReplayNotAsked(t *testing.T) {
	v, _ := clearVtui(t)
	v.event(clearConfirmEv(time.Now().Add(-time.Minute).Format(time.RFC3339Nano), true, true))
	if v.m.confirm.q != "" || strings.Contains(v.view(), "outlive") {
		t.Errorf("a replayed clear_confirm was asked:\n%s", v.view())
	}
}
