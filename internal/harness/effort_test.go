package harness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// Each level's knobs on a request, and the level saved for the next
// session (USER 2026-10-26: a server with thinking off by default showed
// none; dun sent nothing to ask for it).
func TestEffort(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	// dun's names to agentkit's (effortclamp.go): default is unset, off the
	// old name of none, the rest the same; a round's window budget is not
	// sent, as before.
	for level, want := range map[string]llm.Effort{
		EffortDefault: llm.EffortUnset,
		"off":         llm.EffortNone,
		EffortOff:     llm.EffortNone,
		EffortMinimal: llm.EffortMinimal,
		EffortLow:     llm.EffortLow,
		EffortMedium:  llm.EffortMedium,
		EffortHigh:    llm.EffortHigh,
		EffortXHigh:   llm.EffortXHigh,
		EffortMax:     llm.EffortMax,
	} {
		n := 9000
		o := &llm.ChatOpts{Effort: llm.EffortHigh, ReasoningBudgetTokens: &n} // a level before, cleared by the next
		efforts.Apply(o, level)
		if o.Effort != want || o.ReasoningBudgetTokens != nil {
			t.Errorf("%s: effort %q budget %v", level, o.Effort, o.ReasoningBudgetTokens)
		}
	}
	h := &Harness{}
	if h.Effort() != EffortDefault || LoadEffort() != EffortDefault {
		t.Fatal("not default before one is set")
	}
	if err := h.SetEffort("HIGH"); err != nil || h.Effort() != EffortHigh || LoadEffort() != EffortHigh {
		t.Errorf("set: %v %s %s", err, h.Effort(), LoadEffort())
	}
	if err := h.SetEffort("huge"); err == nil {
		t.Error("an unknown level was taken")
	}
	if err := h.SetEffort("off"); err != nil || h.Effort() != EffortOff {
		t.Errorf("off: %v %s", err, h.Effort())
	}
}

// A level saved before agentkit's scale (off, low, medium, high) still
// reads: off as none.
func TestSavedEffort_OldValues(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	for saved, want := range map[string]string{"off": EffortOff, "low": EffortLow, "medium": EffortMedium, "high": EffortHigh, "default": EffortDefault} {
		if err := os.MkdirAll(DunHome(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(effortFile(), []byte(`{"level":"`+saved+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok := SavedEffort(); !ok || got != want {
			t.Errorf("saved %q: read %q %v", saved, got, ok)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// corrallm states what a request that sends no thinking settings gets
// (thinking_* beside context_length); /effort default says it, and says
// "the server decides" when the list does not (USER 2026-10-30).
func TestServerThinking_DefaultInWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"data":[{"id":"chat","context_length":86016,"thinking_default":"on","thinking_modes":["off","on"],
			"thinking_effort":"medium","thinking_budget":{"fraction_of_remaining":0.2,"min":2048,"max":8192}},
			{"id":"quiet","thinking_default":"off"},{"id":"fw","context_length":1048576}]}`))
	}))
	defer srv.Close()
	for model, want := range map[string]string{
		"chat":  "nothing sent: the server thinks at medium, up to 20% of what is left (2k–8k tokens)",
		"quiet": "nothing sent: the server does not think",
		"fw":    EffortAbout[EffortDefault],
	} {
		h := &Harness{cfg: Config{Endpoint: Endpoint{URL: srv.URL + "/v1", Model: model}}}
		h.LearnServerThinking(context.Background())
		if got := h.EffortWords(EffortDefault); got != want {
			t.Errorf("%s: %q, want %q", model, got, want)
		}
		if got := h.EffortWords(EffortHigh); got != EffortAbout[EffortHigh] {
			t.Errorf("%s: high is %q", model, got)
		}
	}
}
