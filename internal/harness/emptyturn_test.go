package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// emptyRunner answers every round with text (Gemini 4, 2026-11-19: "", every
// round, finish_reason stop, native MALFORMED_FUNCTION_CALL), after failing
// the first fails requests as a dropped stream. It keeps what each request
// sent.
type emptyRunner struct {
	mu    sync.Mutex
	text  string
	fails int
	sent  [][]llm.Message
}

func (r *emptyRunner) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.mu.Lock()
	r.sent = append(r.sent, msgs)
	fail := r.fails > 0
	if fail {
		r.fails--
	}
	r.mu.Unlock()
	if fail {
		return nil, midStreamDeath()
	}
	out := make(chan llm.StreamChunk, 2)
	out <- llm.StreamChunk{Content: r.text}
	out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}, FinishReason: "stop", NativeFinishReason: "MALFORMED_FUNCTION_CALL"}
	close(out)
	return out, nil
}

func (r *emptyRunner) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (r *emptyRunner) requests() [][]llm.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent
}

func startEmpty(t *testing.T, r *emptyRunner, notes *[]string) *Harness {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	var mu sync.Mutex
	cfg := Config{Workspace: dir, Servers: []mcpserver.Server{}, Client: r, Exec: &HostShell{Dir: dir},
		SessionFile: filepath.Join(dir, "20261119-120000.jsonl"), PlanNudgeRounds: -1, PlanNudgeFails: -1, PlanStallRounds: -1,
		StatsLedger: NewStatsLedger(filepath.Join(dir, "stats"))}
	cfg.Endpoint.Model = "gemini-4"
	cfg.OnNotify = func(s string) {
		mu.Lock()
		*notes = append(*notes, s)
		mu.Unlock()
	}
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// A turn whose every round is empty with native MALFORMED_FUNCTION_CALL
// (agentkit v0.41.1): three malformed-call repair rounds, then the turn ends
// with *agent.EmptyReplyError. Each repair is a ledger line. dun says it once, naming the
// model and the finish reasons, and keeps a record with them. The repair
// note is the model's: no notice of it, not in the history shown.
func TestEmptyReply_NoticeAndErrorsEntry(t *testing.T) {
	r := &emptyRunner{}
	var notes []string
	h := startEmpty(t, r, &notes)
	h.askOpen("fix the stubs dir")

	res, err := h.Ask(context.Background(), "please fix it")
	if !errors.Is(err, agent.ErrEmptyReply) || res.FinishReason != "stop" {
		t.Fatalf("reply %q finish %q err %v", res.Reply, res.FinishReason, err)
	}
	want := "gemini-4 returned an empty reply: the turn ended with no text from the model (finish_reason stop, native MALFORMED_FUNCTION_CALL)"
	if err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
	for _, n := range notes {
		if strings.Contains(n, "Your last reply was empty") || strings.Contains(strings.ToLower(n), "malformed_function_call") {
			t.Errorf("the repair note reached the person: %q", n)
		}
	}
	recs, err := ReadTurnErrors(TurnErrorsFile(h.cfg.SessionFile))
	if err != nil || len(recs) != 1 || recs[0].Error != want || recs[0].Model != "gemini-4" ||
		recs[0].Finish != "stop" || recs[0].NativeFinish != "MALFORMED_FUNCTION_CALL" {
		t.Fatalf("records %+v err %v", recs, err)
	}
	for _, it := range h.History() {
		if it.Kind == "notification" && strings.Contains(it.Content, "Your last reply was empty") {
			t.Errorf("the repair note is in the history shown: %q", it.Content)
		}
	}
	// The model read the repair notes: one in each request after the first.
	rq := r.requests()
	if len(rq) != 4 {
		t.Fatalf("%d requests, want 1 and 3 repairs", len(rq))
	}
	for i := 1; i < 4; i++ {
		if last := rq[i][len(rq[i])-1].Content; !strings.Contains(strings.ToLower(last), "malformed_function_call") {
			t.Errorf("request %d did not carry the repair note: %q", i, last)
		}
	}
	// The ledger: three repairs, their outcome (failed: the turn ended in
	// the empty reply), then the empty-reply end.
	b, err := os.ReadFile(filepath.Join(filepath.Dir(h.cfg.SessionFile), "stats", "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), `"kind":"repair",`); n != 3 || strings.Count(string(b), `"kind":"empty_reply"`) != 1 ||
		!strings.Contains(string(b), `"attempt":3,"cause":"malformed-call","outcome":"failed"`) {
		t.Errorf("ledger:\n%s", b)
	}
}

// A turn with a reply, and an automatic turn, end without the notice.
func TestEmptyReply_NotForAReply(t *testing.T) {
	r := &emptyRunner{text: "done"}
	var notes []string
	h := startEmpty(t, r, &notes)
	if _, err := h.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if strings.Contains(n, "empty reply") {
			t.Errorf("notice on a turn with a reply: %q", n)
		}
	}
	if recs, _ := ReadTurnErrors(TurnErrorsFile(h.cfg.SessionFile)); len(recs) != 0 {
		t.Errorf("records %+v", recs)
	}
}

// The person's message is the last thing the model reads before its first
// round: the turn's one asks block comes before it, as "[asks] …", and a
// retried attempt adds no second block.
func TestAsksBlock_OncePerTurnBeforeTheMessage(t *testing.T) {
	instantRetries(t)
	for _, fails := range []int{0, 2} {
		r := &emptyRunner{text: "ok", fails: fails}
		var notes []string
		h := startEmpty(t, r, &notes)
		h.askOpen("fix the stubs dir")

		if _, err := h.Ask(context.Background(), "and the docs too"); err != nil {
			t.Fatalf("fails %d: %v", fails, err)
		}
		rq := r.requests()
		// Each attempt's first round; the turn-end check adds a round after.
		if len(rq) < fails+1 {
			t.Fatalf("fails %d: %d requests", fails, len(rq))
		}
		for i, msgs := range rq[:fails+1] {
			last := msgs[len(msgs)-1]
			if last.Role != "user" || last.Content != "and the docs too" {
				t.Errorf("fails %d, request %d ends on %s %q, not the message", fails, i, last.Role, last.Content)
			}
			blocks := 0
			for _, m := range msgs {
				if strings.Contains(m.Content, "Open asks:") {
					blocks++
					if !strings.HasPrefix(m.Content, "[asks] Open asks:") {
						t.Errorf("fails %d: block rendered %q", fails, m.Content)
					}
				}
			}
			if blocks != 1 {
				t.Errorf("fails %d, request %d: %d asks blocks, want 1", fails, i, blocks)
			}
		}
		if hs := h.History(); !historyHas(hs, "[asks] Open asks:") {
			t.Errorf("fails %d: the history shows no [asks] block", fails)
		}
	}
}

func historyHas(items []HistoryItem, prefix string) bool {
	for _, it := range items {
		if it.Kind == "notification" && strings.HasPrefix(it.Content, prefix) {
			return true
		}
	}
	return false
}
