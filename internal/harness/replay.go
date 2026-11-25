package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/fsutil"
)

// replay.go — in-flight tool calls are optionally replayable (Carl
// 2026-11-24, docs/decisions.md "Questions survive a restart").
//
// A tool says whether a call of it that was in flight when the engine
// stopped may run again: replayable is a property of the tool
// (Harness.replayable), not of ask_user. A replayable call that was
// waiting on the person's answer when the engine stopped (crash, kill,
// dund's restart, a model switch, /exit) has not taken effect: its
// question is in <session>.pending.json, and the next engine dispatches
// the call again from its arguments (Replay). The question is asked again
// with the same ask id and call id, so every client shows it again; the
// answer is that call's result, and the turn goes on with no new user
// message (a continuation turn, as a background job's news starts one).
//
// Replayable now: ask_user, and the file tools while they wait on the
// file-access question (fileaccess.go), which runs before the call does
// anything. Every other tool is not: its cut call gets the failed result
// restartedCall says, and the model is told to try again.
//
// The record is written when the question opens and removed when it is
// answered or cancelled. A stop of the engine (the `stop` command: dund's
// restart, a model switch, /exit) while the question is open SUSPENDS it
// (ErrSuspended): the record stays, the call gets no result, the turn ends
// and the engine exits. A kill leaves the same state with no help.

// ErrSuspended is a question the engine's stop cut: its call is replayed
// by the next engine (Replay). Config.Ask returns it (wrapped or not).
var ErrSuspended = errors.New("the engine is stopping: the question is asked again when it starts")

// PendingAsk is a question open in a replayable call, kept in
// <session>.pending.json until it is answered or cancelled.
type PendingAsk struct {
	AskID  string `json:"ask_id"`
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	// Args are the call's arguments, as dispatched: the replay runs them.
	Args        string    `json:"args"`
	Question    string    `json:"question"`
	Options     []string  `json:"options,omitempty"`
	Multi       bool      `json:"multi,omitempty"`
	NoCustom    bool      `json:"no_custom,omitempty"`
	Recommended []string  `json:"recommended,omitempty"`
	Origin      int       `json:"origin,omitempty"` // turnOrigin of the turn that asked
	At          time.Time `json:"at"`
}

// PendingFile is the session's pending-question sidecar.
func PendingFile(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".pending.json"
}

type pendingState struct {
	mu sync.Mutex
	// replaying: a Replay is under way (untilTurnBegin keeps the run paused).
	replaying atomic.Bool
}

// callKey carries the tool call being dispatched (withCallInfo).
type callKey struct{}

// askIDKey carries the ask id a question is to use (pendingWrapAsk).
type askIDKey struct{}

// CallFrom is the tool call ctx is dispatching, or nil.
func CallFrom(ctx context.Context) *llm.ToolCall {
	tc, _ := ctx.Value(callKey{}).(*llm.ToolCall)
	return tc
}

// AskIDFrom is the ask id a question asked under ctx is to carry: a
// replayed question keeps the id it had. "" for a new one.
func AskIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(askIDKey{}).(string)
	return s
}

// withCallInfo is the outermost wrapper: the call rides ctx (CallFrom),
// and a call whose question the engine's stop suspended ends the turn with
// no result (agent.ErrSessionClosed: no further round; the store drops the
// result agentkit writes for it).
func withCallInfo(inner agent.ToolDispatcher, h *Harness) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		c := tc
		out, err := inner(context.WithValue(ctx, callKey{}, &c), tc)
		if h.store != nil && h.store.isSuspended(tc.ID) {
			return "", errors.Join(agent.ErrSessionClosed, ErrSuspended)
		}
		return out, err
	}
}

// replayable reports whether a call of tool name that was waiting on the
// person's answer may be dispatched again after a restart.
func (h *Harness) replayable(name string) bool {
	if name == "ask_user" {
		return true
	}
	ft := h.fileTools()
	return ft != nil && ft.Handles(name)
}

// pendingWrapAsk keeps each question a replayable call asks in the
// sidecar while it is open. Called once, in New, for the root session.
func (h *Harness) pendingWrapAsk() {
	ask := h.cfg.Ask
	if ask == nil || h.isChild() || h.cfg.SessionFile == "" {
		return
	}
	h.cfg.Ask = func(ctx context.Context, q string, opts []string, multi, noCustom bool, rec []string) (string, error) {
		tc := CallFrom(ctx)
		if tc == nil || !h.replayable(tc.Function.Name) {
			return ask(ctx, q, opts, multi, noCustom, rec)
		}
		o, _ := h.turnOriginNow()
		p := PendingAsk{CallID: tc.ID, Tool: tc.Function.Name, Args: tc.Function.Arguments, Question: q,
			Options: opts, Multi: multi, NoCustom: noCustom, Recommended: rec, Origin: int(o), At: time.Now()}
		// The same call asking the same question again (a replay): the same id.
		if old := h.pendingRead(); old != nil && old.CallID == tc.ID && old.Question == q {
			p.AskID, p.At = old.AskID, old.At
		}
		if p.AskID == "" {
			p.AskID = NewMessageID()
		}
		h.pendingWrite(&p)
		ans, err := ask(context.WithValue(ctx, askIDKey{}, p.AskID), q, opts, multi, noCustom, rec)
		if err != nil && errors.Is(err, ErrSuspended) {
			h.store.suspend(tc.ID)
			return "", err
		}
		h.pendingWrite(nil)
		return ans, err
	}
}

// pendingRead is the sidecar's question, or nil.
func (h *Harness) pendingRead() *PendingAsk {
	h.pending.mu.Lock()
	defer h.pending.mu.Unlock()
	return readPending(h.cfg.SessionFile)
}

func readPending(sessionFile string) *PendingAsk {
	if sessionFile == "" {
		return nil
	}
	b, err := os.ReadFile(PendingFile(sessionFile))
	if err != nil {
		return nil
	}
	var p PendingAsk
	if json.Unmarshal(b, &p) != nil || p.CallID == "" {
		return nil
	}
	return &p
}

// pendingWrite saves p, or removes the sidecar for nil.
func (h *Harness) pendingWrite(p *PendingAsk) {
	h.pending.mu.Lock()
	defer h.pending.mu.Unlock()
	f := PendingFile(h.cfg.SessionFile)
	if p == nil {
		_ = os.Remove(f)
		return
	}
	b, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		return
	}
	_ = fsutil.WriteAtomic(f, append(b, '\n'), 0o600)
}

// unanswered reports whether entries hold call id's tool_call with no result.
func unanswered(entries []agent.Entry, id string) bool {
	called := false
	for _, e := range entries {
		if e.ToolCallID != id {
			continue
		}
		switch e.Kind {
		case agent.KindToolCall:
			called = true
		case agent.KindToolResult:
			return false
		}
	}
	return called
}

// PendingReplay is the question a replay would ask again: the sidecar's,
// when its call is in the conversation with no result. A record that is
// not (answered, or a /clear since) is removed.
func (h *Harness) PendingReplay() *PendingAsk {
	if h.isChild() || h.cfg.SessionFile == "" || h.cfg.Ask == nil {
		return nil
	}
	p := h.pendingRead()
	if p == nil || h.store.isSuspended(p.CallID) { // suspended: for the next engine
		return nil
	}
	entries, err := h.store.Context(context.Background(), "dun")
	if err != nil {
		return nil
	}
	if !unanswered(entries, p.CallID) {
		h.pendingWrite(nil)
		return nil
	}
	return p
}

// QuestionPending reports whether the session in sessionFile stopped with a
// question a replayable call asked still open: its next engine asks it
// again (dund: start that engine, and leave the question to it).
func QuestionPending(sessionFile string) bool {
	p := readPending(sessionFile)
	if p == nil {
		return false
	}
	st, err := openSessionStore(sessionFile)
	if err != nil {
		return false
	}
	st.mu.Lock()
	entries := append([]agent.Entry(nil), st.entries...)
	st.mu.Unlock()
	return unanswered(entries, p.CallID)
}

// questionCancelled is a replayed question's result when the person
// interrupts it instead of answering.
func questionCancelled(name string) string {
	return fmt.Sprintf("ERROR: this %s call's question was cancelled by the user (they interrupted it "+
		"instead of answering). Do not ask it again unless their next message wants it.", name)
}

// Replay runs the turn a pending question continues: its call dispatched
// again (the question asked again), its result recorded, and the model's
// rounds after it, with no new user message. ok false: nothing to replay.
// An interrupt while it is open cancels it (questionCancelled) and ends the
// turn; the engine's stop suspends it again (ErrSuspended).
func (h *Harness) Replay(ctx context.Context) (res agent.TurnResult, ok bool, err error) {
	p := h.PendingReplay()
	if p == nil {
		return res, false, nil
	}
	ctx, end := h.interruptible(ctx)
	defer end()
	h.clearBudgetNotes()
	h.pending.replaying.Store(true)
	defer h.pending.replaying.Store(false)
	st := h.settleBegin(false)
	h.turnBegin(turnOrigin(p.Origin))
	h.ledgerBegin(turnOrigin(p.Origin))
	res, err = h.replayRun(ctx, p)
	h.settleEnd(false, st, err)
	res, err = h.finishInterrupt(ctx, res, err)
	h.untilTurnEnd(res, err)
	h.ledgerEnd(ctx, err)
	h.netWaitAfter(err)
	return res, true, err
}

func (h *Harness) replayRun(ctx context.Context, p *PendingAsk) (agent.TurnResult, error) {
	tc := llm.ToolCall{ID: p.CallID, Type: "function"}
	tc.Function.Name, tc.Function.Arguments = p.Tool, p.Args
	out, derr := h.Session.Dispatch(ctx, tc)
	if h.store.isSuspended(p.CallID) {
		return agent.TurnResult{}, ErrSuspended
	}
	e := agent.Entry{ID: uuid.New().String(), Kind: agent.KindToolResult, ToolCallID: p.CallID, ToolName: p.Tool,
		CreatedAt: time.Now().UnixNano()}
	if interrupted(ctx) {
		h.pendingWrite(nil) // the ask did, unless the cut came before it asked
		if strings.TrimSpace(out) == "" {
			out = questionCancelled(p.Tool)
		}
		e.Content = out
		h.store.appendSilent(e)
		return agent.TurnResult{}, context.Cause(ctx)
	}
	if derr != nil {
		out = "ERROR: " + derr.Error()
	}
	e.Content = out
	h.store.publish(e)
	// No prepareTurn: this goes on with a turn, and the model reads the
	// result last, as after any call. The asks and plan block went in with
	// the turn that asked; one after the result would end the prompt on a
	// status block (docs/decisions.md "The person's message ends the
	// prompt"). Each attempt still heals and delivers (prepareAttempt).
	return h.turnThenTriggers(ctx)
}
