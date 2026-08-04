package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
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
func TestBeginTurn_BoundsTheTurnNotTheSession(t *testing.T) {
	sessionCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	turnTimeout = 20 * time.Millisecond
	defer func() { turnTimeout = 0 }()

	tctx, end := beginTurn(sessionCtx)
	defer end()
	select {
	case <-tctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("turn context never expired")
	}
	if sessionCtx.Err() != nil {
		t.Fatal("the turn's deadline killed the session context")
	}
	// The next turn gets a live context, which is the whole point.
	next, nend := beginTurn(sessionCtx)
	defer nend()
	if next.Err() != nil {
		t.Fatalf("the turn after a timeout started dead: %v", next.Err())
	}
}

// With no per-turn budget, a turn is bounded only by the session (ctrl-C) —
// and must not shadow the run clock an ask needs to pause.
func TestBeginTurn_NoTimeoutFollowsTheSession(t *testing.T) {
	turnTimeout = 0
	run := newTurnClock(context.Background(), 0)
	curClock.Store(run)
	defer curClock.Store(nil)

	tctx, end := beginTurn(run.ctx)
	defer end()
	if curClock.Load() != run {
		t.Error("an unbudgeted turn must leave the run clock installed")
	}
	run.Stop()
	select {
	case <-tctx.Done():
	case <-time.After(time.Second):
		t.Fatal("ctrl-C did not reach the turn")
	}
}

// Time a human spends answering ask_user is not dun working. Under a plain
// deadline it was, so a question left open long enough killed the turn that
// asked it.
func TestTurnClock_PausedTimeIsNotCharged(t *testing.T) {
	turnTimeout = 120 * time.Millisecond
	defer func() { turnTimeout = 0 }()

	tctx, end := beginTurn(context.Background())
	defer end()

	// A "human" takes far longer than the whole budget to answer.
	if _, err := withoutClock(func() (string, error) {
		time.Sleep(300 * time.Millisecond)
		return "an answer", nil
	}); err != nil {
		t.Fatal(err)
	}
	if tctx.Err() != nil {
		t.Fatalf("thinking time was charged to the turn: %v", tctx.Err())
	}
	// The budget resumes where it left off, so the turn is still bounded.
	select {
	case <-tctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("budget never resumed — the turn is now unbounded")
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

// Bare /mcp answers the question no per-server command answers without typing
// all of them: which of these is even up. So a STOPPED server must still get a
// line — an absent one reads as "not configured", which is a different fact.
func TestServerListing_ShowsEveryServerNotJustRunningOnes(t *testing.T) {
	out := serverListing([]dun.ServerState{
		{ID: dun.ServerShell, Running: true, Tools: 4},
		{ID: dun.ServerDocs, Auto: true},
		{ID: dun.ServerCode, Err: "mcp: initialize code: transport closed\nsome-lsp: bad flag"},
	})
	for _, id := range []string{dun.ServerShell, dun.ServerDocs, dun.ServerCode} {
		if !strings.Contains(out, id) {
			t.Errorf("%s missing from the listing: %q", id, out)
		}
	}
	if !strings.Contains(out, "4 tools") {
		t.Errorf("a running server should report its tool count: %q", out)
	}
	if !strings.Contains(out, "autostart") {
		t.Errorf("autostart is what happens NEXT session — say so: %q", out)
	}
	if !strings.Contains(out, "bad flag") {
		t.Errorf("a server that failed to start must say why: %q", out)
	}
	if strings.Contains(out, "transport closed\nsome-lsp") {
		t.Errorf("failure detail should be flattened to one line: %q", out)
	}
}

// /mcp restart bounces what is RUNNING and leaves the rest alone. Both servers
// are opt-in by design (see dun.DefaultServers), so silently starting one that
// this session left off would cost a spawn and an index build nobody asked for.
func TestRunAllServersCmd_RestartsRunningOnesAndSkipsTheRest(t *testing.T) {
	if !haveBin("mcpshell") {
		t.Skip("mcpshell not on PATH")
	}
	ctx := context.Background()
	dir := t.TempDir()
	h, err := dun.Start(ctx, dun.Config{
		Workspace: dir,
		Servers: []dun.Server{
			{ID: dun.ServerShell, Command: "mcpshell", Args: []string{"mcp", "--files-dir", dir}, Timeout: 30},
			{ID: dun.ServerDocs, Command: "definitely-not-a-real-binary-xyz", Timeout: 5},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	if err := h.StartServer(ctx, dun.ServerShell); err != nil {
		t.Fatal(err)
	}
	before, _ := stateOf(h, dun.ServerShell)
	if !before.Running || before.Tools == 0 {
		t.Fatalf("shell should be up with tools before the restart: %+v", before)
	}

	out := runServerCmd(ctx, h, allServers, "restart")

	if !strings.Contains(out, "restarted") {
		t.Errorf("the running server should report a restart: %q", out)
	}
	if !strings.Contains(out, "skipped (not running)") || !strings.Contains(out, dun.ServerDocs) {
		t.Errorf("the stopped server should be named as skipped, not silently ignored: %q", out)
	}
	// The point of a restart is a WORKING server on the other side, not just a
	// dead one: the tools have to come back.
	after, _ := stateOf(h, dun.ServerShell)
	if !after.Running || after.Tools != before.Tools {
		t.Errorf("shell should be back with its tools: before=%+v after=%+v", before, after)
	}
}

// Naming a server is explicit intent, so a stopped one is STARTED rather than
// refused — and the message has to say which happened, because "restarted" and
// "started" mean different things about what you were just looking at.
func TestRunServerCmd_RestartNamedStartsAStoppedServer(t *testing.T) {
	if !haveBin("mcpshell") {
		t.Skip("mcpshell not on PATH")
	}
	ctx := context.Background()
	dir := t.TempDir()
	h, err := dun.Start(ctx, dun.Config{
		Workspace: dir,
		Servers:   []dun.Server{{ID: dun.ServerShell, Command: "mcpshell", Args: []string{"mcp", "--files-dir", dir}, Timeout: 30}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	if st, _ := stateOf(h, dun.ServerShell); st.Running {
		t.Fatal("nothing autostarts, so shell should be down")
	}
	out := runServerCmd(ctx, h, dun.ServerShell, "restart")
	if !strings.Contains(out, "started") || strings.Contains(out, "restarted") {
		t.Errorf("a stopped server that was NAMED should report a start, not a restart: %q", out)
	}
	if st, _ := stateOf(h, dun.ServerShell); !st.Running {
		t.Error("naming a stopped server should start it")
	}
}

// An unknown action must not silently do nothing — and now has to advertise
// restart alongside the rest.
func TestRunServerCmd_UnknownActionListsRestart(t *testing.T) {
	if out := runServerCmd(context.Background(), nil, "lsp", "bounce"); !strings.Contains(out, "restart") {
		t.Errorf("the usage line should offer restart: %q", out)
	}
}

func haveBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
