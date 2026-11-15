package tui

import "testing"

// With round ids the engine says where a round's thinking ends
// (reasoning_end): the TUI closes the thought there and nowhere else, and a
// reasoning delta of another round opens a new thought.
func TestThinking_RoundsFromTheEngine(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("go")
	v.event(map[string]any{"type": "reasoning", "text": "# Thinking: plan", "round": 1.0})
	first := thinkIdx(v.m)
	// An error no longer guesses the end: the engine's event does.
	v.event(map[string]any{"type": "error", "error": "x"})
	if th, _ := v.m.liveThought(); th == nil {
		t.Fatal("a round-stamped thought ended before its reasoning_end")
	}
	v.event(map[string]any{"type": "reasoning_end", "round": 1.0})
	if v.m.liveThinking() >= 0 || v.m.convo[first].think.round != 1 {
		t.Fatal("reasoning_end did not close round 1's thought")
	}
	// Round 2 thinks, its reasoning_end is missed: round 3's delta still opens
	// a thought of its own.
	v.event(map[string]any{"type": "reasoning", "text": "two", "round": 2.0})
	v.event(map[string]any{"type": "reasoning", "text": "three", "round": 3.0})
	th, _ := v.m.liveThought()
	if th == nil || th.text != "three" || th.round != 3 {
		t.Fatalf("round 3's thinking: %+v", th)
	}
	// Cut mid-thought: the engine's reasoning_end says so before done.
	v.event(map[string]any{"type": "reasoning_end", "round": 3.0, "cut": true})
	v.event(map[string]any{"type": "done", "reason": "interrupted"})
	if !th.cut || th.live {
		t.Errorf("the cut thought: live %v cut %v", th.live, th.cut)
	}
}
