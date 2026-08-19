package dun

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// rescue.go — the last rung of the context ladder: what happens when the
// Shaper's own estimate was wrong and the ENDPOINT refused the prompt.
//
// The Shaper shapes to BudgetTokens-headroom BEFORE every send (pristine tail →
// LOD stubs → compaction), so a refusal is not "we forgot to compact" — it is
// "the estimate undershot": CharsByFour counts bytes/4, and a payload of dense
// tokenization (a gradle log, a minified file, non-English text) can land the
// real prompt over the server's hard limit while the estimate says we fit. The
// Shaper cannot know that; only the endpoint knows its own tokenizer. So this
// layer treats a refusal as MEASUREMENT: it splits the history, summarizes the
// older half with a compaction prompt that is explicit about what must survive
// (open user asks, work state, and what would have been done had there been
// room), re-roots the session on the summary, and retries.
//
// "Compaction chaining" = repeat while it still does not fit. Each pass folds a
// contiguous older prefix into one marker, so the prompt shrinks monotonically;
// the cap below bounds the number of LLM calls this can spend.

// maxRescuePasses bounds how many split-and-fold passes ONE turn may spend.
// Two covers the realistic case (one pass halves the payload; a second folds
// what remains); more means the budget itself is broken and no folding helps.
const maxRescuePasses = 2

// rescueFoldChars caps how much of each older entry goes INTO a fold prompt.
// The point of a fold is to keep the decisions, not the bytes: tool results
// are the dominant mass in a coding session (measured: 122k of a 180k window),
// and a fold that re-sends them whole has just reproduced the overflow it was
// supposed to fix. The head is enough for the summarizer to know what each call
// did; the full text stays on disk in the session file either way.
const rescueFoldChars = 2000

// inputTooLarge reports whether err is an endpoint refusal of a prompt that did
// not fit — typed by llm.Client when the body says so (llama.cpp's physical
// batch, "exceeds the available context", …). Text-matched as a fallback for
// runners that are not *llm.Client.
func inputTooLarge(err error) bool {
	if err == nil {
		return false
	}
	var itl *llm.InputTooLargeError
	if errors.As(err, &itl) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "too large to process") ||
		strings.Contains(s, "exceeds the available context") ||
		strings.Contains(s, "exceed_context_size")
}

// runTurnWithRescue wraps runTurn: when a turn dies because the ENDPOINT said
// the prompt did not fit, it runs the rescue ladder and retries. Everything
// else (transient upstreams, model loops, user cancel) flows through runTurn's
// own handling untouched — this layer only acts on refusals, which are the one
// failure class where retrying WITHOUT changing the payload is guaranteed to
// fail identically.
func (h *Harness) runTurnWithRescue(ctx context.Context, turn func(context.Context) (agent.TurnResult, error)) (agent.TurnResult, error) {
	res, err := h.runTurn(ctx, turn)
	if !inputTooLarge(err) {
		return res, err
	}
	log.Printf("dun: endpoint refused the prompt (%v) — starting rescue compaction", err)
	for pass := 1; pass <= maxRescuePasses; pass++ {
		n, ferr := h.rescueFold(ctx)
		if ferr != nil {
			return res, fmt.Errorf("rescue fold failed: %w (original: %v)", ferr, err)
		}
		if n == 0 {
			// Nothing left to fold: the history is already down to its pristine
			// tail. The prompt still does not fit, which means the tail itself —
			// or the system prompt — exceeds the endpoint. No folding fixes that;
			// say so instead of looping on a failure no compaction can reach.
			return res, fmt.Errorf("prompt does not fit this endpoint even after "+
				"compacting all foldable history: %v", err)
		}
		h.retry(RetryNote{Scope: "turn", Kind: "interrupted", Attempt: pass,
			Reason: fmt.Sprintf("rescue pass %d — folded %d entries into a summary; retrying with the re-rooted session", pass, n)})
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res, err = h.runTurn(ctx, turn)
		if !inputTooLarge(err) {
			return res, err
		}
	}
	return res, fmt.Errorf("prompt still does not fit after %d rescue passes: %v", maxRescuePasses, err)
}

// rescueFold splits the session's entries into an older prefix and a newer tail,
// summarizes the prefix with a prompt that names what MUST survive (open user
// asks, work state, and the work that would have been done had there been room),
// and re-roots the session: the summary replaces the folded prefix as one
// compaction marker at the front of the history. Returns how many entries were
// folded; 0 means nothing was foldable (only the tail remains).
//
// The split keeps the recent conversation intact — the model is mid-task, and
// its last few exchanges are what it needs to continue — while the OLDER half,
// which has already been acted on, is what can become a paragraph.
func (h *Harness) rescueFold(ctx context.Context) (int, error) {
	entries, err := h.store.Context(ctx, "dun")
	if err != nil {
		return 0, fmt.Errorf("load history: %w", err)
	}
	sortByTime(entries)

	// Split at the middle of what is NOT already a marker. Markers are
	// excluded from both halves' fold set below; splitting on all entries would
	// let an early marker eat half the budget and leave nothing to fold.
	foldable := 0
	for _, e := range entries {
		if e.Kind != agent.KindCompaction && e.Kind != kindRecap {
			foldable++
		}
	}
	if foldable <= 2 {
		return 0, nil // nothing to fold: the session is already just its tail
	}
	half := foldable / 2

	var older []agent.Entry
	taken := 0
	for _, e := range entries {
		if e.Kind == agent.KindCompaction || e.Kind == kindRecap {
			continue
		}
		if taken < half {
			older = append(older, e)
			taken++
		} else {
			break // the rest of the history is the tail; stop walking
		}
	}
	if len(older) == 0 {
		return 0, nil
	}

	summary, err := h.summarizeRescue(ctx, older)
	if err != nil {
		return 0, fmt.Errorf("summarize: %w", err)
	}

	// Re-root: drop the folded entries, keep everything else in order, and put
	// the marker at the FRONT (CreatedAt 1 — before anything in the session),
	// so it reads as "this is what happened earlier" and the live tail follows
	// it chronologically. The store's Compact appends the marker; a front
	// position needs the drop+insert done here, on the same primitive recap uses.
	if err := h.store.reRoot(ctx, older, agent.Entry{
		ID:        uuid.New().String(),
		Kind:      agent.KindCompaction,
		Content:   summary,
		CreatedAt: 1,
	}); err != nil {
		return 0, fmt.Errorf("re-root: %w", err)
	}
	log.Printf("dun: rescue fold — %d entries → one summary marker (%d chars)", len(older), len(summary))
	h.noteCompaction(agent.CompactionInfo{Summary: summary, SubsumedCount: len(older)})
	return len(older), nil
}

// summarizeRescue asks the model to compress an older half of the session. The
// prompt is deliberately a STATE HANDOFF, not a transcript digest: a compaction
// that loses what the user is still waiting for (open asks) or where the work
// actually stands (what is done, and what WOULD have been done next had there
// been room) produces a session that confidently goes in the wrong direction.
func (h *Harness) summarizeRescue(ctx context.Context, older []agent.Entry) (string, error) {
	var b strings.Builder
	b.WriteString("The conversation below is being compacted because it no longer fits this endpoint's context window. " +
		"Write a state handoff in <= 800 tokens that lets the agent CONTINUE without re-reading it. It must contain:\n" +
		"1. OPEN ASKS — every user request that has not yet been fully satisfied, verbatim where short.\n" +
		"2. STATE OF WORK — what is done and verified (files changed, commands run, tests passing/failing), with the specifics: paths, names, values.\n" +
		"3. NEXT STEPS — the work that would have been done next had there been enough context: the concrete plan, in order.\n" +
		"4. DECISIONS & CONSTRAINTS — choices made and why, plus anything the user forbade or required.\n" +
		"Drop tool-call payloads; keep only what they established. Output the handoff only — no preamble.\n\n")
	for i, e := range older {
		label := string(e.Kind)
		if e.Tag != "" {
			label = e.Tag
		}
		content := e.Content
		if len(content) > rescueFoldChars {
			content = content[:rescueFoldChars] + " …[truncated]"
		}
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, label, content)
	}

	ch, err := h.client.ChatStream(ctx, []llm.Message{
		{Role: "system", Content: "You are a compaction worker. Produce the state handoff only."},
		{Role: "user", Content: b.String()},
	}, nil, nil)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for chunk := range ch {
		if chunk.Error != "" {
			return "", fmt.Errorf("%s", chunk.Error)
		}
		out.WriteString(chunk.Content)
		if chunk.Done {
			go func() {
				for range ch {
				}
			}()
			break
		}
	}
	s := strings.TrimSpace(out.String())
	if s == "" {
		return "", fmt.Errorf("model returned empty summary")
	}
	return s, nil
}

// sortByTime orders entries the way the engine renders them: CreatedAt, then ID.
func sortByTime(entries []agent.Entry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0; j-- {
			a, b := entries[j-1], entries[j]
			if a.CreatedAt == b.CreatedAt {
				if a.ID <= b.ID {
					break
				}
			} else if a.CreatedAt < b.CreatedAt {
				break
			}
			entries[j-1], entries[j] = b, a
		}
	}
}
