package harness

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
	// Agent is the sub-agent an agent call acts on (args.id); 0 for a
	// spawn, whose number ToolEnd carries, and for any other tool.
	Agent int
}

// ToolEnd is a tool call's result, as the person is shown it (the capped
// form, cappedReporter), whether it failed, and how long it ran.
type ToolEnd struct {
	CallID  string
	Tool    string
	Result  string
	IsError bool
	Elapsed time.Duration
	// Agent is the sub-agent an agent call started or acted on (withAgent
	// notes it); 0 for any other call and for one that reached no agent.
	Agent int
	// Edits are the files the call changed (withEdits, fileedits.go).
	Edits []FileEdit
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
	agent    int        // the sub-agent the call started or acted on (noteAgent)
	edits    []FileEdit // the files the call changed (noteEdits)
}

// noteEdits records the files the call being dispatched changed, for its
// tool_result event.
func (c *toolCur) noteEdits(e []FileEdit) {
	c.mu.Lock()
	c.edits = e
	c.mu.Unlock()
}

func (c *toolCur) takeEdits() []FileEdit {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.edits
	c.edits = nil
	return e
}

func (c *toolCur) report(result string) {
	c.mu.Lock()
	c.reported, c.has = result, true
	c.mu.Unlock()
}

// noteAgent records the sub-agent the call being dispatched reached, for
// its tool_result event.
func (c *toolCur) noteAgent(n int) {
	c.mu.Lock()
	c.agent = n
	c.mu.Unlock()
}

func (c *toolCur) takeAgent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.agent
	c.agent = 0
	return n
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
			start(ToolStart{CallID: tc.ID, Tool: name, Args: args, Agent: agentArgID(name, args)})
		}
		h.toolCur.take() // nothing left over from a call that never ended
		h.toolCur.takeAgent()
		h.toolCur.takeEdits()
		t0 := time.Now()
		out, err := inner(ctx, tc)
		shown, ok := h.toolCur.take()
		agentNum := h.toolCur.takeAgent()
		edits := h.toolCur.takeEdits()
		if !ok {
			shown = out
		}
		failed := err != nil
		if err != nil && shown == "" {
			shown = "ERROR: " + err.Error()
		}
		if end != nil {
			end(ToolEnd{CallID: tc.ID, Tool: name, Result: shown,
				IsError: failed || ToolFailed(name, shown), Elapsed: time.Since(t0), Agent: agentNum,
				Edits: edits})
		}
		return out, err
	}
}

// agentArgID is the sub-agent an agent call names (its id argument), or 0:
// a spawn names none, and no other tool is an agent call.
func agentArgID(tool string, args map[string]any) int {
	if tool != "agent" {
		return 0
	}
	if f, ok := args["id"].(float64); ok && f > 0 && f == float64(int(f)) {
		return int(f)
	}
	return 0
}
