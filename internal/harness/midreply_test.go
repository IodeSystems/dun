package harness

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("DUN_PLAN_NUDGE_ROUNDS", "")
	t.Setenv("DUN_PLAN_NUDGE_FAILS", "")
	t.Setenv("DUN_PLAN_STALL_ROUNDS", "")
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []mcpserver.Server{}, Client: r, Exec: &HostShell{Dir: dir},
		SessionFile:     filepath.Join(dir, "s.jsonl"),
		PlanNudgeRounds: rounds, PlanNudgeFails: -1, PlanStallRounds: -1, NoAsks: true,
	})
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

// One message: the header, then the words verbatim in «». Several: one
// header, then each on its own line, in order.
func TestMidTurnFrame(t *testing.T) {
	if got, want := midTurnFrame([]string{"what are you doing?"}),
		"[the person wrote mid-turn; reply to them in text before your next tool call, then go on if it still makes sense]: «what are you doing?»"; got != want {
		t.Errorf("one:\n%s\nwant:\n%s", got, want)
	}
	if got, want := midTurnFrame([]string{"one", "two"}), midTurnHead+"\n«one»\n«two»"; got != want {
		t.Errorf("several:\n%s\nwant:\n%s", got, want)
	}
	// The cost: 114 bytes of header, then 5 bytes (" «»" in UTF-8) a message.
	if n := len(midTurnHead); n != 114 {
		t.Errorf("header is %d bytes, want 114", n)
	}
}

// The person's words are last, after the board and any other news.
func TestMidTurnLiftIsLast(t *testing.T) {
	h := newNoteHarness(t)
	h.Say("stop")
	h.Aside("[plan] the board")
	out := h.liftQueued("ok")
	if want := "ok\n\n[session] [plan] the board\n\n" + midTurnHead + " «stop»"; out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

// The system prompt says who the framed text is, once, where a person reads
// along; about 90 bytes.
func TestSystemMidTurn(t *testing.T) {
	h := startStall(t, &nudgeRunner{}, -1, -1, -1)
	if c := strings.Count(h.Session.System, systemMidTurn); c != 1 {
		t.Errorf("the line is in the system prompt %d times, want 1", c)
	}
	if !strings.Contains(systemMidTurn, `"`+midTurnHead[:len("[the person wrote mid-turn")]+`"`) {
		t.Error("the line does not name the header")
	}
	if n := len(systemMidTurn); n > 100 {
		t.Errorf("the line is %d bytes, want about 90", n)
	}
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
	if r.count(1, "«what are you doing?»") != 1 {
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
