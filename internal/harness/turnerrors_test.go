package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// sseErrorEvent is the event llm.iodesystems.com sent (aw4
// i_1m7rqbptp6z6f22q6jgff8), with the fields agentkit drops.
const sseErrorEvent = `{"error":{"message":"JSON error injected into SSE stream","type":"server_error","code":"injected"}}`

// A turn that dies on an SSE error event leaves a record in the session's
// errors file: the time, provider, endpoint host, model and error text.
// agentkit v0.37.0 keeps only the event's message, so that is the payload
// there is; this test fails, and should be changed to expect the raw event,
// once agentkit hands it on.
func TestTurnError_SSEErrorRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`+"\n\n")
		io.WriteString(w, "data: "+sseErrorEvent+"\n\n")
	}))
	t.Cleanup(srv.Close)
	c := llm.NewClient(srv.URL+"/v1", "", "qwen-test")
	c.RetryBudget, c.DisableTurnRetry = time.Millisecond, true

	h := newNoteHarness(t)
	h.cfg.SessionFile = filepath.Join(t.TempDir(), "20261118-200000.jsonl")
	h.cfg.Provider = "iode"
	h.cfg.Endpoint = Endpoint{URL: srv.URL + "/v1", Model: "qwen-test"}
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: c, System: "s"}
	_ = h.store.Append(context.Background(), "dun", agent.Entry{ID: "u", Kind: agent.KindUser, Content: "hi", CreatedAt: 1})

	if _, err := h.turnThenTriggers(context.Background()); err == nil {
		t.Fatal("the turn did not fail")
	}
	recs, err := ReadTurnErrors(TurnErrorsFile(h.cfg.SessionFile))
	if err != nil || len(recs) != 1 {
		t.Fatalf("%d records, err %v", len(recs), err)
	}
	r := recs[0]
	u, _ := url.Parse(srv.URL)
	if r.Session != "20261118-200000" || r.Provider != "iode" || r.Host != u.Host || r.Model != "qwen-test" || r.Time.IsZero() {
		t.Errorf("record = %+v", r)
	}
	if !strings.Contains(r.Error, "JSON error injected into SSE stream") {
		t.Errorf("error %q lacks the server's message", r.Error)
	}
	if r.Status != 0 || r.Payload != "" {
		t.Errorf("status %d payload %q: agentkit v0.37.0 keeps neither for an SSE error", r.Status, r.Payload)
	}
}

// A refused request's status and body come from agentkit's typed errors; a
// plain one's status from its text.
func TestTurnErrorRecord_StatusAndBody(t *testing.T) {
	ep := Endpoint{URL: "https://llm.example.com/v1", Model: "m"}
	r := turnErrorRecord(fmt.Errorf("agent: chat: %w", &llm.InputTooLargeError{Status: 400, Body: `{"error":"too big"}`}), "p", ep, time.Now())
	if r.Status != 400 || r.Payload != `{"error":"too big"}` || r.Host != "llm.example.com" {
		t.Errorf("typed: %+v", r)
	}
	r = turnErrorRecord(errors.New("agent: chat: llm: status 502: bad gateway"), "p", ep, time.Now())
	if r.Status != 502 || r.Payload != "" {
		t.Errorf("text: %+v", r)
	}
}

// The file keeps the newest 200 records, and one payload at most 8 KiB.
func TestTurnErrors_Capped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.errors.jsonl")
	for i := range turnErrorsKept + 25 {
		if err := appendTurnError(path, TurnError{Time: time.Unix(int64(i), 0), Error: fmt.Sprintf("e%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := ReadTurnErrors(path)
	if err != nil || len(recs) != turnErrorsKept {
		t.Fatalf("%d records, err %v", len(recs), err)
	}
	if recs[0].Error != "e25" || recs[len(recs)-1].Error != fmt.Sprintf("e%d", turnErrorsKept+24) {
		t.Errorf("kept %s … %s, want the newest", recs[0].Error, recs[len(recs)-1].Error)
	}
	big := &llm.BudgetSpentError{Status: 402, Body: strings.Repeat("é", 10_000)}
	r := turnErrorRecord(big, "", Endpoint{}, time.Now())
	if len(r.Payload) > turnErrorPayloadMax+32 || !strings.Contains(r.Payload, "bytes cut]") || !strings.HasPrefix(r.Payload, "éé") {
		t.Errorf("payload %d bytes, ends %q", len(r.Payload), r.Payload[len(r.Payload)-20:])
	}
}

// No key and no Authorization header reaches the file.
func TestTurnErrors_Scrubbed(t *testing.T) {
	const key = "iode-live-0123456789abcdef"
	body := `{"error":"denied","request":{"headers":{"Authorization":"Bearer ` + key + `","x-api-key":"k-abcdefgh"}},"api_key":"sk-proj-ABCDEFGH12345678"}`
	err := fmt.Errorf("agent: chat: %w", &llm.InputTooLargeError{Status: 401, Body: body})
	r := turnErrorRecord(fmt.Errorf("%w (Authorization: Bearer abcdefgh12345678)", err), "", Endpoint{Key: key}, time.Now())
	for _, s := range []string{r.Error, r.Payload} {
		for _, secret := range []string{key, "abcdefgh12345678", "k-abcdefgh", "sk-proj-ABCDEFGH12345678"} {
			if strings.Contains(s, secret) {
				t.Errorf("%q survived in %s", secret, s)
			}
		}
	}
	if !strings.Contains(r.Payload, `"denied"`) || !strings.Contains(r.Payload, "[redacted]") {
		t.Errorf("payload = %s", r.Payload)
	}
}
