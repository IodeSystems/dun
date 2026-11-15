package dun

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// A tool call is two events with one id: ToolStart when dispatch begins, so
// a client shows the tool running, and ToolEnd when it returns. Clients pair
// them by CallID (the id the model gave the call, which history's tool_call
// and tool_result items carry too); they do not guess which call a result is
// for.

// ToolStart is a tool call beginning.
type ToolStart struct {
	CallID string
	Tool   string
	Args   map[string]any
}

// ToolEnd is a tool call's result, as the person is shown it (the capped
// form, cappedReporter), whether it failed, and how long it ran.
type ToolEnd struct {
	CallID  string
	Tool    string
	Result  string
	IsError bool
	Elapsed time.Duration
}

// ToolFailed reads whether a result is a failure: exec's own exit marker,
// or the ERROR a dispatcher returns for a call that did not run.
func ToolFailed(tool, result string) bool {
	r := strings.TrimSpace(result)
	if tool == "exec" {
		lines := strings.Split(r, "\n")
		if strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "[exit:") {
			return true
		}
	}
	return strings.HasPrefix(r, "ERROR") || strings.HasPrefix(r, "error:")
}

// toolCur is the call being dispatched: what the reporter fills with the
// shown result. Dispatch is one call at a time (agentkit's turn loop).
type toolCur struct {
	mu       sync.Mutex
	reported string
	has      bool
}

func (c *toolCur) report(result string) {
	c.mu.Lock()
	c.reported, c.has = result, true
	c.mu.Unlock()
}

func (c *toolCur) take() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.reported, c.has
	c.reported, c.has = "", false
	return r, ok
}

// withToolEvents is outside every other wrapper: every dispatched call
// starts and ends, even one a gate refused without reporting it.
func withToolEvents(inner agent.ToolDispatcher, h *Harness) agent.ToolDispatcher {
	start, end := h.cfg.OnToolStart, h.cfg.OnToolEnd
	if start == nil && end == nil {
		return inner
	}
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		name := tc.Function.Name
		if start != nil {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			start(ToolStart{CallID: tc.ID, Tool: name, Args: args})
		}
		h.toolCur.take() // nothing left over from a call that never ended
		t0 := time.Now()
		out, err := inner(ctx, tc)
		shown, ok := h.toolCur.take()
		if !ok {
			shown = out
		}
		failed := err != nil
		if err != nil && shown == "" {
			shown = "ERROR: " + err.Error()
		}
		if end != nil {
			end(ToolEnd{CallID: tc.ID, Tool: name, Result: shown,
				IsError: failed || ToolFailed(name, shown), Elapsed: time.Since(t0)})
		}
		return out, err
	}
}
