package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/spendmeter"
)

// ledgerEvents reads dir's rounds file back as events (rounds too: Kind "").
func ledgerEvents(t *testing.T, dir string) []RoundEvent {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []RoundEvent
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var e RoundEvent
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, e)
	}
	return out
}

func repairHarness(t *testing.T) (*Harness, string) {
	dir := t.TempDir()
	h := &Harness{cfg: Config{SessionFile: "/x/20261125-1.jsonl", Provider: "openrouter", Endpoint: Endpoint{Model: "gemini-4"}}}
	h.rounds.ledger = NewStatsLedger(dir)
	return h, dir
}

// Each of agentkit's repair notices is a "repair" line, keyed by its tag;
// other notices are not.
func TestRepairs_NoticesAreLines(t *testing.T) {
	h, dir := repairHarness(t)
	for _, tag := range []string{agent.EmptyReplyTag, agent.ReasoningCutTag, agent.MalformedCallTag, "repetition", "tool_loop", "idle_check", "docs", ""} {
		h.recordEntry(agent.Entry{Kind: agent.KindNotification, Tag: tag, Content: "x"})
	}
	evs := ledgerEvents(t, dir)
	var got []string
	for _, e := range evs {
		if e.Kind != "repair" || e.Provider != "openrouter" || e.Model != "gemini-4" || e.Session != "20261125-1" {
			t.Errorf("line %+v", e)
		}
		got = append(got, e.Cause)
	}
	if strings.Join(got, ",") != "empty-reply,reasoning-cut,malformed-call,repetition,tool_loop,idle_check" {
		t.Errorf("causes %v", got)
	}
}

// A Turn's end resolves its repairs: repaired unless it ended in what the
// repair fails as; a cancelled Turn resolves none; MaxTurns is a turn_cap.
func TestRepairs_TurnEndOutcomes(t *testing.T) {
	h, dir := repairHarness(t)
	h.recordEntry(agent.Entry{Kind: agent.KindNotification, Tag: agent.EmptyReplyTag})
	h.recordEntry(agent.Entry{Kind: agent.KindNotification, Tag: "tool_loop"})
	h.noteRoundEvent(RoundEvent{Kind: "provider_fail_retry", Attempt: 1, Max: 3, WaitMS: 2000})
	h.noteRoundEvent(RoundEvent{Kind: "provider_fail_retry", Attempt: 2, Max: 3, WaitMS: 4000})
	h.noteTurnEnd(agent.TurnEnd{Reason: agent.TurnToolLoop})
	// Next Turn: an empty-reply repair, then the turn is cancelled.
	h.recordEntry(agent.Entry{Kind: agent.KindNotification, Tag: agent.EmptyReplyTag})
	h.noteTurnEnd(agent.TurnEnd{Reason: agent.TurnCanceled})
	h.noteTurnEnd(agent.TurnEnd{Reason: agent.TurnMaxTurns, Rounds: 400})
	out := map[string]RoundEvent{}
	caps := 0
	for _, e := range ledgerEvents(t, dir) {
		switch e.Kind {
		case "repair_outcome":
			if _, dup := out[e.Cause]; dup {
				t.Errorf("two outcomes for %s", e.Cause)
			}
			out[e.Cause] = e
		case "turn_cap":
			caps++
			if e.Attempt != 400 || e.Max != defaultTurnLimit {
				t.Errorf("turn cap %+v", e)
			}
		}
	}
	if len(out) != 3 || out["empty-reply"].Outcome != "repaired" || out["tool_loop"].Outcome != "failed" ||
		out["provider_fail"].Outcome != "repaired" || out["provider_fail"].Attempt != 2 || caps != 1 {
		t.Errorf("outcomes %+v caps %d", out, caps)
	}
}

// Tool results that are errors are lines by tool and class; a plain one,
// and one an interrupt cut, are not.
func TestRepairs_ToolErrors(t *testing.T) {
	h, dir := repairHarness(t)
	for _, r := range []struct{ tool, content string }{
		{"read", "ERROR: malformed json: invalid character. This call was NOT executed."},
		{"write", "ERROR: malformed json — truncation detected: the arguments were cut off after 900 characters"},
		{"nope", `ERROR: unknown tool "nope"`},
		{"exec", "ERROR: exec: no such file"},
		{"node_edit", `{"note":"x","incomplete":true}`},
		{"node_edit", `{"note":"x","validated":true}`},
		{"read", "fine"},
		{"exec", userInterruptedCall("exec")},
		{"exec", "ERROR: not run — the user interrupted the turn before this exec call started."},
	} {
		h.recordEntry(agent.Entry{Kind: agent.KindToolResult, ToolName: r.tool, Content: r.content})
	}
	var got []string
	for _, e := range ledgerEvents(t, dir) {
		got = append(got, e.Kind+":"+e.Tool+":"+e.Cause)
	}
	want := "tool_error:read:malformed_json,tool_error:write:truncated,tool_error:nope:unknown_tool,tool_error:exec:error,tool_error:node_edit:warned"
	if strings.Join(got, ",") != want {
		t.Errorf("got %v", got)
	}
}

// A request retry is a line with its kind, status and wait; its end has
// the outcome; a provider resend's note is not a retry line (it has its own).
func TestRepairs_RetryLines(t *testing.T) {
	h, dir := repairHarness(t)
	h.noteRetryEvent(noteFromEvent(llm.RetryEvent{Kind: llm.Retry429, Attempt: 1, Status: 429, Delay: 3 * time.Second}))
	h.noteRetryEvent(noteFromEvent(llm.RetryEvent{Kind: llm.RetryRecovered, Attempt: 2}))
	h.noteRetryEvent(RetryNote{Scope: "turn", Kind: "giveup", Attempt: 4})
	h.noteRetryEvent(providerFailNote(agent.ProviderFailRetry{Attempt: 1, Max: 3}))
	evs := ledgerEvents(t, dir)
	if len(evs) != 3 {
		t.Fatalf("lines %+v", evs)
	}
	if e := evs[0]; e.Kind != "retry" || e.Scope != "request" || e.Cause != "429" || e.Status != 429 || e.WaitMS != 3000 || e.Outcome != "" {
		t.Errorf("wait %+v", e)
	}
	if evs[1].Outcome != "recovered" || evs[2].Outcome != "gave_up" || evs[2].Scope != "turn" {
		t.Errorf("ends %+v", evs[1:])
	}
}

// The round after a resend or a repair carries its cause (RoundStat.Repair);
// the one after that does not.
func TestRepairs_RoundTagged(t *testing.T) {
	h, dir := repairHarness(t)
	src := spendmeter.Source{Role: "model", Model: "gemini-4"}
	h.noteRoundEvent(RoundEvent{Kind: "provider_fail_retry", Attempt: 1})
	h.noteRound(src, llm.Usage{PromptTokens: 100, CompletionTokens: 10})
	h.noteRound(src, llm.Usage{PromptTokens: 120, CompletionTokens: 10})
	var rounds []RoundStat
	b, _ := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r RoundStat
		_ = json.Unmarshal([]byte(l), &r)
		if r.Kind == "" {
			rounds = append(rounds, r)
		}
	}
	if len(rounds) != 2 || rounds[0].Repair != "provider_fail" || rounds[1].Repair != "" {
		t.Errorf("rounds %+v", rounds)
	}
}

// A sub-agent's events reach its root's ledger, under its own model.
func TestRepairs_ChildToRootLedger(t *testing.T) {
	root, dir := repairHarness(t)
	child := &Harness{parent: root, cfg: Config{Provider: "local", Endpoint: Endpoint{Model: "qwen"}}}
	child.recordEntry(agent.Entry{Kind: agent.KindNotification, Tag: "repetition"})
	evs := ledgerEvents(t, dir)
	if len(evs) != 1 || evs[0].Model != "qwen" || evs[0].Cause != "repetition" {
		t.Errorf("lines %+v", evs)
	}
}

// A cut turn is a cancel line, by who cut it.
func TestRepairs_CancelLines(t *testing.T) {
	h, dir := repairHarness(t)
	h.store, _ = openSessionStore("")
	for _, reason := range []string{"", "dund restarted into build X while this turn was running"} {
		ctx, end := h.interruptible(context.Background())
		h.Interrupt(reason)
		if _, err := h.finishInterrupt(ctx, agent.TurnResult{}, context.Canceled); !errors.Is(err, ErrInterrupted) {
			t.Fatal(err)
		}
		end()
	}
	// A turn that ended on its own is no cancel.
	if _, err := h.finishInterrupt(context.Background(), agent.TurnResult{}, nil); err != nil {
		t.Fatal(err)
	}
	evs := ledgerEvents(t, dir)
	if len(evs) != 2 || evs[0].Kind != "cancel" || evs[0].Cause != "user" || evs[1].Cause != "restart" {
		t.Errorf("lines %+v", evs)
	}
}

// A fold is a line with its cause and its before and after; a fold that
// failed is fold_failed.
func TestRepairs_FoldLines(t *testing.T) {
	h, dir := repairHarness(t)
	h.cache = &cacheStats{}
	h.noteCompactionCause(agent.CompactionInfo{TokensBefore: 90000, TokensAfter: 12000}, foldByOverflow)
	h.foldBegin()
	h.foldEnd(errors.New("summarize: status 502"))
	h.foldEnd(errors.New("no fold open")) // not a fold: no line
	evs := ledgerEvents(t, dir)
	if len(evs) != 2 || evs[0].Kind != "fold" || evs[0].Cause != "overflow" || evs[0].Before != 90000 || evs[0].After != 12000 ||
		evs[1].Kind != "fold_failed" || !strings.Contains(evs[1].Reason, "502") {
		t.Errorf("lines %+v", evs)
	}
}

// A length cut is a line, and a Turn that ends in overflow fails it.
func TestRepairs_LengthCut(t *testing.T) {
	h, dir := repairHarness(t)
	h.onOverflow(context.Background(), agent.Overflow{PromptTokens: 1000, Generated: 500, Attempt: 1})
	h.noteTurnEnd(agent.TurnEnd{Reason: agent.TurnOverflow})
	evs := ledgerEvents(t, dir)
	if len(evs) != 2 || evs[0].Kind != "length_cut" || evs[0].Cause != "reply" || evs[0].Before != 1000 ||
		evs[1].Kind != "repair_outcome" || evs[1].Cause != "length_cut" || evs[1].Outcome != "failed" {
		t.Errorf("lines %+v", evs)
	}
}

// A capability clamp is one line per model, field and values per process.
func TestRepairs_CapClamp(t *testing.T) {
	dir := t.TempDir()
	prev := capsLedger.Swap(NewStatsLedger(dir))
	defer capsLedger.Store(prev)
	tr := llm.Translation{Key: "effort", Sent: "high", Wanted: "xhigh", Reason: "nearest above"}
	noteCapClamp("https://e.test", "m-clamp-test", tr)
	noteCapClamp("https://e.test", "m-clamp-test", tr)
	evs := ledgerEvents(t, dir)
	if len(evs) != 1 || evs[0].Kind != "cap_clamp" || evs[0].Key != "effort" || evs[0].Sent != "high" || evs[0].Wanted != "xhigh" || evs[0].Model != "m-clamp-test" {
		t.Errorf("lines %+v", evs)
	}
}

// A command's time is a cmd_time line.
func TestRepairs_CmdTime(t *testing.T) {
	h, dir := repairHarness(t)
	h.NoteCmdTime("/mcp", "engine", 420*time.Millisecond)
	evs := ledgerEvents(t, dir)
	if len(evs) != 1 || evs[0].Kind != "cmd_time" || evs[0].Cmd != "/mcp" || evs[0].MS != 420 || evs[0].Surface != "engine" {
		t.Errorf("lines %+v", evs)
	}
}
