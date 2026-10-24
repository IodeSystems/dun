package dun

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A server that takes its time to answer (poly-lsp indexing a big repo took
// a minute live) does not hold the session: Start returns within the grace,
// the server is reported starting, a turn runs without its tools, and the
// tools join once it answers — reported through OnServerStarted.
func TestStart_SlowServerJoinsAfterReady(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	lsp, lspLog := fakeBin(t, "poly-lsp-mcp", "poly-lsp", fakeSlowEnv+"=3000")
	old := serverStartGrace
	serverStartGrace = 200 * time.Millisecond
	t.Cleanup(func() { serverStartGrace = old })
	reported := make(chan error, 1)
	t0 := time.Now()
	h, err := Start(context.Background(), Config{
		Workspace: ws, Client: &scriptRunner{}, SessionFile: filepath.Join(ws, "s.jsonl"),
		Servers: []Server{{ID: ServerCode, Command: lsp, Args: []string{"mcp", "--root", ws}, Timeout: 20, Autostart: true}},
		OnServerStarted: func(id string, err error) {
			if id == ServerCode {
				reported <- err
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("Start waited %s for a slow server", d)
	}
	hasTool := func() bool {
		return slices.ContainsFunc(h.ToolNames(), func(n string) bool { return strings.Contains(n, "node_query") })
	}
	state := func() ServerState {
		for _, st := range h.Servers() {
			if st.ID == ServerCode {
				return st
			}
		}
		t.Fatalf("no %s in Servers()", ServerCode)
		return ServerState{}
	}
	if st := state(); !st.Starting || st.Err != "" {
		t.Fatalf("a server mid-start is not reported starting: %+v", st)
	}
	if hasTool() {
		t.Fatal("the slow server's tools are in before it answered")
	}
	// Asked to start while it starts: not a second process.
	if err := h.StartServer(context.Background(), ServerCode); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Ask(context.Background(), "hello"); err != nil {
		t.Fatalf("a turn while a server starts: %v", err)
	}
	if !state().Starting {
		t.Fatal("the turn waited for the slow server")
	}
	select {
	case err := <-reported:
		if err != nil {
			t.Fatalf("the slow server did not start: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no report for the slow server")
	}
	if st := state(); st.Starting || !st.Running {
		t.Fatalf("after it answered: %+v", st)
	}
	if !hasTool() {
		t.Fatalf("its tools did not join: %v", h.ToolNames())
	}
	starts := 0
	for _, l := range fakeLog(lspLog) {
		if strings.HasPrefix(l, "start ") {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("the server was started %d times, want 1", starts)
	}
}

// A one-shot run's task waits for the servers still starting.
func TestWaitServers_WaitsForTheStartInFlight(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	lsp, _ := fakeBin(t, "poly-lsp-mcp", "poly-lsp", fakeSlowEnv+"=1500")
	old := serverStartGrace
	serverStartGrace = 100 * time.Millisecond
	t.Cleanup(func() { serverStartGrace = old })
	h, err := Start(context.Background(), Config{
		Workspace: ws, Client: &scriptRunner{},
		Servers: []Server{{ID: ServerCode, Command: lsp, Args: []string{"mcp", "--root", ws}, Timeout: 20, Autostart: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.WaitServers(ctx)
	if ctx.Err() != nil {
		t.Fatal("WaitServers did not return")
	}
	if !slices.ContainsFunc(h.ToolNames(), func(n string) bool { return strings.Contains(n, "node_query") }) {
		t.Fatalf("after WaitServers the tools are not in: %v", h.ToolNames())
	}
}
