package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

// usageEvent is the per-turn accounting event, built in ONE place because it is
// emitted from two turn paths that had already drifted apart once. It carries
// three groups: what the turn cost, how the pre-conversation context breaks
// down, and how the window is divided (see harness.WindowBudget) — the last of which
// /context has no other way to learn.
//
// The token counts are the session's, cumulative (agentkit's tally): a
// client sets them. A turn cut before its first round returns no tally, so
// the last one stands for it, or the totals would read 0. They include what
// the engines before this one counted on the session (tally.go), and the
// stats file is saved with the turn closed.
func usageEvent(h *harness.Harness, u agent.TokenUsage) *proto.Usage {
	u = lastUsage.at(u)
	defer tally.save(u)
	sysTokens, forcedCalls, notifLifted := h.SessionStats()
	return &proto.Usage{
		Total: u.Total, Active: u.Active, Cached: u.Cached, Processed: u.Processed,
		Generated: u.Generated, Turns: u.Turns,
		ForcedCalls: forcedCalls, NotificationsLifted: notifLifted,
		SystemCost: systemCost(h.SystemParts(), sysTokens),
		Window:     windowOf(h.Window()),
		// The non-turn LLM calls (suggest, rephrase, commit, rescue) — see
		// dun.sidecall.go. Per-kind totals, so /context can say where the money
		// went outside the conversation itself.
		SideCalls: h.SideCalls(),
		// Proactive RAG's cost — what it appended and what it has been re-sent
		// as. See dun.surfacing.go.
		Surfacing: h.Surfacing(),
		// What the prompt cache did, by the kind of build before each round,
		// and how often the prefix was rewritten. See dun.cachestats.go.
		Cache: h.CacheStats(),
		// Money and limits. The turn is closed here, in the one place both
		// turn paths pass through, so "last turn" means the same on both.
		Spend: endTurnSpend(h),
		// What the turn cost in rounds, time, tokens and money, and the
		// endpoint's prefill speed (dun/roundstats.go).
		TurnStats: turnStatsProto(h),
	}
}

// usageTally is the last cumulative tally a usage event carried, and the
// seed: what the engines before this one counted on the session (tally.go).
type usageTally struct {
	mu   sync.Mutex
	base agent.TokenUsage
	last agent.TokenUsage
}

var lastUsage usageTally

// seed sets the earlier engines' totals, before the first round.
func (t *usageTally) seed(b agent.TokenUsage) {
	t.mu.Lock()
	t.base = b
	t.mu.Unlock()
}

// total is u (this process's tally) with the seed added.
func (t *usageTally) total(u agent.TokenUsage) agent.TokenUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return addUsage(t.base, u)
}

// at is u, or the last tally when u is behind it (a turn with no round
// returns a zero one), with the seed added. Active is u's when it has one:
// the window now.
func (t *usageTally) at(u agent.TokenUsage) agent.TokenUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	if u.Turns >= t.last.Turns {
		t.last = u
		return addUsage(t.base, u)
	}
	out := t.last
	if u.Active > 0 {
		out.Active = u.Active
	}
	return addUsage(t.base, out)
}

// mcpInfoDays is how far back /mcp info totals what the schemas cost.
const mcpInfoDays = 7

func turnStatsProto(h *harness.Harness) *proto.TurnStats {
	t := h.EndTurnStats()
	speed, n := h.PrefillSpeed()
	return &proto.TurnStats{Rounds: t.Rounds, WallMS: t.WallMS, ModelMS: t.ModelMS,
		Prompt: t.Prompt, Cached: t.Cached, CacheWrite: t.CacheWrite, Completion: t.Completion,
		Timed: t.Timed, PromptN: t.PromptN, PromptMS: t.PromptMS, GenN: t.GenN, GenMS: t.GenMS,
		Cost: t.Cost, Currency: t.Currency, Costed: t.Costed, Mixed: t.Mixed, SubAgents: t.SubAgents,
		PrefillTPS: speed, PrefillRounds: n}
}

// monthUsage is the stats ledger's main rounds since the start of now's
// month; nil when there were none.
func monthUsage(h *harness.Harness, now time.Time) *proto.UsageMonth {
	since := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	s := h.StatsSummary(since)
	if s.Rounds == 0 {
		return nil
	}
	return &proto.UsageMonth{Since: since.UTC().Format(time.RFC3339), Rounds: s.Rounds,
		Prompt: s.Prompt, Cached: s.Cached, PrefillTPS: s.Speed}
}

// monthModels is the stats ledger's rounds since the start of now's month,
// by model.
func monthModels(h *harness.Harness, now time.Time) []proto.ModelTokens {
	since := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	var out []proto.ModelTokens
	for _, u := range h.ModelUsage(since) {
		out = append(out, proto.ModelTokens{Model: u.Model, Requests: u.Requests, Prompt: u.Prompt,
			Cached: u.Cached, CacheWrite: u.CacheWrite, Completion: u.Completion})
	}
	return out
}

func endTurnSpend(h *harness.Harness) *proto.Spend {
	h.EndTurnSpend()
	sp := spendProto(h.Spend(), time.Now())
	sp.Models = monthModels(h, time.Now())
	return sp
}

// windowOf is the window division for an event. Separate so context_cost can
// carry it at STARTUP too: the window and the reserve are known before the first
// turn, and a /context opened before anything has run should say so rather than
// look like a session with no window.
func windowOf(w harness.WindowBudget) proto.Window {
	return proto.Window{
		Window: w.Window, WindowReserved: w.Reserved, WindowCap: w.Cap,
		PromptBudget: w.PromptBudget, PromptTokens: w.Prompt, PromptOverhead: w.Overhead,
		CharsPerToken: w.CharsPerToken, RatioMeasured: w.Measured, RatioRounds: w.Rounds,
		WindowCuts: w.Cuts, WindowFolds: w.Folds,
	}
}

// retryEvent renders a RetryNote as an --engine event. Durations travel as milliseconds
// (JSON has no duration type, and a client that wants to count down needs the
// number, not a formatted string) alongside the ready-made one-liner in "text".
func retryEvent(n harness.RetryNote) *proto.Retry {
	ev := &proto.Retry{Scope: n.Scope, Kind: n.Kind, Attempt: n.Attempt, Status: n.Status,
		DelayMS: n.Delay.Milliseconds(), ElapsedMS: n.Elapsed.Milliseconds(),
		BudgetMS: n.Budget.Milliseconds(),
		Reason:   n.Reason, Detail: n.Detail, Text: n.String(), ServerAsked: n.ServerAsked,
		Queue: n.Queue}
	// Queue numbers only when the provider actually reported them: a plain 429
	// carries none, and emitting zeros claims a queue of no slots.
	if n.Queued() {
		ev.Capacity, ev.InFlight, ev.Waiting = n.Capacity, n.InFlight, n.Waiting
	}
	return ev
}

// retryGlyph marks the three states a retry note can be in, for the human stream.
func retryGlyph(n harness.RetryNote) string {
	switch n.Kind {
	case "recovered":
		return "✓"
	case "giveup":
		return "✗"
	}
	return "⏳"
}

// emitter writes one JSON event per line to stdout, serialized (tokens stream
// from the same goroutine as turns today, but the mutex keeps it safe if that
// changes).
type emitter struct {
	mu      sync.Mutex
	w       io.Writer // nil → stdout; a test points this at a buffer
	enc     *json.Encoder
	session string
	// onReply sees every event after it is written: what keeps the person's
	// commands in the session (recordReplies).
	onReply func(payload any)
	// turn follows the events into turn_state (turnstate.go); nil outside --engine.
	turn *turnTracker
	// rounds stamps reasoning, token and tool_call with their model round
	// and sends reasoning_end where a round's thinking ends (proto.StepRound),
	// in the order the events are written.
	rounds proto.RoundState
}

func (e *emitter) emit(payload any) {
	e.mu.Lock()
	now := time.Now()
	if e.enc == nil {
		w := e.w
		if w == nil {
			w = os.Stdout
		}
		e.enc = json.NewEncoder(w)
	}
	if end := proto.StepRound(&e.rounds, payload); end != nil {
		ee := proto.NewEvent(end)
		ee.Session, ee.TS = e.session, now
		_ = e.enc.Encode(ee)
	}
	ev := proto.NewEvent(payload)
	ev.Session, ev.TS = e.session, now
	_ = e.enc.Encode(ev)
	onReply, turn := e.onReply, e.turn
	e.mu.Unlock()
	// After the lock: keeping a command appends to the session, which may
	// emit an event of its own.
	if onReply != nil {
		onReply(payload)
	}
	if turn != nil {
		turn.observe(payload)
	}
}

// recordReplies keeps the person's commands, and what dun answered, in the
// session (dun/usercommand.go): the replies to what they typed or toggled,
// spelled as typed. Views, pickers and the engine's own server starts are
// not commands.
func recordReplies(h *harness.Harness) func(payload any) {
	slashOf := map[string]string{"spend": "usage spend", "files": "file", "ctxconfig": "context config", mcpserver.ServerDocs: "rag"}
	kindSlash := map[string]string{"indexes": "rag", "paths": "file", "lsp": "lsp", "skills": "skill", "rules": "rules"}
	commands := map[string]bool{"slash": true, "files": true, "switches": true, "mcp": true, "ext": true, "jobs": true,
		"schedule": true, "loop": true, "remember": true, "rules": true, "effort": true, "retry": true, "loops": true, "spend": true, "suggest": true,
		"worklog": true, "journal": true, "plan": true, "memories": true, "interrupt": true, "ctxconfig": true, "repaint": true}
	return func(payload any) {
		switch p := payload.(type) {
		case *proto.ControlEvent:
			if !commands[p.ID] || strings.TrimSpace(p.Message) == "" || (p.ID == "ext" && p.Action == "view") {
				return
			}
			line := "/" + p.Action
			switch {
			case p.ID == "switches":
				kind, rest, _ := strings.Cut(p.Action, " ")
				if s := kindSlash[kind]; s != "" {
					kind = s
				}
				line = "/" + kind + " " + rest
			case p.ID != "slash":
				id := p.ID
				if s := slashOf[id]; s != "" {
					id = s
				}
				line = strings.TrimSpace("/" + id + " " + p.Action)
			}
			h.RecordCommand(line, p.Message)
		case *proto.ServerEvent:
			switch p.Action {
			case "", "start", "index", "setup", "activity":
				return
			}
			if p.Background || strings.TrimSpace(p.Message) == "" {
				return
			}
			id := p.ID
			if s := slashOf[id]; s != "" {
				id = s
			}
			h.RecordCommand("/"+id+" "+p.Action, p.Message)
		}
	}
}

// setSession stamps every later event with the session id. Events before it
// (a fatal start error) carry none: there is no session yet.
func (e *emitter) setSession(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.session = id
}
func shortArgs(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, textfmt.Clip(fmt.Sprint(args[k]), 40)))
	}
	return strings.Join(parts, ", ")
}

// agentsProto is every sub-agent, for the agents event.
func agentsProto(as []harness.AgentInfo) []proto.AgentInfo {
	out := make([]proto.AgentInfo, 0, len(as))
	for _, a := range as {
		out = append(out, proto.AgentInfo{ID: a.ID, Model: a.Model, Tier: a.Tier, Effort: a.Effort, State: a.State, Status: a.Status,
			Prompt: a.Prompt, Tokens: a.Tokens, Seconds: a.Seconds, Dir: a.Dir, Blocked: a.Blocked})
	}
	return out
}

// jobsProto mirrors agentsProto — the whole list every time, for the same
// reason. Times cross as unix seconds so the UI can tick a running job's clock
// without the engine pushing a row per second.
func jobsProto(js []harness.JobInfo) []proto.JobInfo {
	out := make([]proto.JobInfo, 0, len(js))
	for _, j := range js {
		out = append(out, proto.JobInfo{ID: j.ID, Command: j.Command, Log: j.Log, State: j.State,
			Bytes: j.Bytes, Started: j.Started, Ended: j.Ended, Code: j.Code, Muted: j.Muted, PGID: j.PGID})
	}
	return out
}
func docsProto(docs []harness.DocHitInfo) []proto.DocHit {
	out := make([]proto.DocHit, 0, len(docs))
	for _, d := range docs {
		out = append(out, proto.DocHit{Title: d.Title, ID: d.DocID, Line: d.Line, Score: d.Score, Inline: d.Inline,
			Kind: d.Kind, Path: d.Path, Index: d.Index, Root: d.Root, Candidates: d.Candidates, RunnerUp: d.RunnerUp,
			Similarity: d.Similarity, Matched: d.Matched, QueryTerms: d.QueryTerms, RunnerUpSim: d.RunnerUpSim,
			Source: d.Source(), Why: d.Why()})
	}
	return out
}

// historyProto is a conversation as replayable items.
func historyProto(items []harness.HistoryItem) []proto.HistoryItem {
	out := make([]proto.HistoryItem, 0, len(items))
	for _, it := range items {
		out = append(out, proto.HistoryItem{Kind: it.Kind, ID: it.ID, Queued: it.Queued, IsError: it.IsError, Content: it.Content, Tool: it.Tool,
			CallID: it.CallID, Args: it.Args, Agent: it.Agent})
	}
	return out
}

// systemCost renders the context breakdown for the event stream. The TUI
// is fed by events rather than holding the harness, so the parts travel as
// plain maps beside the totals they add up to.
func systemCost(bd harness.SystemBreakdown, total int) proto.SystemCost {
	parts := make([]proto.ContextPart, 0, len(bd.Parts))
	for _, p := range bd.Parts {
		parts = append(parts, proto.ContextPart{Name: p.Name, Tokens: p.Tokens})
	}
	files := make([]proto.ContextPart, 0, len(bd.Files))
	for _, p := range bd.Files {
		files = append(files, proto.ContextPart{Name: p.Name, Tokens: p.Tokens})
	}
	return proto.SystemCost{SystemTokens: total, SystemExact: bd.Exact, SystemPrompt: bd.Prompt,
		SystemShared: bd.Shared, SystemParts: parts, SystemRules: bd.Rules, SystemRulesMode: bd.RulesMode,
		SystemFiles: files}
}
