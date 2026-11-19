package harness

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// midRunner is nudgeRunner for a round that may write text AND call a
// tool: step gives round i's text and call (nil: text only).
type midRunner struct {
	mu     sync.Mutex
	choice []string
	sent   [][]llm.Message
	step   func(i int) (string, *llm.ToolCall)
}

func (r *midRunner) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	choice := ""
	if opts != nil {
		choice = opts.ToolChoice
	}
	r.mu.Lock()
	i := len(r.choice)
	r.choice = append(r.choice, choice)
	r.sent = append(r.sent, msgs)
	r.mu.Unlock()
	text, tc := r.step(i)
	if strings.Contains(choice, `"plan_`) && tc != nil {
		tc = nudgeCall(fmt.Sprintf("p%d", i), "plan_add", `{"title":"finish","done_when":"it works"}`)
	}
	out := make(chan llm.StreamChunk, 3)
	if text != "" {
		out <- llm.StreamChunk{Content: text}
	}
	if tc != nil {
		out <- llm.StreamChunk{ToolCall: tc}
	}
	out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
	close(out)
	return out, nil
}

func (r *midRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.choice)
}

// count is how many messages of request i carry text.
func (r *midRunner) count(i int, text string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.sent[i] {
		if strings.Contains(m.Content, text) {
			n++
		}
	}
	return n
}

func (r *midRunner) forcedAt() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var at []int
	for i, c := range r.choice {
		if strings.Contains(c, `"plan_`) {
			at = append(at, i)
		}
	}
	return at
}

func startMid(t *testing.T, r *midRunner, rounds int) *Harness {
	t.Helper()
	return startMidWith(t, r, func(c *Config) { c.PlanNudgeRounds = rounds })
}

// startMidWith is startMid with set applied to the config.
func startMidWith(t *testing.T, r *midRunner, set func(*Config)) *Harness {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("DUN_PLAN_NUDGE_ROUNDS", "")
	t.Setenv("DUN_PLAN_NUDGE_FAILS", "")
	t.Setenv("DUN_PLAN_STALL_ROUNDS", "")
	dir := t.TempDir()
	cfg := Config{
		Workspace: dir, Servers: []mcpserver.Server{}, Client: r, Exec: &HostShell{Dir: dir},
		SessionFile:     filepath.Join(dir, "s.jsonl"),
		PlanNudgeRounds: -1, PlanNudgeFails: -1, PlanStallRounds: -1, NoAsks: true,
	}
	set(&cfg)
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func midExec(i int) *llm.ToolCall {
	// A command of its own each round: the loop guard quotes a repeat's result.
	return nudgeCall(fmt.Sprintf("c%d", i), "exec", fmt.Sprintf(`{"command":"true %d"}`, i))
}

// liftedTexts is the person's messages held to follow the result being
// dispatched (deliverLifted).
func liftedTexts(h *Harness) []string {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	var out []string
	for _, q := range h.lifted {
		out = append(out, q.text)
	}
	return out
}

const midAsk = "What is your goal here? what are you doing?"

// midSayOnce runs exec in round 0, the person typing while it runs, and
// replies with text after.
func midSayOnce(h **Harness) *midRunner {
	return &midRunner{step: func(i int) (string, *llm.ToolCall) {
		if i == 0 {
			(*h).Say(midAsk)
			return "", midExec(i)
		}
		return "Fixing the build: the test fails on a missing file.", nil
	}}
}

func roles(msgs []llm.Message) string {
	var rs []string
	for _, m := range msgs {
		rs = append(rs, m.Role)
	}
	return strings.Join(rs, ",")
}

// A message typed while a tool runs reaches the next request as a user
// message of its own, verbatim, right after that round's tool result. The
// result does not carry it, and the system prompt needs no line about it.
func TestMidTurnUserAfterResults(t *testing.T) {
	var h *Harness
	r := midSayOnce(&h)
	h = startMid(t, r, -1)
	askOK(t, h)
	msgs := r.sent[1]
	n := len(msgs)
	if got := roles(msgs[n-3:]); got != "assistant,tool,user" || len(msgs[n-3].ToolCalls) != 1 {
		t.Fatalf("request 1 ends %s, want assistant(tool_calls),tool,user", got)
	}
	if msgs[n-1].Content != midAsk {
		t.Errorf("the user message is %q, want the words verbatim", msgs[n-1].Content)
	}
	if strings.Contains(msgs[n-2].Content, midAsk) {
		t.Errorf("the tool result carries the message too: %q", msgs[n-2].Content)
	}
	if strings.Contains(h.Session.System, "mid-turn") {
		t.Error("the system prompt explains the message")
	}
}

// fold_user_into_tool_result: the message goes inside the last tool result,
// framed by agentkit, and no user message follows it.
func TestMidTurnFoldIntoToolResult(t *testing.T) {
	var h *Harness
	r := midSayOnce(&h)
	h = startMidWith(t, r, func(c *Config) { c.FoldUserIntoToolResult = true })
	askOK(t, h)
	msgs := r.sent[1]
	last := msgs[len(msgs)-1]
	if last.Role != "tool" || !strings.HasSuffix(last.Content, "\n\n[the user wrote, while this ran] "+midAsk) {
		t.Fatalf("request 1 ends %s: %q", roles(msgs), last.Content)
	}
}

// Delivered once: one user entry with the message's id, once in the next
// request and in every later one, once on the intake, nothing left pending
// to start a turn, and no second copy (a delivered entry, the queue).
func TestMidTurnNoDoubleDelivery(t *testing.T) {
	var h *Harness
	r := midSayOnce(&h)
	var got []Delivery
	h = startMidWith(t, r, func(c *Config) {
		c.NoAsks = false
		c.OnDelivered = func(d Delivery) { got = append(got, d) }
	})
	askOK(t, h)
	if r.calls() != 2 {
		t.Errorf("%d requests, want 2: the message started no turn of its own", r.calls())
	}
	for i := 1; i < r.calls(); i++ {
		if c := r.count(i, midAsk); c != 1 {
			t.Errorf("request %d carries the message %d times, want 1", i, c)
		}
	}
	n := 0
	for _, e := range h.store.History() {
		if strings.Contains(e.Content, midAsk) {
			n++
			if e.Kind != agent.KindUser || e.Tag != midTurnTag {
				t.Errorf("entry %+v, want a mid-turn user entry", e)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d entries hold the message, want 1", n)
	}
	if len(got) != 1 || len(got[0].IDs) != 1 || got[0].CallID != "c0" {
		t.Errorf("OnDelivered %+v, want once, for call c0", got)
	}
	// Not Pending(): the exec the message released became a job, and its
	// finishing is news of its own.
	if q := h.QueuedUser(); len(q) != 0 {
		t.Errorf("still queued after the turn: %+v", q)
	}
	h.asks.mu.Lock()
	intake := 0
	for _, m := range h.asksLocked().Intake {
		if m.Text == midAsk {
			intake++
		}
	}
	h.asks.mu.Unlock()
	if intake != 1 {
		t.Errorf("the intake holds the message %d times, want 1", intake)
	}
	if items := h.History(); !historyQueued(items, midAsk) {
		t.Error("History does not show the message as typed mid-turn")
	}
}

func historyQueued(items []HistoryItem, text string) bool {
	for _, it := range items {
		if it.Kind == "user" && it.Content == text {
			return it.Queued
		}
	}
	return false
}

// The round after the lift made only tool calls: one aside, in that round's
// results, and no second one however long the model goes on.
func TestMidTurnMissedReplyOnce(t *testing.T) {
	var h *Harness
	r := &midRunner{step: func(i int) (string, *llm.ToolCall) {
		switch {
		case i == 0:
			h.Say("what are you doing?")
			return "", midExec(i)
		case i < 5:
			return "", midExec(i)
		}
		return "done", nil
	}}
	h = startMid(t, r, -1)
	askOK(t, h)
	if r.count(1, "what are you doing?") != 1 {
		t.Fatal("request 1 does not carry the message")
	}
	if r.count(1, midTurnNudge) != 0 {
		t.Error("the aside came before the round it is about")
	}
	if r.count(2, "[dun] "+midTurnNudge) != 1 {
		t.Error("request 2 does not carry the aside")
	}
	if n := r.count(r.calls()-1, midTurnNudge); n != 1 {
		t.Errorf("the aside is in the conversation %d times, want 1", n)
	}
}

// The model replied with text in the round after the lift: no aside.
func TestMidTurnRepliedNoAside(t *testing.T) {
	var h *Harness
	r := &midRunner{step: func(i int) (string, *llm.ToolCall) {
		switch i {
		case 0:
			h.Say("what are you doing?")
			return "", midExec(i)
		case 1:
			return "Fixing the build; going on.", midExec(i)
		case 2:
			return "", midExec(i)
		}
		return "done", nil
	}}
	h = startMid(t, r, -1)
	askOK(t, h)
	if n := r.count(r.calls()-1, midTurnNudge); n != 0 {
		t.Errorf("the aside was given after a reply (%d times)", n)
	}
}

// The plan force due on the reply's round waits one request, armed.
func TestMidTurnPlanForceWaits(t *testing.T) {
	var h *Harness
	r := &midRunner{step: func(i int) (string, *llm.ToolCall) {
		switch {
		case i == 1:
			h.Say("what are you doing?")
			return "", midExec(i)
		case i == 2:
			return "Fixing the build.", midExec(i)
		case i < 6:
			return "", midExec(i)
		}
		return "done", nil
	}}
	h = startMid(t, r, 2) // without the message, request 2 is forced
	askOK(t, h)
	if at := r.forcedAt(); len(at) != 1 || at[0] != 3 {
		t.Errorf("forced at %v, want [3]: not on the reply's round", at)
	}
}

// A text-only reply right after the lift ends the turn: no "Open plan
// nodes" check sends it back to its tools. Without the message, the check
// runs.
func TestMidTurnIdleAcceptsReply(t *testing.T) {
	for _, say := range []bool{false, true} {
		var h *Harness
		r := &midRunner{step: func(i int) (string, *llm.ToolCall) {
			switch i {
			case 0:
				return "", nudgeCall("p", "plan_add", `{"title":"the work","done_when":"it works"}`)
			case 1:
				if say {
					h.Say("what are you doing?")
				}
				return "", midExec(i)
			}
			return "I am working on the plan's node.", nil
		}}
		h = startMid(t, r, -1)
		askOK(t, h)
		checked := r.calls() > 3 && r.count(3, "Open plan nodes") > 0
		if checked == say {
			t.Errorf("message %v: turn-end check given = %v (%d requests)", say, checked, r.calls())
		}
	}
}
