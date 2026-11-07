package dun

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A gateway with no /props states the window in its model list: OpenRouter
// (context_length), vLLM (max_model_len), LiteLLM (/model/info
// max_input_tokens). USER 2026-10-26, critical: dun did not learn it.
func TestListedWindow(t *testing.T) {
	for name, tc := range map[string]struct {
		path, body string
		want       int
	}{
		"openrouter": {"/v1/models", `{"data":[{"id":"other","context_length":1},{"id":"deepseek/x","context_length":163840}]}`, 163840},
		"top":        {"/v1/models", `{"data":[{"id":"deepseek/x","top_provider":{"context_length":65536}}]}`, 65536},
		"vllm":       {"/v1/models", `{"data":[{"id":"deepseek/x","max_model_len":32768}]}`, 32768},
		"anthropic":  {"/v1/models", `{"data":[{"id":"deepseek/x","max_input_tokens":1000000,"max_tokens":128000}]}`, 1000000},
		"litellm":    {"/model/info", `{"data":[{"model_name":"deepseek/x","model_info":{"max_input_tokens":128000,"max_tokens":8192}}]}`, 128000},
		"none":       {"/v1/models", `{"data":[{"id":"deepseek/x"}]}`, 0},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != tc.path || r.Header.Get("Authorization") != "Bearer k" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(tc.body))
		}))
		got := listedWindow(context.Background(), Endpoint{URL: srv.URL + "/v1", Key: "k", Model: "deepseek/x"})
		srv.Close()
		if got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}

// A URL with no /v1 is the host (agentkit's chatURL adds it for chat): the
// list is at /v1/models, and /models may be a web page that answers 200.
// corrallm at https://llm.iodesystems.com did exactly that (2026-11-07).
func TestListedWindow_HostOnlyURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"glm","context_length":1048576,"thinking_default":"on"}]}`))
		case "/models":
			w.Write([]byte("<!doctype html><html><title>corrallm</title></html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e := Endpoint{URL: srv.URL, Model: "glm"}
	if got := listedWindow(context.Background(), e); got != 1048576 {
		t.Errorf("window %d, want 1048576 from /v1/models", got)
	}
	if th, ok := listedThinking(context.Background(), e); !ok || th.Default != "on" {
		t.Errorf("thinking %+v %v, want the /v1/models entry", th, ok)
	}
}

// A window set for a model wins over the server and is kept for the next
// session of that model only; auto forgets it.
func TestSetWindow(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("DUN_CONTEXT_TOKENS", "")
	e := Endpoint{URL: "https://litellm.corp/v1", Model: "deepseek-1017"}
	h := &Harness{cfg: Config{Endpoint: e, Client: nil}}
	msg, err := h.SetWindow(context.Background(), 131072)
	if err != nil || h.windowTokens() != 131072 || !strings.Contains(msg, "131,072") {
		t.Fatalf("set: %q %v %d", msg, err, h.windowTokens())
	}
	h2 := &Harness{cfg: Config{Endpoint: e}}
	if n, src := h2.askWindow(context.Background(), windowSayer{220160}); n != 131072 || !strings.Contains(src, "set for deepseek-1017") {
		t.Errorf("the next session: %d from %q", n, src)
	}
	other := &Harness{cfg: Config{Endpoint: Endpoint{URL: e.URL, Model: "other"}}}
	if n, _ := other.askWindow(context.Background(), windowSayer{220160}); n != 220160 {
		t.Errorf("another model took it: %d", n)
	}
	if _, err := h.SetWindow(context.Background(), 0); err != nil || savedWindow(e) != 0 {
		t.Errorf("auto: %v, saved %d", err, savedWindow(e))
	}
	for s, want := range map[string]int{"131072": 131072, "128k": 128000, "1m": 1000000, "200,000": 200000} {
		if n, err := ParseTokens(s); err != nil || n != want {
			t.Errorf("%s: %d %v", s, n, err)
		}
	}
	if _, err := ParseTokens("lots"); err == nil {
		t.Error("lots parsed")
	}
}
