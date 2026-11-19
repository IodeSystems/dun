package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// mandatoryBody is the user's 400, verbatim (2026-11-18, google/gemini-4-hydrogen
// through llm.iodesystems.com).
const mandatoryBody = `{"error":{"message":"Reasoning is mandatory for this endpoint and cannot be disabled.","code":400,"metadata":{"provider_name":null}}}`

// mandatoryServer is an endpoint whose model cannot stop reasoning: a request
// that asks for off, in any dialect's form, gets mandatoryBody (or refuse
// when set); any other gets reply, streamed. bodies keeps every request.
type mandatoryServer struct {
	*httptest.Server
	reply  string
	refuse string
	mu     sync.Mutex
	bodies []map[string]any
}

func newMandatoryServer(t *testing.T, reply string) *mandatoryServer {
	s := &mandatoryServer{reply: reply, refuse: mandatoryBody}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		if asksOff(body) {
			w.WriteHeader(400)
			io.WriteString(w, s.refuse)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		c, _ := json.Marshal(s.reply)
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":`+string(c)+`},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

// asksOff is off as each dialect sends it.
func asksOff(body map[string]any) bool {
	if kw, ok := body["chat_template_kwargs"].(map[string]any); ok && kw["enable_thinking"] == false {
		return true
	}
	if r, ok := body["reasoning"].(map[string]any); ok && (r["enabled"] == false || r["effort"] == "none") {
		return true
	}
	return body["reasoning_effort"] == "none"
}

func (s *mandatoryServer) sent() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

func (s *mandatoryServer) client(dialect string) *llm.Client {
	c := llm.NewClient(s.URL+"/v1", "", "google/gemini-4-hydrogen")
	c.SetReasoningDialect(dialect)
	c.RetryBudget = time.Millisecond
	TrackCaps(c, "")
	return c
}

// offDialects are the dialects a thinking-only model is reached by: llama.cpp
// named, llama.cpp detected (no dialect set), and OpenRouter.
var offDialects = []string{llm.DialectLlamaCpp, llm.DialectAuto, llm.DialectOpenRouter}

func dialectName(d string) string {
	if d == llm.DialectAuto {
		return "auto"
	}
	return d
}

func foldMsgs() []llm.Message {
	return []llm.Message{{Role: "system", Content: agent.DefaultSummarySystem}, {Role: "user", Content: "events"}}
}

// A fold on a model that cannot stop reasoning folds: the refused off is
// sent once more at the lowest effort, the refusal is stored, and the next
// fold sends the lowest effort from the start. The dialects dun reaches such
// a model by: llm.iodesystems.com (llama.cpp, named or detected) and
// OpenRouter. agentkit does the resend (v0.34.0); dun's guard did before.
func TestReasoningOff_FoldOnMandatoryReasoning(t *testing.T) {
	for _, dialect := range offDialects {
		t.Run(dialectName(dialect), func(t *testing.T) {
			capsHome(t)
			srv := newMandatoryServer(t, foldGoodSummary(repoAsk))
			h := newNoteHarness(t)
			h.ask = foldAsk{id: "u", text: repoAsk, at: 1}
			r := foldRunner{inner: srv.client(dialect), h: h}

			text, errMsg, _, err := collectText(foldCtx(t), r, foldMsgs(), nil, h.foldOpts())
			if err != nil || errMsg != "" || text != foldGoodSummary(repoAsk) {
				t.Fatalf("fold: %q %q %v", text, errMsg, err)
			}
			sent := srv.sent()
			if len(sent) != 2 || !asksOff(sent[0]) || asksOff(sent[1]) {
				t.Fatalf("want off refused, then sent without off; sent %v", sent)
			}
			if dialect == llm.DialectOpenRouter {
				if r, _ := sent[1]["reasoning"].(map[string]any); r["effort"] != "minimal" {
					t.Errorf("retry sent reasoning %v, want effort minimal", sent[1]["reasoning"])
				}
			}

			// Remembered: stored for (endpoint, model), and the next fold, on a
			// new client seeded from the store, sends no off at all.
			waitThinkingOnly(t, srv.client(dialect))
			r = foldRunner{inner: srv.client(dialect), h: h}
			if _, _, _, err := collectText(foldCtx(t), r, foldMsgs(), nil, h.foldOpts()); err != nil {
				t.Fatal(err)
			}
			if sent := srv.sent(); len(sent) != 3 || asksOff(sent[2]) {
				t.Errorf("the next fold sent off again: %d requests, last %v", len(sent), sent[len(sent)-1])
			}
		})
	}
}

// The rescue fold on such a model folds the same way: one refusal, one
// resend without off, and the next rescue on a new client sends no off.
func TestReasoningOff_RescueFoldOnMandatoryReasoning(t *testing.T) {
	for _, dialect := range offDialects {
		t.Run(dialectName(dialect), func(t *testing.T) {
			capsHome(t)
			srv := newMandatoryServer(t, "handoff")
			h := newNoteHarness(t)
			older := []agent.Entry{{Kind: agent.KindUser, Content: "events"}}
			h.client = srv.client(dialect)
			if s, err := h.summarizeRescue(context.Background(), older, 500); err != nil || s != "handoff" {
				t.Fatalf("rescue: %q %v", s, err)
			}
			sent := srv.sent()
			if len(sent) != 2 || !asksOff(sent[0]) || asksOff(sent[1]) {
				t.Fatalf("want off refused, then sent without off; sent %v", sent)
			}
			waitThinkingOnly(t, srv.client(dialect))
			h.client = srv.client(dialect)
			if _, err := h.summarizeRescue(context.Background(), older, 500); err != nil {
				t.Fatal(err)
			}
			if sent := srv.sent(); len(sent) != 3 || asksOff(sent[2]) {
				t.Errorf("the next rescue sent off again: %d requests, last %v", len(sent), sent[len(sent)-1])
			}
		})
	}
}

// waitThinkingOnly waits for the background store to hold c's model as
// thinking-only.
func waitThinkingOnly(t *testing.T, c *llm.Client) {
	t.Helper()
	snap := c.Capabilities()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if f, ok := LoadCaps(snap.Endpoint, snap.Model).Facts[llm.CapReasoningThinkingOnly]; ok && f.Value == "true" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("reasoning.thinking_only was not stored")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A round at /effort off on such a model goes out at the lowest effort, and
// the person is told once.
func TestReasoningOff_EffortOffRoundNotesOnce(t *testing.T) {
	for _, dialect := range offDialects {
		t.Run(dialectName(dialect), func(t *testing.T) { effortOffRoundNotesOnce(t, dialect) })
	}
}

func effortOffRoundNotesOnce(t *testing.T, dialect string) {
	capsHome(t)
	srv := newMandatoryServer(t, "hi")
	h := newNoteHarness(t)
	h.effort.Store(EffortOff)
	var notes []string
	h.cfg.OnNotify = func(s string) { notes = append(notes, s) }
	r := h.teeTurn(srv.client(dialect), nil)
	for range 2 {
		opts := &llm.ChatOpts{}
		h.sessionOpts(opts, RoleMain)
		text, errMsg, _, err := collectText(context.Background(), r, []llm.Message{{Role: "user", Content: "hi"}}, nil, opts)
		if err != nil || errMsg != "" || text != "hi" {
			t.Fatalf("round: %q %q %v", text, errMsg, err)
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "cannot turn reasoning off") {
		t.Errorf("notes = %q, want one", notes)
	}
	// Refused once; the second round knew.
	if n := len(srv.sent()); n != 3 {
		t.Errorf("%d requests, want 3 (refused, retried, then the lowest effort directly)", n)
	}
}

// Any other 400 to a fold's off is the call's: not sent again, not learned
// as thinking-only.
func TestReasoningOff_OtherRefusalNotRetried(t *testing.T) {
	capsHome(t)
	srv := newMandatoryServer(t, "hi")
	srv.refuse = `{"error":{"message":"Invalid value for max_tokens","code":400}}`
	c := srv.client(llm.DialectOpenRouter)
	_, _, _, err := collectText(context.Background(), c, []llm.Message{{Role: "user", Content: "hi"}}, nil, (&Harness{}).foldOpts())
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("err = %v, want the refusal", err)
	}
	if n := len(srv.sent()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
	if thinkingOnly(c) {
		t.Error("learned thinking-only from another refusal")
	}
}
