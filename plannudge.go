package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"
)

// plannudge.go — one forced plan call, on a signal (USER 2026-11-07).
//
// Measured need: in 72 Terminal-Bench trials (--non-interactive), 20 ran 60
// or more rounds and 5 hit the 150-round cap; none used the plan. Telling the
// model in the prompt does not work (it ignored "use file_read, not cat" 179
// times), so the plan is asked for when a turn shows it needs one:
//
//   - rounds: the turn has run N rounds (--plan-nudge-rounds, 25) and the plan
//     has no open node;
//   - stall: the last K tool results failed (--plan-nudge-fails, 4) and the
//     plan has no open node. A failure is a result that starts "ERROR" (dun's
//     tools and MCP errors), a dispatcher error, or an exec that failed
//     (ExecResult.Failed: non-zero exit, spawn error, killed) or ran past its
//     timeout and became a job. Any other result ends the run.
//
// The response is on the next request: tool_choice forces a `plan` call (a
// heredoc transport gets a grammar over plan alone, as fork.go does), and an
// aside keyed "plan-nudge" says why, riding the same round's tool results.
//
// At most once per turn, the main agent only (a child owns a node already),
// and only while plan is in the tool set. An endpoint known not to honor a
// forced tool_choice (llm.Capabilities.ToolChoiceHonored), or that refuses
// the request, gets the aside alone: the request is sent again unforced, and
// the turn goes on.

const (
	defaultPlanNudgeRounds = 25
	defaultPlanNudgeFails  = 4
)

// planNudgeRounds is the rounds threshold, 0 when off: Config.PlanNudgeRounds
// (negative: off), else DUN_PLAN_NUDGE_ROUNDS, else 25.
func (h *Harness) planNudgeRounds() int {
	return nudgeSetting(h.cfg.PlanNudgeRounds, "DUN_PLAN_NUDGE_ROUNDS", defaultPlanNudgeRounds)
}

// planNudgeFails is the stall threshold, 0 when off: Config.PlanNudgeFails
// (negative: off), else DUN_PLAN_NUDGE_FAILS, else 4.
func (h *Harness) planNudgeFails() int {
	return nudgeSetting(h.cfg.PlanNudgeFails, "DUN_PLAN_NUDGE_FAILS", defaultPlanNudgeFails)
}

func nudgeSetting(v int, env string, def int) int {
	switch {
	case v < 0:
		return 0
	case v > 0:
		return v
	}
	if s := os.Getenv(env); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// planNudge is one turn's state; prepareTurn resets it.
type planNudge struct {
	mu    sync.Mutex
	fails int  // consecutive failed tool results
	done  bool // nudged in this turn
	force bool // the next request forces a plan call
}

func (n *planNudge) reset() {
	n.mu.Lock()
	n.fails, n.done, n.force = 0, false, false
	n.mu.Unlock()
}

// The aside's text. Model-facing.
const (
	planNudgeRoundsText = "[plan] This task has run %d rounds with no plan. "
	planNudgeFailsText  = "[plan] This task has had %d failed runs in a row and no plan. "
	planNudgeAsk        = "Write the plan now: what is done, what is left, and done_when for each node. Then go on with the work."
)

// planNudgeOK reports whether a nudge may be given now: the main agent,
// plan in the tool set, none yet in this turn, and no open node.
func (h *Harness) planNudgeOK() bool {
	if h.isChild() || h.nudge.done || !h.hasTool(taskgraph.ToolName) {
		return false
	}
	g, err := planStore{h}.View(context.Background())
	return err == nil && g.Open() == 0
}

// hasTool reports whether name is in the tool set as of the last rebuild.
func (h *Harness) hasTool(name string) bool {
	if p := h.toolNames.Load(); p != nil {
		for _, n := range *p {
			if n == name {
				return true
			}
		}
	}
	return false
}

// planNudgeRound is called once per round (noteUsage), after the turn limit
// counted it.
func (h *Harness) planNudgeRound() {
	limit := h.planNudgeRounds()
	if limit == 0 {
		return
	}
	n := int(h.turnRounds.n.Load())
	if n < limit {
		return
	}
	h.nudge.mu.Lock()
	defer h.nudge.mu.Unlock()
	if h.planNudgeOK() {
		h.planNudgeNow(fmt.Sprintf(planNudgeRoundsText, n))
	}
}

// planNudgeResult counts one tool result toward a stall.
func (h *Harness) planNudgeResult(failed bool) {
	limit := h.planNudgeFails()
	h.nudge.mu.Lock()
	defer h.nudge.mu.Unlock()
	if !failed {
		h.nudge.fails = 0
		return
	}
	h.nudge.fails++
	if limit > 0 && h.nudge.fails >= limit && h.planNudgeOK() {
		h.planNudgeNow(fmt.Sprintf(planNudgeFailsText, h.nudge.fails))
	}
}

// planNudgeNow queues the aside and arms the forced call. Caller holds
// nudge.mu.
func (h *Harness) planNudgeNow(why string) {
	h.nudge.done, h.nudge.force = true, true
	h.AsideWith(why+planNudgeAsk, AsideOpts{Key: "plan-nudge", TurnScoped: true})
}

// planForceOpts is the turn runner's hook (reasoningTee): the options for
// this request with a plan call forced, or nil to send it as it is. It
// disarms the force: one request only.
func (h *Harness) planForceOpts(tools []llm.ToolDef, opts *llm.ChatOpts) *llm.ChatOpts {
	h.nudge.mu.Lock()
	force := h.nudge.force
	h.nudge.force = false
	h.nudge.mu.Unlock()
	if !force {
		return nil
	}
	// Known not to honor it: the aside is all there is.
	if c, ok := untee(h.Session.Runner).(interface{ Capabilities() llm.Capabilities }); ok {
		if honored, known := c.Capabilities().ToolChoiceHonored(); known && !honored {
			return nil
		}
	}
	o := llm.ChatOpts{}
	if opts != nil {
		o = *opts
	}
	if defOf(tools, taskgraph.ToolName) != nil {
		choice, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": taskgraph.ToolName}})
		o.ToolChoice = string(choice)
		return &o
	}
	// Heredoc transport: no tools in the request, a grammar over all of them.
	// A grammar over plan alone forces it (fork.go).
	if len(tools) == 0 && o.Grammar != "" {
		if d := defOf(h.Session.Tools, taskgraph.ToolName); d != nil {
			o.Grammar = llm.HeredocGrammar([]llm.ToolDef{*d})
			return &o
		}
	}
	return nil
}

func defOf(tools []llm.ToolDef, name string) *llm.ToolDef {
	for i := range tools {
		if tools[i].Function.Name == name {
			return &tools[i]
		}
	}
	return nil
}

// planForcedStream sends a request with the plan call forced; a refusal is
// logged and the request sent again as it was, so it never fails the turn.
func planForcedStream(ctx context.Context, r agent.LLMRunner, msgs []llm.Message, tools []llm.ToolDef, opts, forced *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	ch, err := r.ChatStream(ctx, msgs, tools, forced)
	if err == nil || ctx.Err() != nil {
		return ch, err
	}
	log.Printf("dun: the forced plan call was refused (%v); sent again without it", err)
	return r.ChatStream(ctx, msgs, tools, opts)
}

// execOutcomeKey carries an *execOutcome down the dispatch chain, so exec can
// say it failed without anyone parsing its text (ExecResult says why not).
type execOutcomeKey struct{}

type execOutcome struct{ failed bool }

// markExecFailed records, for withPlanNudge, that exec failed or timed out.
func markExecFailed(ctx context.Context) {
	if o, ok := ctx.Value(execOutcomeKey{}).(*execOutcome); ok {
		o.failed = true
	}
}

// withPlanNudge counts each tool result toward a stall. Inside
// withLiftedQueue, so an aside it queues rides this very result.
func withPlanNudge(inner agent.ToolDispatcher, h *Harness) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		o := &execOutcome{}
		out, err := inner(context.WithValue(ctx, execOutcomeKey{}, o), tc)
		h.planNudgeResult(err != nil || o.failed || strings.HasPrefix(out, "ERROR"))
		return out, err
	}
}
