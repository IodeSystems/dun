package dun

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// --non-interactive: a turn that did work is checked once before it ends; a
// one-round answer is not. Interactive: nothing extra unless the plan is open.
func TestOnIdle(t *testing.T) {
	ctx := context.Background()
	h := planHarness(t, false)

	if got := h.onIdle(ctx, 5, 0); got != "" {
		t.Errorf("interactive, no plan: %q, want nothing", got)
	}
	h.cfg.NonInteractive = true
	if got := h.onIdle(ctx, 1, 0); got != "" {
		t.Errorf("one-round answer was checked: %q", got)
	}
	if got := h.onIdle(ctx, 5, 0); !strings.Contains(got, "list each requirement") || !strings.Contains(got, "do not end the turn: keep working") {
		t.Errorf("non-interactive work turn not checked, or the check reads as an order to finish: %q", got)
	}
	if got := h.onIdle(ctx, 6, 1); got != "" {
		t.Errorf("asked twice in one turn: %q", got)
	}

	h.cfg.NonInteractive = false
	planCall(t, h, `{"action":"add","title":"write the parser","done_when":"tests pass"}`)
	if got := h.onIdle(ctx, 3, 0); !strings.Contains(got, "Open plan nodes") || !strings.Contains(got, "write the parser") {
		t.Errorf("interactive with an open plan: %q", got)
	}
}

// The check lists each open node with its done_when, beside the requirement
// check; a done or dropped node is not listed. A child is not told.
func TestOnIdle_ListsOpenNodesDoneWhen(t *testing.T) {
	ctx := context.Background()
	h := planHarness(t, false)
	h.cfg.NonInteractive = true
	planCall(t, h, `{"action":"add","title":"write the parser","done_when":"go test ./parser passes"}`)
	planCall(t, h, `{"action":"add","title":"old idea","done_when":"never"}`)
	planCall(t, h, `{"action":"add","title":"read the spec","done_when":"spec read"}`)
	planCall(t, h, `{"action":"drop","id":"2","reason":"not needed"}`)
	planCall(t, h, `{"action":"update","id":"3","status":"doing"}`)
	if out := planCall(t, h, `{"action":"done","id":"3","account":"read it"}`); !strings.Contains(out, "done") {
		t.Fatalf("done: %s", out)
	}
	got := h.onIdle(ctx, 5, 0)
	for _, want := range []string{
		"list each requirement", // the requirement check stays
		"Open plan nodes:",
		"- 1 write the parser; done_when: go test ./parser passes",
		"Finish each, or mark it done with how done_when was met, or drop it with why.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, not := range []string{"old idea", "read the spec"} {
		if strings.Contains(got, not) {
			t.Errorf("a finished node is listed (%q):\n%s", not, got)
		}
	}
}

// After the check, an edit or a write re-arms it once, naming the change; an
// exec (the verification the check asks for) does not; no change, no second
// check; never a third.
func TestOnIdle_RearmsAfterALateChange(t *testing.T) {
	ctx := context.Background()
	h := planHarness(t, false)
	h.cfg.NonInteractive = true

	h.idle.note("file_write", `{"path":"/app/early.html"}`) // before any check: not a change since one
	if got := h.onIdle(ctx, 5, 0); !strings.Contains(got, "list each requirement") || strings.Contains(got, "after the last check") {
		t.Fatalf("first check: %q", got)
	}
	if got := h.onIdle(ctx, 6, 1); got != "" {
		t.Fatalf("no change since the check, checked again: %q", got)
	}
	h.idle.note("file_read", `{"path":"/app/out.html"}`)
	if got := h.onIdle(ctx, 6, 1); got != "" {
		t.Fatalf("a read is not a change: %q", got)
	}
	h.idle.note("file_edit", `{"path":"/app/out.html"}`)
	h.idle.note("file_write", `{"path":"/app/out.html"}`)
	got := h.onIdle(ctx, 7, 1)
	if !strings.HasPrefix(got, "You changed /app/out.html after the last check: check the requirements again against what is there now.\n\n") ||
		!strings.Contains(got, "list each requirement") || strings.Contains(got, "early.html") {
		t.Fatalf("second check: %q", got)
	}
	h.idle.note("exec", `{"command":"make"}`)
	if got := h.onIdle(ctx, 8, 2); got != "" {
		t.Fatalf("a third check: %q", got)
	}

	// A new turn: an exec alone after the check does not re-arm it.
	h.idle.reset()
	h.onIdle(ctx, 5, 0)
	h.idle.note("exec", `{"command":"python test.py"}`)
	if got := h.onIdle(ctx, 6, 1); got != "" {
		t.Errorf("an exec re-armed the check: %q", got)
	}
	h.idle.note("file_write", `{"path":"/app/a"}`)
	if got := h.onIdle(ctx, 6, 1); !strings.HasPrefix(got, "You changed /app/a after the last check:") {
		t.Errorf("write after exec: %q", got)
	}
}

// Through a session: check, write, second check naming the file, write,
// end. Six requests; the rearm text first rides the fifth.
func TestOnIdle_RearmInATurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.html")
	write := func(i int) *llm.ToolCall {
		return nudgeCall(fmt.Sprintf("w%d", i), "file_write", fmt.Sprintf(`{"path":%q,"content":"v%d"}`, path, i))
	}
	r := &nudgeRunner{reply: func(i int, _ bool) *llm.ToolCall {
		switch i {
		case 0, 2, 4:
			return write(i)
		}
		return nil
	}}
	t.Setenv("DUN_HOME", t.TempDir())
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []Server{}, Client: r, Exec: &HostShell{Dir: dir},
		SessionFile: filepath.Join(dir, "s.jsonl"), NonInteractive: true,
		PlanNudgeRounds: -1, PlanNudgeFails: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	if _, err := h.Ask(context.Background(), "write out.html"); err != nil {
		t.Fatal(err)
	}
	if n := r.calls(); n != 6 {
		t.Fatalf("%d requests, want 6", n)
	}
	rearm := "You changed " + path + " after the last check"
	for i := range 6 {
		if has := r.sentHas(i, rearm); has != (i >= 4) {
			t.Errorf("request %d: rearm text present %v", i, has)
		}
	}
	if !r.sentHas(2, "list each requirement") {
		t.Error("the first check is missing")
	}
}
