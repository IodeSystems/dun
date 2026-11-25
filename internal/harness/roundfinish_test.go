package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/spendmeter"
)

// An OpenRouter-style stream (finish chunk, then a usage-only chunk) must
// reach the rounds ledger with finish and native_finish.
func TestRoundStats_RecordsNativeFinish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"id":"g1","model":"google/gemini","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
			`{"id":"g1","model":"google/gemini","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop","native_finish_reason":"MALFORMED_FUNCTION_CALL"}]}`,
			`{"id":"g1","model":"google/gemini","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":10}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	dir := t.TempDir()
	h := &Harness{cfg: Config{SessionFile: "/x/20261124-1.jsonl"}}
	h.rounds.ledger = NewStatsLedger(dir)
	m := spendmeter.NewMeter()
	m.SetRoundHook(h.noteRound)
	c := llm.NewClient(srv.URL, "k", "google/gemini")
	c.OnUsage = m.NoteAs(spendmeter.Source{Provider: "openrouter", Model: "google/gemini", Role: spendmeter.RoleMain})
	ch, err := c.ChatStream(context.Background(), []llm.Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	b, err := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var r RoundStat
	if err := json.Unmarshal([]byte(strings.Split(strings.TrimSpace(string(b)), "\n")[0]), &r); err != nil {
		t.Fatal(err)
	}
	if r.Finish != "stop" || r.NativeFinish != "MALFORMED_FUNCTION_CALL" {
		t.Errorf("finish %q native %q\n%s", r.Finish, r.NativeFinish, b)
	}
}
