package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun"
)

// --rag=false (force off for this run) must be distinguishable from not passing
// --rag at all (use whatever /rag auto saved). A plain bool cannot say that.
func TestTristate_UnsetIsNotFalse(t *testing.T) {
	var rag, lsp tristate
	if got := autostartOverrides(rag, lsp); got != nil {
		t.Fatalf("nothing passed should override nothing, got %v", got)
	}
	if err := rag.Set("false"); err != nil {
		t.Fatal(err)
	}
	got := autostartOverrides(rag, lsp)
	if v, ok := got[dun.ServerDocs]; !ok || v {
		t.Fatalf("--rag=false should force docs off, got %v", got)
	}
	if _, ok := got[dun.ServerCode]; ok {
		t.Error("an unset --lsp must not be overridden")
	}
	if rag.String() != "false" {
		t.Errorf("String() must round-trip for procArgs: %q", rag.String())
	}
}

// The hint is the only thing that tells a user a tool family is missing —
// the alternative symptom is an agent that silently never searches.
func TestServerHint_NamesWhatIsOffAndHow(t *testing.T) {
	hint := serverHint([]dun.ServerState{
		{ID: dun.ServerShell, Running: true},
		{ID: dun.ServerDocs},
		{ID: dun.ServerCode, Err: "mcp: initialize code: transport closed\nsome-lsp: bad flag"},
	})
	if strings.Contains(hint, dun.ServerShell) {
		t.Error("a running server should not be in the hint")
	}
	if !strings.Contains(hint, "/rag on") || !strings.Contains(hint, "/rag auto") {
		t.Errorf("docs hint should name both commands: %q", hint)
	}
	if !strings.Contains(hint, "bad flag") {
		t.Errorf("a failed start must say why: %q", hint)
	}
	if strings.Contains(hint, "\ncode did not start") && strings.Count(hint, "\n") > 1 {
		t.Errorf("failure detail should be flattened to one line: %q", hint)
	}
}

// A turn timeout must not be a session timeout. The old behaviour applied
// --timeout to the whole run, so at the deadline every following turn failed
// instantly on the same dead context and the engine exited — while the UI was
// still advising "send a message to retry".
func TestTurnCtx_BoundsTheTurnNotTheSession(t *testing.T) {
	sessionCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	turnTimeout = 20 * time.Millisecond
	defer func() { turnTimeout = 0 }()

	tctx, tcancel := turnCtx(sessionCtx)
	defer tcancel()
	select {
	case <-tctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("turn context never expired")
	}
	if sessionCtx.Err() != nil {
		t.Fatal("the turn's deadline killed the session context")
	}
	// The next turn gets a live context, which is the whole point.
	next, ncancel := turnCtx(sessionCtx)
	defer ncancel()
	if next.Err() != nil {
		t.Fatalf("the turn after a timeout started dead: %v", next.Err())
	}
}

// With no per-turn timeout, a turn is bounded only by the session (ctrl-C).
func TestTurnCtx_NoTimeoutFollowsTheSession(t *testing.T) {
	turnTimeout = 0
	sessionCtx, cancel := context.WithCancel(context.Background())
	tctx, tcancel := turnCtx(sessionCtx)
	defer tcancel()
	if _, ok := tctx.Deadline(); ok {
		t.Error("turn should have no deadline of its own")
	}
	cancel()
	select {
	case <-tctx.Done():
	case <-time.After(time.Second):
		t.Fatal("ctrl-C did not reach the turn")
	}
}

// "the turn failed" and "the session is over" are different facts; the engine
// reports which, because the UI cannot tell from the error text.
func TestEmitTurnError_FatalOnlyWhenTheSessionIsGone(t *testing.T) {
	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf bytes.Buffer
	em := &emitter{w: &buf}

	emitTurnError(live, em, errors.New("context deadline exceeded"))
	if got := decodeEvent(t, buf.String()); got["fatal"] != false {
		t.Errorf("a turn timeout is not fatal to the session: %v", got)
	}

	buf.Reset()
	dead, dcancel := context.WithCancel(context.Background())
	dcancel()
	emitTurnError(dead, em, errors.New("context canceled"))
	if got := decodeEvent(t, buf.String()); got["fatal"] != true {
		t.Errorf("a dead session must be reported as fatal: %v", got)
	}
}

func decodeEvent(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &m); err != nil {
		t.Fatalf("bad event %q: %v", line, err)
	}
	return m
}
