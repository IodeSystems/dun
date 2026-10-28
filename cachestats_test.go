package dun

import (
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

func msgs(contents ...string) []llm.Message {
	out := make([]llm.Message, len(contents))
	for i, c := range contents {
		out[i] = llm.Message{Role: "user", Content: c}
	}
	return out
}

// Each round is filed under the build before it: first, back-to-back append,
// append after a gap, and a rewrite — with the rewrite's cause.
func TestCacheStats_ClassifiesRounds(t *testing.T) {
	s := &cacheStats{}
	tools := []llm.ToolDef{{Type: "function"}}
	t0 := time.Unix(1000, 0)

	s.noteBuild(msgs("sys", "u1"), tools, t0)
	s.noteRound(agent.TokenUsage{Cached: 0, Processed: 100}, t0.Add(2*time.Second))

	// Append, sent 100ms after the response: back-to-back.
	s.noteBuild(msgs("sys", "u1", "a1"), tools, t0.Add(2100*time.Millisecond))
	s.noteRound(agent.TokenUsage{Cached: 95, Processed: 110}, t0.Add(3*time.Second))

	// Append, sent 30s later: after a gap.
	s.noteBuild(msgs("sys", "u1", "a1", "u2"), tools, t0.Add(33*time.Second))
	s.noteRound(agent.TokenUsage{Cached: 195, Processed: 150}, t0.Add(34*time.Second))

	// A fold rewrote message 1.
	s.noteFold()
	s.noteBuild(msgs("sys", "summary", "u2"), tools, t0.Add(34200*time.Millisecond))
	s.noteRound(agent.TokenUsage{Cached: 200, Processed: 250}, t0.Add(35*time.Second))

	// Tools changed (/rag on), messages append-only: still a rewrite.
	s.noteBuild(msgs("sys", "summary", "u2", "a2"), []llm.ToolDef{{Type: "function"}, {Type: "x"}}, t0.Add(35100*time.Millisecond))

	want := map[cacheClass]cacheClassStats{
		classFirst:      {Rounds: 1, Cached: 0, Processed: 100},
		classBackToBack: {Rounds: 1, Cached: 95, Processed: 10},
		classAfterGap:   {Rounds: 1, Cached: 100, Processed: 40},
		classRewrite:    {Rounds: 1, Cached: 5, Processed: 100},
	}
	for c, w := range want {
		if s.Class[c] != w {
			t.Errorf("%s: got %+v want %+v", cacheClassNames[c], s.Class[c], w)
		}
	}
	if s.Builds != 5 || s.AppendOnly != 2 || s.RewritesFold != 1 || s.RewritesTools != 1 || s.RewritesOther != 0 {
		t.Fatalf("build counts: %+v", s)
	}
	// The tools change kept every previous message (12 chars) and appended
	// "a2"; chars from the first changed message on: fold 9 ("summary"+"u2"),
	// tools 2 (the new "a2" — messages only, see cachestats.go).
	if s.LastKeptChars != 12 || s.LastChars != 14 {
		t.Errorf("tools-only rewrite: kept %d of %d", s.LastKeptChars, s.LastChars)
	}
	if s.ResentChars != 11 {
		t.Errorf("chars after the first change: %d", s.ResentChars)
	}
}

// A build with no round after it (a suggest build) does not double-file the
// next round; a tally reset does not go negative.
func TestCacheStats_PairingAndReset(t *testing.T) {
	s := &cacheStats{}
	now := time.Unix(1, 0)
	s.noteBuild(msgs("a"), nil, now)
	s.noteBuild(msgs("a"), nil, now) // e.g. suggest re-building the same prompt
	s.noteRound(agent.TokenUsage{Cached: 10, Processed: 5}, now)
	s.noteRound(agent.TokenUsage{Cached: 12, Processed: 6}, now) // no pending build
	total := 0
	for _, c := range s.Class {
		total += c.Rounds
	}
	if total != 1 {
		t.Fatalf("one round per build: %+v", s.Class)
	}
	s.noteBuild(msgs("a", "b"), nil, now)
	s.noteRound(agent.TokenUsage{Cached: 3, Processed: 2}, now) // fresh Session
	if c := s.Class[classBackToBack]; c.Cached != 3 || c.Processed != 2 {
		t.Fatalf("reset tally must not go negative: %+v", c)
	}
}

// Re-heading and recap are named causes, so "other" means unexplained.
func TestCacheStats_ReheadAndRecapCauses(t *testing.T) {
	s := &cacheStats{}
	now := time.Unix(1, 0)
	s.noteBuild(msgs("sys", "read1", "read2"), nil, now)
	s.noteRehead(1, 100)
	s.noteBuild(msgs("sys", "stub", "read2"), nil, now)
	s.noteRecap()
	s.noteBuild(msgs("sys", "summary"), nil, now)
	s.noteBuild(msgs("sys2", "summary"), nil, now) // e.g. a system-prompt change
	if s.RewritesRehead != 1 || s.RewritesRecap != 1 || s.RewritesOther != 1 || s.RewritesFold != 0 {
		t.Fatalf("causes: %+v", s)
	}
}
