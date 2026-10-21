package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/agentkit/ragnotify"
)

// Proactive notifications — wiring raglit's search tool as an agent.DocFinder so
// dun can ping relevant docs before each turn (the ragnotify pattern, over an
// MCP server instead of a Go call). The finder reuses the SAME raglit server the
// model searches explicitly.
//
// Unlike agentkit's stock FinderPreparer (one notification PER hit), dun uses an
// AGGREGATING preparer: each pass emits ONE summary — "N found, M surfaced" —
// with the surfaced docs listed. "Surfaced" = the top-MaxHits actually injected
// into the prompt (what the model is given to read); "found" = all candidate
// hits after threshold/dedup. The UI collapses this to one line, expandable to
// the doc list (see the TUI's docsBlock).

// docsFinder returns a DocFinder over the discovered raglit `search` tool, or nil
// if raglit isn't in the tool set.
func docsFinder(mgr *mcpmgr.Manager, tools []mcpmgr.MCPTool) agent.DocFinder {
	for _, t := range tools {
		if t.Name == "search" {
			return ragnotify.MCPFinder(mgr, t.ServerID, "search", ragnotify.Opts{})
		}
	}
	return nil
}

// docsServerID is the server that owns the search tool docsFinder uses.
func docsServerID(tools []mcpmgr.MCPTool) string {
	for _, t := range tools {
		if t.Name == "search" {
			return t.ServerID
		}
	}
	return ""
}

// DocHitInfo is one surfaced document, for the UI.
type DocHitInfo struct {
	Title  string
	DocID  string
	Line   string
	Score  float64
	Inline bool // the whole document went into the prompt, not a pointer
}

// surfaceMargin is how far the top hit must beat the runner-up to surface.
// See the relevance note in docsPreparer.
const surfaceMargin = 1.5

// inlineMaxChars is the largest document surfaced whole. Memory notes are a
// few hundred characters (the M0 notes: 400–650); a source file is not.
const inlineMaxChars = 1500

// docBodyFunc reads a document's whole text when it is at most max chars;
// ok is false when it is longer, or could not be read.
type docBodyFunc func(ctx context.Context, docID string, max int) (text string, ok bool)

// docsBodyFetcher reads documents through raglit's get_document on the same
// server the finder searches. max_chars is asked one over the cap, so a
// document exactly at the cap is distinguishable from a truncated one.
func docsBodyFetcher(mgr *mcpmgr.Manager, serverID string) docBodyFunc {
	return func(ctx context.Context, docID string, max int) (string, bool) {
		raw, err := mgr.CallTool(ctx, serverID, "get_document", map[string]any{"path": docID, "max_chars": max + 1})
		if err != nil {
			return "", false
		}
		var doc struct {
			Text      string `json:"text"`
			Truncated bool   `json:"truncated"`
		}
		if json.Unmarshal([]byte(raw), &doc) != nil || doc.Truncated || doc.Text == "" || len(doc.Text) > max {
			return "", false
		}
		return doc.Text, true
	}
}

// DocsNote is the aggregated proactive-RAG summary for one pass.
type DocsNote struct {
	Found    int          // candidate hits after threshold/dedup
	Surfaced int          // top-MaxHits actually injected into the prompt
	Docs     []DocHitInfo // the surfaced docs
	Bytes    int          // size of the appended entry
	Tokens   int          // its estimated tokens — re-sent every round until folded
	LatMS    int64        // the finder's round-trip for this pass
}

// docsPreparer is dun's aggregating NotificationPreparer. It mirrors agentkit's
// FinderPreparer watermark/dedup/threshold logic, but instead of one notice per
// hit it appends ONE readable summary Entry (Tag "docs", so the store skips the
// plain onNotify — the UI is driven by onDocs with structure instead) and calls
// onDocs with the found/surfaced counts + surfaced docs. Every pass, hit or
// not, is recorded in stats (nil-safe); estimate sizes the appended entry;
// fetch (nil-safe) reads a short document whole so it can be inlined; group
// (nil-safe: one group) names the corpus a hit came from.
func docsPreparer(store *sessionStore, finder agent.DocFinder, opts agent.FinderOpts, stats *surfacingStats, estimate func(string) int, fetch docBodyFunc, group func(docID string) string, onDocs func(DocsNote)) agent.NotificationPreparer {
	kinds := opts.Kinds
	if len(kinds) == 0 {
		kinds = []agent.EntryKind{agent.KindUser, agent.KindAssistant}
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	observed := make(map[agent.EntryKind]bool, len(kinds))
	for _, k := range kinds {
		observed[k] = true
	}

	var mu sync.Mutex
	watermark := map[string]int64{}
	seen := map[string]map[string]bool{}

	return agent.PreparerFunc(func(ctx context.Context, sessionID string) error {
		entries, err := store.Context(ctx, sessionID)
		if err != nil {
			return err
		}

		mu.Lock()
		mark := watermark[sessionID]
		seenIDs := seen[sessionID]
		if seenIDs == nil {
			seenIDs = map[string]bool{}
			seen[sessionID] = seenIDs
		}
		mu.Unlock()

		var texts []string
		maxObserved := mark
		for _, e := range entries {
			if e.CreatedAt <= mark || !observed[e.Kind] {
				continue
			}
			if e.Content != "" {
				texts = append(texts, e.Content)
			}
			if e.CreatedAt > maxObserved {
				maxObserved = e.CreatedAt
			}
		}
		if len(texts) == 0 {
			return nil
		}

		fctx := ctx
		if timeout > 0 {
			var cancel context.CancelFunc
			fctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		findStart := time.Now()
		hits, err := finder.Find(fctx, texts)
		lat := time.Since(findStart)
		if err != nil {
			stats.notePass(lat, 0, 0, 0, 0)
			return nil // fail-open: keep the watermark so these entries retry
		}

		kept := hits[:0:0]
		for _, h := range hits {
			if h.Score >= opts.MinScore && h.DocID != "" && !seenIDs[h.DocID] {
				kept = append(kept, h)
			}
		}
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].Score > kept[j].Score })
		// One slot per DOCUMENT. Hits are per fragment, and a document with a
		// generated caption has two; measured in M0 (2026-10-20), one note took
		// both slots of a pass. Sorted first, so the best fragment is the one
		// kept.
		inPass := map[string]bool{}
		uniq := kept[:0]
		for _, h := range kept {
			if !inPass[h.DocID] {
				inPass[h.DocID] = true
				uniq = append(uniq, h)
			}
		}
		kept = uniq

		// Relevance: the TOP document only, and only when it clearly beats the
		// runner-up. Measured in M0 (2026-10-20): "any hit, two slots" made
		// 11 of 21 surfaced notes the wrong one and surfaced two on every
		// negative probe. BM25 scores have no fixed range — they move with the
		// corpus — so the bar is a RATIO to the runner-up, not a score:
		// top-1 with a 1.5× margin kept 14 of 19 right hits and 3 of 30 noise.
		// A group that fails the margin surfaces nothing and marks nothing
		// seen, so the same documents can win a later, clearer pass.
		//
		// The rule runs once per GROUP (memory notes, everything else), because
		// a score is only comparable within the index that produced it. Measured
		// live (M2, 2026-10-21): the stash note scored 8.5e-6 in the 1-note
		// memory index and suggest.go 2.3e-5 in the code index for "should I
		// stash before rebasing" — one ranking let the code file win every time.
		type win struct {
			h agent.DocHit
			n int // candidates in its group
		}
		var wins []win
		for _, g := range groupHits(kept, group) {
			if len(g) == 1 || g[0].Score >= surfaceMargin*g[1].Score {
				wins = append(wins, win{g[0], len(g)})
			}
		}
		if len(wins) > 0 {
			// Model-facing content. A short document is put in WHOLE: what the
			// pointer showed was its first line (a memory note's question), so
			// the model had to call search to read the answer — a round trip
			// to fetch fewer bytes than the round trip costs. A long one stays
			// a pointer; nothing large enters the prompt unasked.
			var b strings.Builder
			var infos []DocHitInfo
			for i, w := range wins {
				h := w.h
				if i > 0 {
					b.WriteString("\n\n")
				}
				body, inline := "", false
				if fetch != nil {
					body, inline = fetch(fctx, h.DocID, inlineMaxChars)
				}
				infos = append(infos, DocHitInfo{Title: h.Title, DocID: h.DocID, Line: h.Line, Score: h.Score, Inline: inline})
				if group != nil && group(h.DocID) == groupMemory {
					b.WriteString("A note an earlier session on this project saved may apply (it records what was true then):\n")
				}
				if inline {
					fmt.Fprintf(&b, "Possibly relevant (%d candidate document(s); this one clearly ranked first): %q (id=%s, score=%.2f)\n", w.n, h.Title, h.DocID, h.Score)
					b.WriteString(strings.TrimSpace(body))
				} else {
					fmt.Fprintf(&b, "%d possibly-relevant document(s); surfacing 1:\n", w.n)
					fmt.Fprintf(&b, "- %q (id=%s, score=%.2f) — %s\n", h.Title, h.DocID, h.Score, h.Line)
					b.WriteString("Call the search tool with an id to read a document if it helps.")
				}
				seenIDs[h.DocID] = true
			}
			content := b.String()
			if err := store.Append(ctx, sessionID, agent.Entry{
				ID: uuid.New().String(), Kind: agent.KindNotification, Tag: tagDocs,
				Content: content, CreatedAt: time.Now().UnixNano(),
			}); err != nil {
				return err
			}
			tok := 0
			if estimate != nil {
				tok = estimate(content)
			}
			stats.notePass(lat, len(kept), len(wins), len(content), tok)
			if onDocs != nil {
				onDocs(DocsNote{Found: len(kept), Surfaced: len(wins), Docs: infos,
					Bytes: len(content), Tokens: tok, LatMS: lat.Milliseconds()})
			}
		} else {
			stats.notePass(lat, len(kept), 0, 0, 0)
		}

		mu.Lock()
		if maxObserved > watermark[sessionID] {
			watermark[sessionID] = maxObserved
		}
		mu.Unlock()
		return nil
	})
}

// groupMemory is the group of hits that are memory notes.
const groupMemory = "memory"

// groupHits splits score-sorted hits by group, keeping each group sorted and
// the groups in order of first appearance. A nil group is one group.
func groupHits(hits []agent.DocHit, group func(string) string) [][]agent.DocHit {
	var order []string
	by := map[string][]agent.DocHit{}
	for _, h := range hits {
		g := ""
		if group != nil {
			g = group(h.DocID)
		}
		if _, ok := by[g]; !ok {
			order = append(order, g)
		}
		by[g] = append(by[g], h)
	}
	out := make([][]agent.DocHit, len(order))
	for i, g := range order {
		out[i] = by[g]
	}
	return out
}
