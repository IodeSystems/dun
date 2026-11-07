package dun

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// glmNone is Fireworks GLM-5.3's real answer to reasoning_effort "none"
// (2026-11-07, through corrallm).
const glmNone = `{"error":{"message":"GLM-5.3 is a thinking-only model; disabling thinking (reasoning_effort='none') is not supported.","type":"invalid_request_error","code":400}}`

// What a Client learns is stored in DUN_HOME and seeded into the next Client
// for the same (endpoint, model): the refusal is paid for once.
func TestCaps_LearnStoreSeed(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"none"`) {
			w.WriteHeader(400)
			io.WriteString(w, glmNone)
			return
		}
		io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	newClient := func() *llm.Client {
		c := llm.NewClient(srv.URL+"/v1", "", "glm")
		c.SetReasoningDialect(llm.DialectOpenAI)
		c.RetryBudget = time.Millisecond
		TrackCaps(c, "")
		return c
	}
	c := newClient()
	_, _, err := c.ChatWithOpts(context.Background(), []llm.Message{{Role: "user", Content: "hi"}}, nil, &llm.ChatOpts{ReasoningEffort: "none"})
	if err == nil {
		t.Fatal("the refusal did not come back as an error")
	}
	if only, ok := c.Capabilities().ThinkingOnly(); !ok || !only {
		t.Fatalf("thinking-only not learned from the real GLM body: %+v", c.Capabilities().Facts)
	}
	// Stored in the background: wait for the file.
	snap := c.Capabilities()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := LoadCaps(snap.Endpoint, snap.Model).Facts[llm.CapReasoningThinkingOnly]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the learned fact was not stored in DUN_HOME")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if only, ok := newClient().Capabilities().ThinkingOnly(); !ok || !only {
		t.Error("a new client was not seeded from the store")
	}
}

// effort_off is pinned as a host fact; /model shows where each fact came from.
func TestCaps_EffortOffPinned(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	c := llm.NewClient("https://llm.example/v1", "", "glm")
	TrackCaps(c, "low")
	caps := c.Capabilities()
	if off, ok := caps.OffEffort(); !ok || off != "low" || caps.Facts[llm.CapReasoningOff].Source != llm.SourceHost {
		t.Errorf("effort_off not pinned as host: %+v", caps.Facts)
	}
	if r := CapsReport(caps); !strings.Contains(r, "reasoning.off") || !strings.Contains(r, "host") {
		t.Errorf("report: %s", r)
	}
}

// With effort_off pinned, off goes on the wire as it: a fold never sends the
// "none" a thinking-only model refuses.
func TestCaps_PinTranslatesOff(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	c := llm.NewClient(srv.URL+"/v1", "", "glm")
	c.SetReasoningDialect(llm.DialectOpenAI)
	TrackCaps(c, "low")
	h := &Harness{}
	if _, _, err := c.ChatWithOpts(context.Background(), []llm.Message{{Role: "user", Content: "fold"}}, nil, h.foldOpts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, `"reasoning_effort":"low"`) {
		t.Errorf("fold sent %s, want reasoning_effort low", sent)
	}
}
