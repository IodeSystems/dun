package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/spendmeter"
)

// traceServer answers each chat request with reply (an SSE body) and keeps
// the X-Trace-Id each request carried.
func traceServer(t *testing.T, reply string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		ids = append(ids, r.Header.Get("X-Trace-Id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req-upstream-7")
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ids...)
	}
}

const okReply = `data: {"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}` + "\n\n" +
	`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}` + "\n\n" +
	"data: [DONE]\n\n"

// traceHarness is a root session on c with round stats in a ledger.
func traceHarness(t *testing.T, c *llm.Client) (*Harness, string) {
	t.Helper()
	dir := t.TempDir()
	h := newNoteHarness(t)
	h.cfg.SessionFile = filepath.Join(dir, "20261120-150405.jsonl")
	h.trace = newTracer(sessionOfFile(h.cfg.SessionFile))
	h.rounds.ledger = NewStatsLedger(dir)
	m := spendmeter.NewMeter()
	m.SetRoundHook(h.noteRound)
	c.OnUsage = m.NoteAs(spendmeter.Source{Provider: "p", Model: "m", Role: spendmeter.RoleMain})
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: h.teeTurn(c, nil), System: "s"}
	_ = h.store.Append(context.Background(), "dun", agent.Entry{ID: "u", Kind: agent.KindUser, Content: "hi", CreatedAt: 1})
	return h, dir
}

func ledgerRounds(t *testing.T, dir string) []RoundStat {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []RoundStat
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r RoundStat
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// A turn's request carries a trace id naming the session and the main role
// in X-Trace-Id; its round stats keep that id and the provider's request id.
func TestTrace_MainRoundHeaderAndStats(t *testing.T) {
	srv, sent := traceServer(t, okReply)
	c := llm.NewClient(srv.URL+"/v1", "", "m")
	h, dir := traceHarness(t, c)

	if _, err := h.turnThenTriggers(context.Background()); err != nil {
		t.Fatal(err)
	}
	ids := sent()
	if len(ids) != 1 {
		t.Fatalf("requests %v", ids)
	}
	p, ok := ParseTraceID(ids[0])
	if !ok || p.Session != "20261120-150405" || p.Role != "main" || p.Seq != "1" {
		t.Fatalf("X-Trace-Id %q parsed %+v %v", ids[0], p, ok)
	}
	rounds := ledgerRounds(t, dir)
	if len(rounds) != 1 || rounds[0].Trace != ids[0] || rounds[0].ProviderID != "req-upstream-7" {
		t.Fatalf("rounds %+v, want trace %s and provider id req-upstream-7", rounds, ids[0])
	}
	if h.trace.Last() != ids[0] {
		t.Errorf("last %q", h.trace.Last())
	}
}

// A side call gets its own id, with its kind as the role; a sub-agent's
// side call is scoped to the child.
func TestTrace_SideCallRole(t *testing.T) {
	srv, sent := traceServer(t, okReply)
	c := llm.NewClient(srv.URL+"/v1", "", "m")
	h, dir := traceHarness(t, c)
	child := &Harness{trace: h.trace.child(2)}
	for _, r := range []agent.LLMRunner{h.sideRunner("suggest", c), child.sideRunner("fold", c)} {
		ch, err := r.ChatStream(context.Background(), []llm.Message{{Role: "user", Content: "x"}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
	}
	ids := sent()
	if len(ids) != 2 {
		t.Fatalf("requests %v", ids)
	}
	if p, _ := ParseTraceID(ids[0]); p.Role != "suggest" {
		t.Errorf("side call id %q", ids[0])
	}
	if p, _ := ParseTraceID(ids[1]); p.Role != "child#2.fold" || p.Seq != "2" {
		t.Errorf("child side call id %q", ids[1])
	}
	rounds := ledgerRounds(t, dir)
	if len(rounds) != 2 || rounds[0].Trace != ids[0] || rounds[1].Trace != ids[1] {
		t.Errorf("rounds %+v", rounds)
	}
}

// A turn that ends in an error keeps the id of the request that failed.
func TestTrace_TurnErrorKeepsID(t *testing.T) {
	srv, sent := traceServer(t, `data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`+"\n\n"+"data: "+sseErrorEvent+"\n\n")
	c := llm.NewClient(srv.URL+"/v1", "", "m")
	c.RetryBudget, c.DisableTurnRetry = time.Millisecond, true
	h, _ := traceHarness(t, c)
	h.cfg.Endpoint = Endpoint{URL: srv.URL + "/v1", Model: "m"}

	if _, err := h.turnThenTriggers(context.Background()); err == nil {
		t.Fatal("the turn did not fail")
	}
	recs, err := ReadTurnErrors(TurnErrorsFile(h.cfg.SessionFile))
	if err != nil || len(recs) != 1 {
		t.Fatalf("%d records, err %v", len(recs), err)
	}
	ids := sent()
	if len(ids) == 0 || recs[0].Trace != ids[len(ids)-1] {
		t.Errorf("record trace %q, sent %v", recs[0].Trace, ids)
	}
}

// A round no traced stream claims is still written, without an id.
func TestTrace_UnclaimedRoundWritten(t *testing.T) {
	hub := &traceHub{}
	hub.begin()
	got := make(chan RoundStat, 1)
	if !hub.park(roundKey{prompt: 1}, RoundStat{Prompt: 1}, func(r RoundStat) { got <- r }) {
		t.Fatal("not parked while a stream is open")
	}
	select {
	case r := <-got:
		if r.Trace != "" {
			t.Errorf("trace %q", r.Trace)
		}
	case <-time.After(parkFor + 2*time.Second):
		t.Fatal("never written")
	}
	hub.end()
	if hub.park(roundKey{}, RoundStat{}, func(RoundStat) {}) {
		t.Error("parked with no stream open")
	}
}

// The "user" field goes only to endpoints known to take it (OpenRouter by
// host or dialect, api.openai.com); X-Session-Id and X-Trace-Id go to every
// endpoint.
func TestSessionTag_UserFieldOnlyWhereAccepted(t *testing.T) {
	for _, tc := range []struct {
		url, dialect string
		user         bool
	}{
		{"https://openrouter.ai/api/v1", "", true},
		{"https://api.openai.com/v1", "", true},
		{"https://litellm.example/v1", llm.DialectOpenRouter, true},
		{"http://127.0.0.1:8080/v1", "", false},
		{"https://llm.example/v1", llm.DialectLlamaCpp, false},
	} {
		var mu sync.Mutex
		var body map[string]any
		var hdr http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&body)
			hdr = r.Header.Clone()
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, okReply)
		}))
		// The client talks to the test server; the url SessionTag is told is
		// the endpoint being judged.
		c := llm.NewClient(srv.URL+"/v1", "", "m")
		SessionTag(c, "20261120-150405", tc.url, tc.dialect)
		tr := newTracer("20261120-150405")
		ch, err := tracedRunner{inner: c, t: tr, role: "suggest"}.ChatStream(context.Background(), []llm.Message{{Role: "user", Content: "x"}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
		srv.Close()
		mu.Lock()
		user, sent := body["user"]
		if sent != tc.user || (sent && user != "dun-20261120-150405") {
			t.Errorf("%s %q: user %v (sent %v), want sent %v", tc.url, tc.dialect, user, sent, tc.user)
		}
		if hdr.Get("X-Session-Id") != "dun-20261120-150405" || !strings.HasPrefix(hdr.Get("X-Trace-Id"), "dun-20261120-150405-") {
			t.Errorf("%s: X-Session-Id %q X-Trace-Id %q", tc.url, hdr.Get("X-Session-Id"), hdr.Get("X-Trace-Id"))
		}
		mu.Unlock()
	}
}
