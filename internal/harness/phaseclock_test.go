package harness

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/proto"
)

// Start marks its phases on the clock it is given: a slow built-in server
// is the time of the "built-in servers" phase, up to the grace.
func TestStart_MarksItsPhases(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	lsp, _ := fakeBin(t, "poly-lsp-mcp", "poly-lsp", fakeSlowEnv+"=3000")
	old := serverStartGrace
	serverStartGrace = 300 * time.Millisecond
	t.Cleanup(func() { serverStartGrace = old })
	clk := NewPhaseClock(time.Now())
	h, err := Start(context.Background(), Config{
		Workspace: ws, Client: &scriptRunner{}, SessionFile: filepath.Join(ws, "s.jsonl"), StartClock: clk,
		Servers: []mcpserver.Server{{ID: mcpserver.ServerCode, Command: lsp, Args: []string{"mcp", "--root", ws}, Timeout: 20, Autostart: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	tm := clk.Timing()
	i := slices.IndexFunc(tm.Phases, func(p proto.Phase) bool { return p.Name == "built-in servers" })
	if i < 0 || tm.Phases[i].MS < 250 {
		t.Fatalf("start phases %+v: want built-in servers ~300ms", tm.Phases)
	}
	for _, want := range []string{"config", "session", "model's context window", "tools", "sub-agents"} {
		if !slices.ContainsFunc(tm.Phases, func(p proto.Phase) bool { return p.Name == want }) {
			t.Errorf("start phases %+v lack %q", tm.Phases, want)
		}
	}
}

// A clock adds up a phase marked twice, keeps the order phases ran in,
// and is nil-safe.
func TestPhaseClock(t *testing.T) {
	var nilClock *PhaseClock
	nilClock.Mark("x")
	if nilClock.Timing() != nil {
		t.Fatal("a nil clock has a timing")
	}
	c := NewPhaseClock(time.Now().Add(-time.Second))
	c.Mark("a")
	time.Sleep(20 * time.Millisecond)
	c.Mark("b")
	c.Mark("a")
	tm := c.Timing()
	if tm.TotalMS < 1000 || len(tm.Phases) != 2 || tm.Phases[0].Name != "a" || tm.Phases[1].Name != "b" || tm.Phases[1].MS < 15 {
		t.Fatalf("timing %+v", tm)
	}
}
