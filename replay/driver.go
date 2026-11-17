package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	agent "github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	shim "github.com/iodesystems/openai-claude-api-shim"
)

// RoundReport is what the replay reports for one model round.
type RoundReport struct {
	// Round is the 1-based round number.
	Round int
	// PromptTokens is the estimated token size of the built prompt, measured
	// by the estimator over the messages Shaper.Build returned.
	PromptTokens int
	// PrefixKept is the number of leading messages in this round's built prompt
	// that are byte-identical to the previous round's built prompt. Round 1
	// has no previous round, so PrefixKept is 0.
	PrefixKept int
	// Shrink, when non-nil, records a compaction that happened during this
	// round's Build: the tokens before and after the fold.
	Shrink *ShrinkEvent
	// Cost is the USD cost of this round at the given prices, using the
	// provider's token counts from the request event.
	Cost float64
	// Model is the model id the cost was computed for.
	Model string
}

// ShrinkEvent records one compaction fold.
type ShrinkEvent struct {
	TokensBefore int
	TokensAfter  int
	Subsumed     int
	Summary      string
}

// Config controls how the replay runs.
type Config struct {
	// Policy is the shaping policy to drive the Shaper with.
	Policy agent.ShaperPolicy
	// System is the system prompt passed to Build.
	System string
	// Estimator is the token estimator; nil uses agentkit's default (CharsByThree).
	Estimator agent.TokenEstimator
	// Prices is the price table for cost reporting. Nil means no cost reported.
	Prices shim.Prices
	// Model is the model id for cost lookup.
	Model string
}

// Run drives the Shaper over the reconstructed session history, one round at a
// time, and returns one RoundReport per round.
//
// entries is the full reconstructed history (from Load). rounds carries the
// round boundaries: rounds[i].EntryCount is how many entries were in the store
// when round i+1's Build was called. The driver appends entries incrementally
// up to each boundary, calls Shaper.Build, and measures the result.
//
// The Shaper is driven with a fake zero-network LLMRunner: fold calls return a
// fixed summary string, so compaction is deterministic and no model is called.
func Run(ctx context.Context, entries []agent.Entry, rounds []RoundMeta, cfg Config) []RoundReport {
	store := newMemStore()
	shaper := &agent.Shaper{
		Store:    store,
		Runner:   fakeRunner{},
		Estimate: cfg.Estimator,
		Policy:   cfg.Policy,
	}

	// Count compaction markers in the initial entries so we can detect new
	// ones the Shaper writes during Build.
	initialMarkers := countMarkers(entries)

	var (
		reports       []RoundReport
		prevMsgHashes []string
		prevEntryIdx  int
	)

	for i, rm := range rounds {
		// Append entries up to this round's boundary.
		for prevEntryIdx < rm.EntryCount {
			store.entries = append(store.entries, entries[prevEntryIdx])
			prevEntryIdx++
		}

		// Build the prompt for this round.
		msgs, err := shaper.Build(ctx, "replay", cfg.System)
		if err != nil {
			// A Build failure (e.g. compaction did not converge) ends the
			// replay for this round; record what we have.
			break
		}

		// Prompt size: sum the estimator over the built messages.
		promptTokens := 0
		if shaper.Estimate != nil {
			for _, m := range msgs {
				promptTokens += messageTokens(shaper.Estimate, m)
			}
		}

		// Prefix kept: longest common message prefix vs the previous round.
		msgHashes := hashMessages(msgs)
		prefixKept := 0
		if len(prevMsgHashes) > 0 {
			max := len(prevMsgHashes)
			if len(msgHashes) < max {
				max = len(msgHashes)
			}
			for j := 0; j < max; j++ {
				if prevMsgHashes[j] != msgHashes[j] {
					break
				}
				prefixKept = j + 1
			}
		}
		prevMsgHashes = msgHashes

		// Shrink events: detect new compaction markers the Shaper wrote.
		var shrink *ShrinkEvent
		nowMarkers := countMarkers(store.entries)
		if nowMarkers > initialMarkers {
			// Find the newest marker for the summary text.
			var lastMarker agent.Entry
			for j := len(store.entries) - 1; j >= 0; j-- {
				if store.entries[j].Kind == agent.KindCompaction {
					lastMarker = store.entries[j]
					break
				}
			}
			shrink = &ShrinkEvent{
				Subsumed: nowMarkers - initialMarkers,
				Summary:  lastMarker.Content,
			}
			initialMarkers = nowMarkers
		}

		// Cost at ledger prices, using the provider's token counts.
		var cost float64
		if cfg.Prices != nil {
			cost = CostFor(cfg.Prices, cfg.Model, rm.PromptTokens, rm.CachedTokens, rm.GenTokens)
		}

		reports = append(reports, RoundReport{
			Round:        i + 1,
			PromptTokens: promptTokens,
			PrefixKept:   prefixKept,
			Shrink:       shrink,
			Cost:         cost,
			Model:        cfg.Model,
		})
	}

	return reports
}

// messageTokens estimates the token cost of one built message, matching
// agentkit's internal messageTokens (which is unexported).
func messageTokens(est agent.TokenEstimator, m llm.Message) int {
	t := est.Estimate(m.Content)
	for _, tc := range m.ToolCalls {
		t += est.Estimate(tc.Function.Name) + est.Estimate(tc.Function.Arguments)
	}
	return t
}

// hashMessages returns a SHA-256 hash for each message, for prefix comparison.
func hashMessages(msgs []llm.Message) []string {
	hashes := make([]string, len(msgs))
	for i, m := range msgs {
		b, _ := json.Marshal(m)
		h := sha256.Sum256(b)
		hashes[i] = hex.EncodeToString(h[:])
	}
	return hashes
}

// countMarkers counts KindCompaction entries in a slice.
func countMarkers(entries []agent.Entry) int {
	n := 0
	for _, e := range entries {
		if e.Kind == agent.KindCompaction {
			n++
		}
	}
	return n
}

// --- in-memory Store ---

// memStore is a slice-backed agent.Store for the replay. It implements the
// Store interface so the Shaper can read and compact history in memory.
type memStore struct {
	entries []agent.Entry
}

func newMemStore() *memStore {
	return &memStore{}
}

// ClaimPending is not used by the Shaper; the replay has no pending claims.
func (s *memStore) ClaimPending(_ context.Context, _ string, _ int64) (int, error) {
	return 0, nil
}

// Append adds an entry to the store.
func (s *memStore) Append(_ context.Context, _ string, e agent.Entry) error {
	s.entries = append(s.entries, e)
	return nil
}

// Context returns the current entries in CreatedAt order.
func (s *memStore) Context(_ context.Context, _ string) ([]agent.Entry, error) {
	// Return a copy so the Shaper's sort does not mutate the store.
	out := make([]agent.Entry, len(s.entries))
	copy(out, s.entries)
	return out, nil
}

// Compact replaces the subsumed entries with the marker, in one step.
// The marker's CreatedAt determines its position: it is inserted at the
// position of the first subsumed entry, and all subsumed entries are removed.
func (s *memStore) Compact(_ context.Context, _ string, c agent.Compaction) error {
	// Build a set of subsumed entry indices (by CreatedAt, which is unique
	// in the replay because we assign them sequentially).
	subsumed := make(map[int64]bool, len(c.Subsumes))
	for _, e := range c.Subsumes {
		subsumed[e.CreatedAt] = true
	}

	// Walk the store, keeping non-subsumed entries, and insert the marker
	// at the position of the first subsumed entry.
	var out []agent.Entry
	markerPlaced := false
	for _, e := range s.entries {
		if subsumed[e.CreatedAt] {
			if !markerPlaced {
				out = append(out, c.Marker)
				markerPlaced = true
			}
			continue
		}
		out = append(out, e)
	}
	if !markerPlaced {
		// No subsumed entries found (should not happen); append the marker.
		out = append(out, c.Marker)
	}
	s.entries = out
	return nil
}

// --- fake LLMRunner ---

// fakeRunner is a zero-network LLMRunner for the replay. Fold calls return a
// fixed summary string, so compaction is deterministic and no model is called.
type fakeRunner struct{}

func (fakeRunner) ChatStream(_ context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: "[replay fold summary]"}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}
