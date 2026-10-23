package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// Proactive notifications — wiring a turn.before extension's search tool
// (raglit's) as an agent.DocFinder so dun can ping relevant docs before each
// turn (the ragnotify pattern, over an MCP server instead of a Go call; see
// newSurfacer in extension.go). The finder reuses the SAME server the model
// searches explicitly.
//
// Unlike agentkit's stock FinderPreparer (one notification PER hit), dun uses an
// AGGREGATING preparer: each pass emits ONE summary — "N found, M surfaced" —
// with the surfaced docs listed. "Surfaced" = the top-MaxHits actually injected
// into the prompt (what the model is given to read); "found" = all candidate
// hits after threshold/dedup. The UI collapses this to one line, expandable to
// the doc list (see the TUI's docsBlock).

// DocHitInfo is one surfaced document, for the UI.
type DocHitInfo struct {
	Title  string
	DocID  string
	Line   string
	Score  float64
	Inline bool // the whole document went into the prompt, not a pointer

	// Where it came from, so a person can see what feeds the agent.
	Kind  string // skill, memory, rule, mcp-doc, doc or attached doc
	Path  string // the file: ~ for $HOME, relative inside the workspace
	Index string // the raglit index of the hit, "" when the server names none
	Root  string // a skill's root: where it was loaded from
	// Why it won: the candidates in its group (it counted) and the
	// runner-up's score, 0 when it was the only candidate.
	Candidates int
	RunnerUp   float64
}

// Why says why a surfaced document won: it was the only candidate in its
// group (no margin was tested, so it may be weak), or how far it beat the
// runner-up. The raw score is %.2g: BM25 scores of a small index are in the
// 1e-5 range, where %.2f prints 0.00.
func (d DocHitInfo) Why() string {
	switch {
	case d.Candidates == 1:
		return fmt.Sprintf("only candidate · score %.2g", d.Score)
	case d.RunnerUp > 0:
		return fmt.Sprintf("%.1f× the runner-up (of %d) · score %.2g", d.Score/d.RunnerUp, d.Candidates, d.Score)
	}
	return fmt.Sprintf("score %.2g", d.Score)
}

// Source is the origin of a surfaced document on one line: kind, file,
// index and, for a skill, the root it was loaded from.
func (d DocHitInfo) Source() string {
	var parts []string
	for _, p := range []string{d.Kind, d.Path} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if d.Index != "" {
		parts = append(parts, "index "+d.Index)
	}
	if d.Root != "" {
		parts = append(parts, d.Root)
	}
	return strings.Join(parts, " · ")
}

// hitSources tells docsPreparer where a hit came from. Every field is
// nil-safe.
type hitSources struct {
	index    func(docID string) string // the raglit index of a hit
	attached func(index string) bool   // an index attached from elsewhere (attach.go)
	display  func(path string) string  // a path as a person reads it
}

func (s *hitSources) indexOf(docID string) string {
	if s == nil || s.index == nil {
		return ""
	}
	return s.index(docID)
}

func (s *hitSources) isAttached(index string) bool {
	return s != nil && s.attached != nil && index != "" && s.attached(index)
}

func (s *hitSources) show(p string) string {
	if s == nil || s.display == nil {
		return p
	}
	return s.display(p)
}

// displayPath is p as a person reads it: relative to the first of roots
// that holds it, else with ~ for home.
func displayPath(p, home string, roots ...string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	for _, r := range roots {
		if r == "" {
			continue
		}
		if rel, err := filepath.Rel(r, p); err == nil && inside(rel) {
			return rel
		}
	}
	if home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && inside(rel) {
			return "~/" + rel
		}
	}
	return p
}

// inside reports whether a filepath.Rel result is below its base.
func inside(rel string) bool {
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// docKind is the kind of a hit in group g (see docGroup).
func docKind(g string, attached bool) string {
	switch g {
	case groupSkill:
		return "skill"
	case groupMemory:
		return "memory"
	case groupRules:
		return "rule"
	case groupMCPDocs:
		return "mcp-doc"
	}
	if attached {
		return "attached doc"
	}
	return "doc"
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

// docsBodyFetcher reads documents through the search role's get tool
// (raglit's get_document) on the same server the finder searches. max_chars
// is asked one over the cap, so a document exactly at the cap is
// distinguishable from a truncated one. index, when set, is the attached
// indexes' selector (attach.go), so a hit from one of them can be read.
func docsBodyFetcher(mgr *mcpmgr.Manager, serverID, tool, index string) docBodyFunc {
	return func(ctx context.Context, docID string, max int) (string, bool) {
		args := map[string]any{"path": docID, "max_chars": max + 1}
		if index != "" {
			args["index"] = index
		}
		raw, err := mgr.CallTool(ctx, serverID, tool, args)
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
// (nil-safe: one group) names the corpus a hit came from; skill (nil-safe)
// names the skill a hit is the SKILL.md of; src (nil-safe) names the index
// a hit came from and shows its path.
func docsPreparer(store *sessionStore, finder agent.DocFinder, opts agent.FinderOpts, stats *surfacingStats, estimate func(string) int, fetch docBodyFunc, group func(docID string) string, skill func(docID string) (skillHit, bool), src *hitSources, onDocs func(DocsNote)) agent.NotificationPreparer {
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
		// And one per SKILL: a skill's SKILL.md and its reference files are
		// one candidate. Measured live (2026-10-22): two files of one skill
		// read as "2 candidate skills, this one clearly ranked first".
		skillSeen := map[string]bool{}
		uniq = kept[:0]
		for _, h := range kept {
			if sk, ok := lookupSkill(skill, h.DocID); ok {
				if skillSeen[sk.Name] {
					continue
				}
				skillSeen[sk.Name] = true
			}
			uniq = append(uniq, h)
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
			h        agent.DocHit
			n        int     // candidates in its group
			runnerUp float64 // the second score, 0 when alone
		}
		var wins []win
		for _, g := range groupHits(kept, group) {
			// A lone candidate passes with no test at all: there is nothing
			// to beat, however weak the match. It is marked "only candidate"
			// in what the model and the person read. A floor for it waits on
			// plan item "surfacing floor" (embedding scores from raglit).
			if len(g) == 1 {
				wins = append(wins, win{g[0], 1, 0})
			} else if g[0].Score >= surfaceMargin*g[1].Score {
				wins = append(wins, win{g[0], len(g), g[1].Score})
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
				seenIDs[h.DocID] = true
				g := ""
				if group != nil {
					g = group(h.DocID)
				}
				index := src.indexOf(h.DocID)
				info := DocHitInfo{Title: h.Title, DocID: h.DocID, Line: h.Line, Score: h.Score,
					Path: src.show(h.DocID), Index: index, Candidates: w.n, RunnerUp: w.runnerUp}
				if sk, ok := lookupSkill(skill, h.DocID); ok {
					// A skill, not a document: name it and say how to load it.
					// A quoted fragment of a SKILL.md reads as advice, and the
					// model acts on the fragment without loading the skill or
					// the files it names.
					info.Title, info.Line, info.Kind, info.Root = "skill "+sk.Name, sk.Description, "skill", sk.Root
					infos = append(infos, info)
					fmt.Fprintf(&b, "A skill matches what you are working on (%s): %q — %s\n", info.Why(), sk.Name, sk.Description)
					fmt.Fprintf(&b, "Source: %s\n", info.Source())
					fmt.Fprintf(&b, "If it applies, call skill name=%q and follow it before you act.", sk.Name)
					if sk.File != "" {
						// The match was in a file the skill points at: say
						// which, so the model can read it after the skill.
						fmt.Fprintf(&b, " The match is in its file %q (skill name=%q file=%q).", sk.File, sk.Name, sk.File)
					}
					continue
				}
				body, inline := "", false
				if fetch != nil {
					body, inline = fetch(fctx, h.DocID, inlineMaxChars)
				}
				info.Inline, info.Kind = inline, docKind(g, src.isAttached(index))
				infos = append(infos, info)
				switch g {
				case groupMemory:
					// The cue is here because this is the one moment the model
					// holds the evidence: it is working in what the note is about.
					b.WriteString("A note an earlier session on this project saved may apply. It records what was true " +
						"then; if the user or the code now says otherwise, fix it — memory action=refine (or void if it " +
						"no longer applies) with why and source:\n")
				case groupMCPDocs:
					b.WriteString("From the documentation of an MCP server you have:\n")
				case groupRules:
					b.WriteString("From a file the project's rules import (they are searchable, not in your instructions; /rules index):\n")
				}
				if inline {
					fmt.Fprintf(&b, "Possibly relevant (%s): %q (id=%s)\n", info.Why(), h.Title, h.DocID)
					fmt.Fprintf(&b, "Source: %s\n", info.Source())
					b.WriteString(strings.TrimSpace(body))
				} else {
					fmt.Fprintf(&b, "%d possibly-relevant document(s); surfacing 1 (%s):\n", w.n, info.Why())
					fmt.Fprintf(&b, "- %q (id=%s) — %s\n", h.Title, h.DocID, h.Line)
					fmt.Fprintf(&b, "Source: %s\n", info.Source())
					b.WriteString("Call the search tool with an id to read a document if it helps.")
				}
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

// groupSkill is the group of hits that are skills' SKILL.md and reference
// files.
const groupSkill = "skill"

// groupRules is the group of hits that are imported rules files in the
// rules index (rules.go).
const groupRules = "rules"

func lookupSkill(skill func(string) (skillHit, bool), docID string) (skillHit, bool) {
	if skill == nil {
		return skillHit{}, false
	}
	return skill(docID)
}

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
