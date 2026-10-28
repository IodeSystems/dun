package dun

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Cache and reshaping accounting — what the prompt cache actually did, and
// how much of each miss was dun's own doing.
//
// Context sizes were being tuned blind. The provider reports cached vs
// processed tokens, but a session-level total says nothing about WHY a round
// re-read its prompt. There are two different reasons, and they need opposite
// fixes:
//
//   - dun rewrote the prefix: a fold, an LOD step, a tool-set change
//     (/rag on), a re-heading. The server HAD to re-read from the first
//     changed message. The fix is in dun's shaping policy.
//   - dun only appended, and the server re-read anyway: its KV slot was
//     taken by other traffic in between (a shared llama-server). The fix is
//     on the serving side — or in sending the next request sooner.
//
// So each BUILD is compared with the previous one: how many leading messages
// are byte-identical (kept), and whether everything before the new tail
// survived (append-only) or something earlier changed (rewrite, with the
// cause when dun knows it: fold, re-heading, the model's recap, a tool-set
// change; "other" is what is left — an LOD step, a system-prompt change).
// Each ROUND's usage delta is then filed under the
// build that preceded it, by class:
//
//   - first:          no previous build — nothing could be cached
//   - rewrite:        the build rewrote the prefix
//   - back-to-back:   append-only, sent < backToBack after the last response
//   - after a gap:    append-only, sent later (the user was typing, a tool ran)
//
// aw4 measured the split this is after (aw4 internal/agent/modelgate.go,
// a06d5cde): back-to-back rounds hit 95%, interleaved ones 72%, and 68% of a
// session's next requests left within 0.1s of the previous response.
//
// Tools are compared by hash and reported as a rewrite cause, but the kept
// prefix is measured over MESSAGES only: where the tools land in the rendered
// prompt is the chat template's business (llama.cpp renders them before the
// system text, which would make any tool change a full re-read — that is the
// claim the "tools" rewrite count and its hit rate let us check).

// backToBack is the gap under which an append-only round counts as
// back-to-back. aw4's gap distribution: 78.8% under 1s, and fewer than 2% of
// gaps end past ~0.75s, so 1s separates "the loop sent it" from "something
// waited".
const backToBack = time.Second

type cacheClass int

const (
	classFirst cacheClass = iota
	classRewrite
	classBackToBack
	classAfterGap
	numClasses
)

var cacheClassNames = [numClasses]string{"first", "rewrite", "back_to_back", "after_gap"}

type cacheClassStats struct {
	Rounds    int
	Cached    int
	Processed int
}

type cacheStats struct {
	mu sync.Mutex

	// Build-to-build comparison state.
	prevSigs     [][32]byte
	prevChars    []int // per-message content length, aligned with prevSigs
	prevTools    [32]byte
	haveTools    bool
	foldMarked   bool // a fold happened since the last build
	reheadMarked bool // a re-heading changed the context since the last build
	recapMarked  bool // the model's recap replaced a span since the last build

	// Round pairing: the class of the last build, waiting for its usage.
	pending     cacheClass
	havePending bool
	lastResp    time.Time

	// Cumulative usage at the last round, for deltas.
	prevCached, prevProcessed int

	Builds         int
	AppendOnly     int
	RewritesFold   int
	RewritesTools  int
	RewritesRehead int
	RewritesRecap  int
	RewritesOther  int
	ResentChars    int // chars of the build from the first changed message on, summed over rewrites
	LastKeptChars  int
	LastChars      int
	Reheads        int // re-headings that changed the context (rehead.go)
	ReheadEntries  int // superseded reads stubbed, summed
	ReheadChars    int // chars those stubs removed, summed

	Class [numClasses]cacheClassStats
}

func msgSig(m llm.Message) [32]byte {
	b, _ := json.Marshal(m)
	return sha256.Sum256(b)
}

// noteRehead records one re-heading: n stubbed reads, chars removed.
func (s *cacheStats) noteRehead(n, chars int) {
	if s == nil || n == 0 {
		return
	}
	s.mu.Lock()
	s.Reheads++
	s.reheadMarked = true
	s.ReheadEntries += n
	s.ReheadChars += chars
	s.mu.Unlock()
}

// noteRecap marks that the model's recap replaced a span, so the next
// build's rewrite is attributed to it.
func (s *cacheStats) noteRecap() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.recapMarked = true
	s.mu.Unlock()
}

// noteFold marks that the next build follows a fold, so its rewrite is
// attributed to the fold.
func (s *cacheStats) noteFold() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.foldMarked = true
	s.mu.Unlock()
}

// noteBuild compares this build with the previous one and records the class
// the next round's usage is filed under.
func (s *cacheStats) noteBuild(msgs []llm.Message, tools []llm.ToolDef, now time.Time) {
	if s == nil {
		return
	}
	sigs := make([][32]byte, len(msgs))
	chars := make([]int, len(msgs))
	total := 0
	for i, m := range msgs {
		sigs[i] = msgSig(m)
		chars[i] = len(m.Content)
		total += chars[i]
	}
	tb, _ := json.Marshal(tools)
	th := sha256.Sum256(tb)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.Builds++
	class := classFirst
	if s.prevSigs != nil {
		keep := 0
		for keep < len(sigs) && keep < len(s.prevSigs) && sigs[keep] == s.prevSigs[keep] {
			keep++
		}
		kept := 0
		for i := 0; i < keep; i++ {
			kept += chars[i]
		}
		s.LastKeptChars, s.LastChars = kept, total
		toolsChanged := s.haveTools && th != s.prevTools
		// Append-only: every previous message survived in place. (The last
		// previous message may legitimately be the one the new tail follows;
		// anything shorter than the whole previous list is a rewrite.)
		if keep == len(s.prevSigs) && !toolsChanged {
			s.AppendOnly++
			class = classAfterGap
			if !s.lastResp.IsZero() && now.Sub(s.lastResp) < backToBack {
				class = classBackToBack
			}
		} else {
			class = classRewrite
			s.ResentChars += total - kept
			switch {
			case s.foldMarked:
				s.RewritesFold++
			case s.reheadMarked:
				s.RewritesRehead++
			case s.recapMarked:
				s.RewritesRecap++
			case toolsChanged:
				s.RewritesTools++
			default:
				s.RewritesOther++
			}
		}
	} else {
		s.LastKeptChars, s.LastChars = 0, total
	}
	s.prevSigs, s.prevChars = sigs, chars
	s.prevTools, s.haveTools = th, true
	s.foldMarked, s.reheadMarked, s.recapMarked = false, false, false
	s.pending, s.havePending = class, true
}

// noteRound files one round's usage delta under the class of the build that
// preceded it. u is the Session's cumulative tally.
func (s *cacheStats) noteRound(u agent.TokenUsage, now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dc, dp := u.Cached-s.prevCached, u.Processed-s.prevProcessed
	if dc < 0 || dp < 0 { // the tally was reset (a fresh Session)
		dc, dp = u.Cached, u.Processed
	}
	s.prevCached, s.prevProcessed = u.Cached, u.Processed
	s.lastResp = now
	if !s.havePending {
		return
	}
	c := &s.Class[s.pending]
	c.Rounds++
	c.Cached += dc
	c.Processed += dp
	s.havePending = false
}

func hitPct(cached, processed int) float64 {
	if cached+processed == 0 {
		return 0
	}
	return 100 * float64(cached) / float64(cached+processed)
}

// summary is the JSON shape for the usage event; nil before the first build.
func (s *cacheStats) summary() map[string]any {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Builds == 0 {
		return nil
	}
	classes := map[string]any{}
	var tc, tp, tr int
	for i, c := range s.Class {
		tc += c.Cached
		tp += c.Processed
		tr += c.Rounds
		if c.Rounds == 0 {
			continue
		}
		classes[cacheClassNames[i]] = map[string]any{
			"rounds": c.Rounds, "cached": c.Cached, "processed": c.Processed,
			"hit_pct": hitPct(c.Cached, c.Processed),
		}
	}
	return map[string]any{
		"rounds": tr, "cached": tc, "processed": tp, "hit_pct": hitPct(tc, tp),
		"classes": classes,
		"builds":  s.Builds, "append_only": s.AppendOnly,
		"rewrites_fold": s.RewritesFold, "rewrites_tools": s.RewritesTools,
		"rewrites_rehead": s.RewritesRehead, "rewrites_recap": s.RewritesRecap,
		"rewrites_other": s.RewritesOther, "resent_chars": s.ResentChars,
		"last_kept_chars": s.LastKeptChars, "last_chars": s.LastChars,
		"reheads": s.Reheads, "rehead_entries": s.ReheadEntries, "rehead_chars": s.ReheadChars,
	}
}

// CacheStats exposes the totals, JSON-serializable for the usage event.
func (h *Harness) CacheStats() map[string]any { return h.cache.summary() }
