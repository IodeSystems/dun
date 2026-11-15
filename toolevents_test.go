package dun

import (
	"context"
	"errors"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// The start goes out before the tool runs, the end after, both with the
// call's id; the end carries the shown (reported) text and is_error.
func TestToolEventsBracketTheCall(t *testing.T) {
	h := newNoteHarness(t)
	var log []string
	var end ToolEnd
	h.cfg.OnToolStart = func(s ToolStart) { log = append(log, "start "+s.CallID+" "+s.Tool) }
	h.cfg.OnToolEnd = func(e ToolEnd) { log = append(log, "end "+e.CallID); end = e }
	inner := func(ctx context.Context, tc llm.ToolCall) (string, error) {
		log = append(log, "run")
		h.toolCur.report("shown")
		return "raw", nil
	}
	tc := llm.ToolCall{ID: "c1"}
	tc.Function.Name = "exec"
	tc.Function.Arguments = `{"command":"ls"}`
	out, err := withToolEvents(inner, h)(context.Background(), tc)
	if err != nil || out != "raw" {
		t.Fatalf("the model's result changed: %q %v", out, err)
	}
	if len(log) != 3 || log[0] != "start c1 exec" || log[1] != "run" || log[2] != "end c1" {
		t.Fatalf("order %v", log)
	}
	if end.Result != "shown" || end.IsError {
		t.Errorf("end %+v", end)
	}
	// Unreported and failing: the returned error is the result.
	fail := func(ctx context.Context, tc llm.ToolCall) (string, error) { return "", errors.New("boom") }
	_, _ = withToolEvents(fail, h)(context.Background(), tc)
	if !end.IsError || end.Result != "ERROR: boom" {
		t.Errorf("failed end %+v", end)
	}
}

func TestToolFailed(t *testing.T) {
	for _, c := range []struct {
		tool, res string
		want      bool
	}{{"exec", "out\n[exit: status 1]", true}, {"exec", "ok", false}, {"x", "ERROR: no", true}, {"x", "fine", false}} {
		if ToolFailed(c.tool, c.res) != c.want {
			t.Errorf("%s %q", c.tool, c.res)
		}
	}
}
