package dun

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// exiterHarness is a session running the fake "exiter" as server ex.
func exiterHarness(t *testing.T, instance string) (*Harness, string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	cmd, logPath := fakeBin(t, "exiter", "exiter")
	ctx := context.Background()
	h, err := Start(ctx, Config{Workspace: t.TempDir(), Client: &scriptRunner{},
		Servers: []Server{{ID: "ex", Command: cmd, Autostart: true, Instance: instance}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	h.WaitServers(ctx)
	if !h.mgr.ServerStarted("ex") {
		t.Fatalf("ex did not start: %v", h.lastErr)
	}
	return h, logPath
}

// callAs is one model tool call through h's dispatcher wiring.
func callAs(t *testing.T, h *Harness, caller toolCaller, tool string) string {
	t.Helper()
	call := mcpDispatcher(caller, h.mcpTools(), nil, h.regain)
	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = tool, `{}`
	out, err := call(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A server that exits during a call is restarted, the call is not
// repeated, and the model is told why with the server's last stderr; the
// next call reaches the new process. Past the limit, dun stops restarting.
func TestRestart_ExitedServer(t *testing.T) {
	defer func(n int) { restartLimit = n }(restartLimit)
	restartLimit = 2
	h, logPath := exiterHarness(t, "")
	pid := callAs(t, h, h.mgr, "ex__pid")
	out := callAs(t, h, h.mgr, "ex__crash")
	for _, want := range []string{"ERROR: the ex server exited during this call. dun restarted it; the call was not run again.",
		"state from earlier calls is gone", "Retry, in smaller pieces", "panic: boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("the restart error lacks %q:\n%s", want, out)
		}
	}
	if n := countStarts(logPath); n != 2 {
		t.Errorf("starts: %d, want 2", n)
	}
	if c := strings.Count(strings.Join(fakeLog(logPath), "\n"), "call crash"); c != 1 {
		t.Errorf("the crashing call ran %d times; it must not be repeated", c)
	}
	if again := callAs(t, h, h.mgr, "ex__pid"); again == pid || strings.HasPrefix(again, "ERROR") {
		t.Errorf("after the restart: %q (first process %s)", again, pid)
	}
	logged := false
	for _, l := range h.ServerLog("ex") {
		logged = logged || strings.Contains(l.Text, "exited during a call: restarting it")
	}
	if !logged {
		t.Error("the restart is not in the server log")
	}
	callAs(t, h, h.mgr, "ex__crash") // the second restart: the limit
	out = callAs(t, h, h.mgr, "ex__crash")
	if !strings.Contains(out, "exited 3 times in 10m0s, so dun no longer restarts it") || !strings.Contains(out, "Binary: ") ||
		!strings.Contains(out, "version 1") {
		t.Errorf("past the limit:\n%s", out)
	}
	if n := countStarts(logPath); n != 3 {
		t.Errorf("starts past the limit: %d, want 3", n)
	}
	if h.mgr.ServerStarted("ex") {
		t.Error("still running past the limit")
	}
}

// A child's call to its parent's server restarts it in the parent; a
// child's own instance restarts in the child.
func TestRestart_ChildCall(t *testing.T) {
	for _, inst := range []string{"", InstanceAgent} {
		t.Run("instance="+inst, func(t *testing.T) {
			h, logPath := exiterHarness(t, inst)
			sa, err := h.spawnAgent(context.Background(), "look", "", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			waitIdle(t, sa)
			child := sa.h
			var caller toolCaller = child.mgr
			if child.ownMgr != nil {
				caller = childCaller{child}
			}
			if inst == InstanceAgent && !child.own["ex"] {
				t.Fatal("the child has no instance of its own")
			}
			before := countStarts(logPath)
			out := callAs(t, child, caller, "ex__crash")
			if !strings.Contains(out, "dun restarted it") {
				t.Fatalf("the child's restart error:\n%s", out)
			}
			if n := countStarts(logPath); n != before+1 {
				t.Errorf("starts: %d, want %d", n, before+1)
			}
			if again := callAs(t, child, caller, "ex__pid"); strings.HasPrefix(again, "ERROR") {
				t.Errorf("the child's next call: %q", again)
			}
			if inst == "" && strings.HasPrefix(callAs(t, h, h.mgr, "ex__pid"), "ERROR") {
				t.Error("the parent's call after the child's restart failed")
			}
		})
	}
}
