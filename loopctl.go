package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// loop_ctl — the model's hand on a /loop (docs/scheduling.md).
//
// A loop is dund's (internal/dund/schedule.go): it fires a prompt into this
// session again and again. The model paces it: a self-paced loop asks it
// when to come back, and any loop can be stopped by it once the work is
// done. That is this tool.
//
// It is in the tool set ONLY while this session has a loop (SetLoops, from
// dund's loops command and from the replies to /loop itself). Every first
// request is measured, and a tool nobody can use is nothing but tokens in
// it; the same rule as jobs, which joins once something went to the
// background. The dispatch is always there, so a call after the last loop
// went still answers, saying so.

// LoopState is what a loop_ctl call left: the loop, and its next run or that
// it stopped.
type LoopState struct {
	ID      int
	Next    time.Time
	Stopped bool
}

// LoopLink is the way to dund's scheduler. Its errors are dund's refusals,
// worded for the model.
type LoopLink interface {
	LoopCtl(ctx context.Context, nextIn time.Duration, stop bool, reason string) (LoopState, error)
}

func loopCtlToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "loop_ctl"
	td.Function.Description = "Pace or stop the /loop that sent you its prompt: next_in sets when it runs next, " +
		"stop ends it. reason is shown to your user."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"next_in": map[string]any{"type": "string", "description": "time until the next run, e.g. \"15m\", \"2h\" (1m to 24h)"},
			"stop":    map[string]any{"type": "boolean", "description": "end the loop"},
			"reason":  map[string]any{"type": "string", "description": "a few words for your user"},
		},
	}
	return td
}

// withLoopCtl handles loop_ctl; everything else routes onward.
func withLoopCtl(inner agent.ToolDispatcher, link LoopLink, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "loop_ctl" {
			return inner(ctx, tc)
		}
		var args struct {
			NextIn string `json:"next_in"`
			Stop   bool   `json:"stop"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		out := loopCtlCall(ctx, link, args.NextIn, args.Stop, args.Reason, time.Now())
		if onCall != nil {
			onCall("loop_ctl", map[string]any{"next_in": args.NextIn, "stop": args.Stop, "reason": args.Reason}, out)
		}
		return out, nil
	}
}

// loopCtlCall is loop_ctl's result for one call.
func loopCtlCall(ctx context.Context, link LoopLink, nextIn string, stop bool, reason string, now time.Time) string {
	var d time.Duration
	if !stop {
		var err error
		if d, err = time.ParseDuration(strings.TrimSpace(nextIn)); err != nil || d <= 0 {
			return fmt.Sprintf("ERROR: next_in %q is not a duration such as \"15m\" (or pass stop:true)", nextIn)
		}
	}
	st, err := link.LoopCtl(ctx, d, stop, reason)
	switch {
	case err != nil:
		return "ERROR: " + err.Error()
	case st.Stopped:
		return fmt.Sprintf("loop %d stopped.", st.ID)
	}
	return fmt.Sprintf("loop %d: next run in %s, at %s.", st.ID, st.Next.Sub(now).Round(time.Second), st.Next.Local().Format("15:04"))
}
