package dun

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerLog_CommandLineHidesCredentials(t *testing.T) {
	got := commandLine("srv", []string{"--root", "/w", "--api-key", "sk-1", "--token=abc", "-v"})
	if want := "srv --root /w --api-key … --token=… -v"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := logURL("https://u:p@host/mcp?key=1#x"); got != "https://host/mcp" {
		t.Errorf("logURL: %q", got)
	}
}

func logText(st ServerState) string {
	var b strings.Builder
	for _, l := range st.Log {
		b.WriteString(l.Text + "\n")
	}
	return b.String()
}

// A start says each step as it takes it, and the activity hook hears it.
func TestServerLog_AStartSaysEachStep(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	lsp, _ := fakeBin(t, "poly-lsp-mcp", "poly-lsp", fakeSlowEnv+"=300")
	var heard atomic.Int32
	h, err := Start(context.Background(), Config{
		Workspace: ws, Client: &scriptRunner{},
		OnServerActivity: func(string) { heard.Add(1) },
		Servers:          []Server{{ID: ServerCode, Command: lsp, Args: []string{"mcp", "--root", ws, "--api-key", "sk-1"}, Timeout: 20, Autostart: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.WaitServers(ctx)
	st, _ := serverState(h, ServerCode)
	log := logText(st)
	for _, want := range []string{"running " + lsp + " mcp --root " + ws + " --api-key …", "handshake done", "ready · "} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "sk-1") {
		t.Errorf("the log shows the key:\n%s", log)
	}
	if heard.Load() == 0 {
		t.Error("OnServerActivity never called")
	}
}

// A start that fails says why, with what the server said on stderr (the
// manager has no stderr for a process that is gone: the error carries it).
func TestServerLog_AFailedStartSaysWhy(t *testing.T) {
	h, err := Start(context.Background(), Config{
		Workspace: t.TempDir(),
		Servers: []Server{{ID: ServerDocs, Command: "sh", Timeout: 5, Autostart: true,
			Args: []string{"-c", "echo 'no project name' >&2; exit 1"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.WaitServers(ctx)
	st, _ := serverState(h, ServerDocs)
	if log := logText(st); !strings.Contains(log, "(timeout 5s)") || !strings.Contains(log, "did not start after") ||
		!strings.Contains(log, "no project name") {
		t.Errorf("log:\n%s", log)
	}
}

// A server's stderr joins the log as it writes it, while its handshake is
// still pending: the start that hangs (USER 2026-10-25).
func TestServerLog_StderrAsItComes(t *testing.T) {
	h, err := Start(context.Background(), Config{
		Workspace: t.TempDir(),
		Servers: []Server{{ID: ServerDocs, Command: "sh", Timeout: 3, Autostart: true,
			Args: []string{"-c", "echo 'warming up' >&2; sleep 2"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		st, _ := serverState(h, ServerDocs)
		for _, l := range st.Log {
			if l.Stderr && l.Text == "warming up" {
				return
			}
		}
	}
	st, _ := serverState(h, ServerDocs)
	t.Errorf("no stderr line while it starts:\n%s", logText(st))
}

// A slow step says it is still at it, with how long and the timeout.
func TestServerLog_ASlowStartSaysItIsStillAtIt(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	old := stillEvery
	stillEvery = 200 * time.Millisecond
	t.Cleanup(func() { stillEvery = old })
	ws := t.TempDir()
	lsp, _ := fakeBin(t, "poly-lsp-mcp", "poly-lsp", fakeSlowEnv+"=1200")
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
	st, _ := serverState(h, ServerCode)
	if log := logText(st); !strings.Contains(log, "still waiting for") || !strings.Contains(log, "of the 20s timeout") {
		t.Errorf("log:\n%s", log)
	}
}
