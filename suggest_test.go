package dun

import (
	"context"
	"reflect"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

func TestParseSuggestions(t *testing.T) {
	// Wrapped in prose (small models do this), unsorted, one empty (dropped).
	s := `sure: {"suggestions":[{"text":"run the tests","prob":0.2},{"text":"  ","prob":0.9},{"text":"commit it","prob":0.6}]} ok`
	got := parseSuggestions(s)
	if len(got) != 2 {
		t.Fatalf("want 2 (empty dropped), got %d: %+v", len(got), got)
	}
	if got[0].Text != "commit it" { // 0.6 sorts above 0.2
		t.Fatalf("should sort by prob desc, got %+v", got)
	}

	// Probabilities clamp to [0,1].
	if c := parseSuggestions(`{"suggestions":[{"text":"x","prob":1.5}]}`); len(c) != 1 || c[0].Prob != 1 {
		t.Fatalf("clamp failed: %+v", c)
	}

	// No JSON → nil.
	if parseSuggestions("no json here") != nil {
		t.Fatal("non-JSON should yield nil")
	}
}

func TestParseSuggestions_FillerFilter(t *testing.T) {
	// Filler phrases are dropped.
	for _, phrase := range []string{"looks good", "thanks", "ok", "done", "great", "awesome"} {
		s := `{"suggestions":[{"text":"` + phrase + `","prob":0.9}]}`
		if got := parseSuggestions(s); len(got) != 0 {
			t.Errorf("filler %q should be dropped, got %+v", phrase, got)
		}
	}

	// Case-insensitive.
	for _, phrase := range []string{"Looks Good", "THANKS", "Ok", "Done"} {
		s := `{"suggestions":[{"text":"` + phrase + `","prob":0.9}]}`
		if got := parseSuggestions(s); len(got) != 0 {
			t.Errorf("filler %q should be dropped (case-insensitive), got %+v", phrase, got)
		}
	}

	// Action-bearing suggestions survive.
	for _, phrase := range []string{"looks good, commit it", "thanks, now run tests", "ok, ship it"} {
		s := `{"suggestions":[{"text":"` + phrase + `","prob":0.5}]}`
		if got := parseSuggestions(s); len(got) != 1 {
			t.Errorf("action phrase %q should survive, got %+v", phrase, got)
		}
	}
}

func TestForkTools_MatchesTheTurnsTransport(t *testing.T) {
	defs := []llm.ToolDef{{Type: "function"}}
	defs[0].Function.Name = "exec"

	tools, choice := forkTools(&agent.Session{Tools: defs})
	if len(tools) != 1 || choice != "none" {
		t.Errorf("native: got %d tools, choice %q; want the turn's tools and \"none\"", len(tools), choice)
	}
	tools, choice = forkTools(&agent.Session{Tools: defs, ToolFormat: agent.ToolFormatHeredoc})
	if tools != nil || choice != "" {
		t.Errorf("heredoc: got %d tools, choice %q; the turn sends none, so the fork must not", len(tools), choice)
	}
	tools, choice = forkTools(&agent.Session{})
	if tools != nil || choice != "" {
		t.Errorf("no tools: got %d tools, choice %q", len(tools), choice)
	}
}

// prefixRunner records every request so a test can compare one call's prefix
// with another's.
type prefixRunner struct {
	msgs  [][]llm.Message
	tools [][]llm.ToolDef
}

func (r *prefixRunner) ChatStream(_ context.Context, msgs []llm.Message, tools []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.msgs = append(r.msgs, append([]llm.Message(nil), msgs...))
	r.tools = append(r.tools, tools)
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: `{"suggestions":[{"text":"run the tests","prob":0.5}]}`}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

// The suggest call must begin with exactly what the turn sent — system prompt,
// history and tools — or the provider's prompt cache misses from token 0
// (measured live: 0 cached). This runs a real agent.Session.Turn, so it also
// fails if forkSystem stops matching the prompt Turn computes.
func TestSuggest_SharesTheTurnsPrefix(t *testing.T) {
	defs := []llm.ToolDef{{Type: "function"}}
	defs[0].Function.Name = "exec"
	for _, format := range []agent.ToolFormat{agent.ToolFormatNative, agent.ToolFormatHeredoc} {
		h := newNoteHarness(t)
		r := &prefixRunner{}
		h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: r,
			System: "You are dun.", Tools: defs, ToolFormat: format,
			Build: func(ctx context.Context, sid, system string) ([]llm.Message, error) {
				return agent.DefaultContextBuilder(ctx, h.store, sid, system)
			}}
		ctx := context.Background()
		_ = h.store.Append(ctx, "dun", agent.Entry{ID: "u1", Kind: agent.KindUser, Content: "what does exec do?"})
		if _, err := h.Session.Turn(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Suggestions(ctx); err != nil {
			t.Fatal(err)
		}
		if len(r.msgs) != 2 {
			t.Fatalf("%q: want the turn and one suggest call, got %d calls", format, len(r.msgs))
		}
		turn, fork := r.msgs[0], r.msgs[1]
		if len(fork) <= len(turn) || !reflect.DeepEqual(fork[:len(turn)], turn) {
			t.Errorf("%q: suggest does not start with the turn's messages\nturn: %+v\nfork: %+v", format, turn, fork)
		}
		if !reflect.DeepEqual(r.tools[1], r.tools[0]) {
			t.Errorf("%q: suggest sent %d tools, the turn sent %d", format, len(r.tools[1]), len(r.tools[0]))
		}
	}
}
