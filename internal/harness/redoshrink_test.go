package harness

import "testing"

func TestRedoAfterShrink_Basic(t *testing.T) {
	// A session where:
	//   Round 1: call A (file_read path=a)
	//   Round 2: call B (file_read path=b)
	//   Round 5: shrink (fold)
	//   Round 6: call A again (file_read path=a)  → repeat
	//   Round 7: call C (file_read path=c)         → new
	//   Round 8: call B again (file_read path=b)  → repeat
	//   Round 30: call D (file_read path=d)        → outside window
	shrinks := []ShrinkAt{{Round: 5, Cause: "fold"}}
	calls := []ToolCallAt{
		{Round: 1, Tool: "file_read", Key: `{"path":"a"}`},
		{Round: 2, Tool: "file_read", Key: `{"path":"b"}`},
		{Round: 6, Tool: "file_read", Key: `{"path":"a"}`},
		{Round: 7, Tool: "file_read", Key: `{"path":"c"}`},
		{Round: 8, Tool: "file_read", Key: `{"path":"b"}`},
		{Round: 30, Tool: "file_read", Key: `{"path":"d"}`},
	}
	res := RedoAfterShrink(shrinks, calls)

	if res.TotalShrinks != 1 {
		t.Fatalf("TotalShrinks = %d, want 1", res.TotalShrinks)
	}
	// Window is rounds 5..24. Calls in window: rounds 6,7,8 = 3 calls.
	if res.TotalCalls != 3 {
		t.Fatalf("TotalCalls = %d, want 3", res.TotalCalls)
	}
	// Repeats: round 6 (path a, first at 1 < 6) and round 8 (path b, first at 2 < 8) = 2.
	if res.Repeats != 2 {
		t.Fatalf("Repeats = %d, want 2", res.Repeats)
	}
	want := 2.0 / 3.0
	if res.Share != want {
		t.Fatalf("Share = %v, want %v", res.Share, want)
	}
	fold := res.PerCause["fold"]
	if fold.Shrinks != 1 || fold.Calls != 3 || fold.Repeats != 2 || fold.Share != want {
		t.Errorf("PerCause[fold] = %+v, want Shrinks=1 Calls=3 Repeats=2 Share=%v", fold, want)
	}
}

func TestRedoAfterShrink_MultipleKinds(t *testing.T) {
	shrinks := []ShrinkAt{
		{Round: 10, Cause: "fold"},
		{Round: 20, Cause: "rehead"},
	}
	calls := []ToolCallAt{
		// Before fold
		{Round: 1, Tool: "exec", Key: `{"command":"ls"}`},
		// In fold window (10..29): exec ls again → repeat; exec pwd → new
		{Round: 12, Tool: "exec", Key: `{"command":"ls"}`},
		{Round: 13, Tool: "exec", Key: `{"command":"pwd"}`},
		// In rehead window (20..39): exec pwd again → repeat (first at 13 < 25)
		{Round: 25, Tool: "exec", Key: `{"command":"pwd"}`},
	}
	res := RedoAfterShrink(shrinks, calls)

	// The windows overlap: the fold window (rounds 10..29) also contains the
	// call at 25, which sits in the rehead window (20..39) too.
	// Fold: calls at 12 (ls, first at 1 → repeat), 13 (pwd, new), 25 (pwd,
	// first at 13 → repeat) = 3 calls, 2 repeats. Rehead: call at 25 = 1
	// call, 1 repeat. TotalCalls sums both windows = 4, Repeats = 3.
	if res.TotalCalls != 4 {
		t.Fatalf("TotalCalls = %d, want 4", res.TotalCalls)
	}
	if res.Repeats != 3 {
		t.Fatalf("Repeats = %d, want 3", res.Repeats)
	}
	fold := res.PerCause["fold"]
	if fold.Calls != 3 || fold.Repeats != 2 {
		t.Errorf("PerCause[fold] = %+v, want Calls=3 Repeats=2", fold)
	}
	rehead := res.PerCause["rehead"]
	if rehead.Calls != 1 || rehead.Repeats != 1 {
		t.Errorf("PerCause[rehead] = %+v, want Calls=1 Repeats=1", rehead)
	}
}

func TestRedoAfterShrink_NoShrinks(t *testing.T) {
	calls := []ToolCallAt{{Round: 1, Tool: "x", Key: "y"}}
	res := RedoAfterShrink(nil, calls)
	if res.TotalShrinks != 0 || res.TotalCalls != 0 || res.Share != 0 {
		t.Errorf("no shrinks: got %+v", res)
	}
}

func TestNormalizeArgs_SortedKeys(t *testing.T) {
	a := NormalizeArgs(`{"path":"a","offset":1}`)
	b := NormalizeArgs(`{"offset":1,"path":"a"}`)
	if a != b {
		t.Errorf("NormalizeArgs not order-insensitive:\n  a=%s\n  b=%s", a, b)
	}
}

func TestNormalizeArgs_Nested(t *testing.T) {
	a := NormalizeArgs(`{"outer":{"z":1,"a":2},"x":[]}`)
	b := NormalizeArgs(`{"x":[],"outer":{"a":2,"z":1}}`)
	if a != b {
		t.Errorf("nested normalization differs:\n  a=%s\n  b=%s", a, b)
	}
}
