package main

import (
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// The same block, enough copies for the guard, as stream deltas.
func exactLoop(w llm.StreamWatcher) *llm.RepetitionInfo {
	block := "the parcel runs north along the east line of lot seven.\n"
	for i := 0; i < 40; i++ {
		if r := w.Watch(llm.ChannelContent, block); r != nil {
			return r
		}
	}
	return nil
}

func toolLoop(l *loopWatcher, c *llm.Client) *llm.RepetitionInfo {
	var h []llm.Message
	for i := 0; i < 13; i++ {
		tc := llm.ToolCall{ID: "c" + strings.Repeat("x", i)}
		tc.Function.Name, tc.Function.Arguments = "read", `{"path":"a"}`
		h = append(h, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{tc}}, llm.Message{Role: "tool", ToolCallID: tc.ID, Content: "same"})
	}
	tw := l.begin(c, h).(llm.ToolCallWatcher)
	return tw.WatchToolCall("read", `{"path":"a"}`)
}

// count finds loops and cuts only what the guard cuts: Install's detector would
// replace the guard, and a negative Tolerance would then cut nothing at all.
func TestLoops_CountKeepsTheGuard(t *testing.T) {
	l := &loopWatcher{}
	c := llm.NewClient("http://x/v1", "", "m")
	t.Setenv("DUN_LOOP_DETECT", "")
	l.install(c, loopConfig{})
	if r := exactLoop(c.Watch(nil)); r == nil {
		t.Error("the same bytes back to back were not cut")
	}
	if r := toolLoop(l, c); r != nil {
		t.Errorf("a tool loop was cut: %v", r)
	}
	if d := l.d.Counters().Detected; d["tool"] == 0 {
		t.Errorf("the tool loop was not counted: %v", d)
	}
}

func TestLoops_CutAndOff(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("DUN_LOOP_DETECT", "")
	l := &loopWatcher{}
	c := llm.NewClient("http://x/v1", "", "m")
	l.install(c, loopConfig{})
	msg, save, err := l.command("cut")
	if err != nil || save == nil || save.Loops.Mode != loopsCut || !strings.Contains(msg, "cutting at 3") {
		t.Fatalf("cut: %q %+v %v", msg, save, err)
	}
	if r := toolLoop(l, c); r == nil {
		t.Error("cut: a tool loop was not cut")
	}
	if _, _, err := l.command("tools count"); err != nil {
		t.Fatal(err)
	}
	if r := toolLoop(l, c); r != nil {
		t.Errorf("tools count: a tool loop was cut: %v", r)
	}
	if _, _, err := l.command("off"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Watch(nil).(llm.ToolCallWatcher); ok {
		t.Error("off: still watching tool calls")
	}
	if r := exactLoop(c.Watch(nil)); r == nil {
		t.Error("off: the guard went too")
	}
}

func TestLoops_Command(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("DUN_LOOP_DETECT", "")
	l := &loopWatcher{}
	l.load(loopConfig{})
	if msg, save, err := l.command(""); err != nil || save != nil || !strings.HasPrefix(msg, "loops: counting") {
		t.Fatalf("status: %q %v", msg, err)
	}
	if _, save, err := l.command("tolerance 5"); err != nil || save.Loops.Tolerance != 5 {
		t.Fatalf("tolerance: %+v %v", save, err)
	}
	if _, save, err := l.command("tolerance default"); err != nil || save.Loops.Tolerance != 0 {
		t.Fatalf("default: %+v %v", save, err)
	}
	for _, bad := range []string{"tolerance 1", "tools soon", "fast", "cut now"} {
		if _, _, err := l.command(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

// The environment wins at start, and a mode that is not one is ignored.
func TestLoops_Load(t *testing.T) {
	t.Setenv("DUN_LOOP_DETECT", "cut")
	l := &loopWatcher{}
	l.load(loopConfig{Mode: loopsOff})
	if l.cfg.mode() != loopsCut {
		t.Errorf("env: %q", l.cfg.mode())
	}
	t.Setenv("DUN_LOOP_DETECT", "")
	l = &loopWatcher{}
	l.load(loopConfig{Mode: "sometimes"})
	if l.cfg.mode() != loopsCount {
		t.Errorf("junk: %q", l.cfg.mode())
	}
}
