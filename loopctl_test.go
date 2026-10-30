package dun

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeLoops struct {
	calls []string
	err   error
}

func (f *fakeLoops) LoopCtl(_ context.Context, nextIn time.Duration, stop bool, reason string) (LoopState, error) {
	if f.err != nil {
		return LoopState{}, f.err
	}
	f.calls = append(f.calls, nextIn.String()+"|"+map[bool]string{true: "stop", false: "go"}[stop]+"|"+reason)
	return LoopState{ID: 4, Next: time.Now().Add(nextIn), Stopped: stop}, nil
}

// loop_ctl is listed from the first request for a root under dund: a tool
// that joined when a loop began changed the prompt's prefix, a full re-read
// (USER 2026-10-28).
func TestLoopCtl_ListedForARootUnderDund(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), Servers: []Server{}, Peers: &fakePeers{}, Loops: &fakeLoops{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if !slices.Contains(h.ToolNames(), "loop_ctl") {
		t.Fatalf("not listed: %v", h.ToolNames())
	}
	// A child never has it: a loop fires into the root.
	parent := testHarness(t)
	parent.cfg.Loops = &fakeLoops{}
	if childConfig(parent, 1, childRun{}, isolation{}).Loops != nil {
		t.Error("a child inherits the loop link")
	}
}

func TestLoopCtl_Calls(t *testing.T) {
	f := &fakeLoops{}
	d := withLoopCtl(nil, f, nil)
	out, _ := d(context.Background(), peerCall("loop_ctl", `{"next_in":"15m","reason":"build still running"}`))
	if !strings.HasPrefix(out, "loop 4: next run in 15m") || f.calls[0] != "15m0s|go|build still running" {
		t.Errorf("next_in: %q %v", out, f.calls)
	}
	out, _ = d(context.Background(), peerCall("loop_ctl", `{"stop":true,"reason":"done"}`))
	if out != "loop 4 stopped." || f.calls[1] != "0s|stop|done" {
		t.Errorf("stop: %q %v", out, f.calls)
	}
	out, _ = d(context.Background(), peerCall("loop_ctl", `{"next_in":"soon"}`))
	if !strings.HasPrefix(out, "ERROR: next_in \"soon\"") || len(f.calls) != 2 {
		t.Errorf("bad next_in: %q %v", out, f.calls)
	}
}
