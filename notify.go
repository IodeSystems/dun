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
	// What the rule read (raglit e66a3df on; all 0 from an older one): the
	// hit's cosine (0: the vector side did not rank it), Matched of
	// QueryTerms query words it holds, and the runner-up's cosine.
	Similarity  float64
	Matched     int
	QueryTerms  int
	RunnerUpSim float64
}

// Why says why a surfaced document won: it was the only candidate in its
// group (no margin was tested, only the floor), or how far it beat the
// runner-up; with its similarity and the query words it holds. A hit from
// an older raglit has neither, and says how far its score beat the
// runner-up's; that raw score is %.2g: BM25 scores of a small index are in
// the 1e-5 range, where %.2f prints 0.00.
func (d DocHitInfo) Why() string {
	if d.Similarity == 0 && d.QueryTerms == 0 {
		switch {
		case d.Candidates == 1:
			return fmt.Sprintf("only candidate · score %.2g", d.Score)
		case d.RunnerUp > 0:
			return fmt.Sprintf("%.1f× the runner-up (of %d) · score %.2g", d.Score/d.RunnerUp, d.Candidates, d.Score)
		}
		return fmt.Sprintf("score %.2g", d.Score)
	}
	var parts []string
	if d.Candidates == 1 {
		parts = append(parts, "only candidate")
	}
	if d.Similarity > 0 {
		parts = append(parts, fmt.Sprintf("sim %.2f", d.Similarity))
	}
	if d.QueryTerms > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d terms", d.Matched, d.QueryTerms))
	}
	if d.Candidates > 1 {
		switch {
		case d.Similarity > 0 && d.RunnerUpSim > 0:
			parts = append(parts, fmt.Sprintf("+%.2f over runner-up (of %d)", d.Similarity-d.RunnerUpSim, d.Candidates))
		case d.Similarity > 0:
			parts = append(parts, fmt.Sprintf("runner-up has no sim (of %d)", d.Candidates))
		case d.RunnerUp > 0:
			parts = append(parts, fmt.Sprintf("%.1f× the runner-up (of %d)", d.Score/d.RunnerUp, d.Candidates))
		default:
			parts = append(parts, fmt.Sprintf("top of %d", d.Candidates))
		}
	}
	return strings.Join(parts, " · ")
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

// hitSources tells docsPreparer where a hit came from and what raglit
// measured about it. Every field is nil-safe.
type hitSources struct {
	index    func(docID string) string // the raglit index of a hit
	attached func(index string) bool   // an index attached from elsewhere (attach.go)
	display  func(path string) string  // a path as a person reads it
	meta     func(docID string) (hitMeta, bool)
	// meaning: whether the hit's index searches by meaning (has
	// embeddings); known is false when the index list does not name it.
	meaning func(docID string) (meaning, known bool)
}

// hitMeta is what raglit says about a hit besides its score (e66a3df on):
// the best cosine among the document's fragments in this search (HasSim
// false when the vector side ranked none of them), and the most query
// words one fragment holds, of the query's Terms.
type hitMeta struct {
	Sim     float64
	HasSim  bool
	Matched int
	Terms   int
}

func (s *hitSources) metaOf(docID string) (hitMeta, bool) {
	if s == nil || s.meta == nil {
		return hitMeta{}, false
	}
	return s.meta(docID)
}

func (s *hitSources) meaningOf(docID string) (meaning, known bool) {
	if s == nil || s.meaning == nil {
		return false, false
	}
	return s.meaning(docID)
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

// surfaceMargin is how far the top hit's score must beat the runner-up's to
// surface, for a hit from a raglit older than e66a3df (no similarity, no
// terms). See the relevance note in docsPreparer.
const surfaceMargin = 1.5

// The surfacing rule for an index that searches by meaning: the top hit's
// similarity is at least surfaceFloor and beats the runner-up's by at least
// surfaceDelta; a lone candidate needs only the floor. For an index without
// embeddings: the top hit holds at least coverageFloor of the query's words
// and, against a runner-up, has surfaceMargin × its score. See the
// relevance note in docsPreparer.
//
// Calibrated 2026-10-23 (plan/done.md, "Surfacing floor"): 43 on-topic and
// 42 off-topic questions over the dun code, skills, memory and rules
// indexes, nomic-embed-text. On-topic right hits scored 0.62–0.84,
// off-topic top hits 0.47–0.73 (37 of 42 under 0.67). 0.68 + 0.01: 22 of
// 43 surfaced the right or a related document, 3 a wrong one, and 1 of 42
// off-topic questions surfaced anything; 0.70 cut that to 0 but surfaced
// the right or a related one on 15 of 43. Without embeddings (bm25):
// coverage 0.5 alone surfaced on 22 of 42 off-topic questions; with the
// ratio, 6.
const (
	surfaceFloor  = 0.68
	surfaceDelta  = 0.01
	coverageFloor = 0.5
)

// surfaceQueryWords caps the search query: the newest user message (or,
// later in a turn, the newest entry), its first words. Measured 2026-10-23
// over 8 long messages (55–92 words) on the code, skills and memory
// indexes: the right document ranked first for
// 4 with the first 50 words (as many as the whole message), 2 with 30, 0
// with 20, 3 with the last 30, 2 with the first 30 content words; and 1
// with the old query (the ~110-word reply before the message joined on),
// which pushed the right document out of the top 20 for 4 of 8.
const surfaceQueryWords = 50

// surfaceQuery is the search query for a pass: the first surfaceQueryWords
// words of text.
func surfaceQuery(text string) string {
	w := strings.Fields(text)
	if len(w) > surfaceQueryWords {
		w = w[:surfaceQueryWords]
	}
	return strings.Join(w, " ")
}

// surfaceRule is how a hit's group decides its winner.
type surfaceRule int

const (
	ruleScore      surfaceRule = iota // a raglit older than e66a3df: the score ratio
	ruleSimilarity                    // an index with embeddings: floor + delta
	ruleCoverage                      // an index without: the query words held
)

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

		// The query: the newest user message since the last pass, else (a
		// later pass in a turn) the newest entry; its first words only. The
		// whole new text was the query before (the reply since the last pass
		// joined to the message): a long query blurs what it is about. See
		// the measurement at surfaceQueryWords' value in plan/done.md.
		var newest, newestUser string
		maxObserved := mark
		for _, e := range entries {
			if e.CreatedAt <= mark || !observed[e.Kind] {
				continue
			}
			if e.Content != "" {
				newest = e.Content
				if e.Kind == agent.KindUser {
					newestUser = e.Content
				}
			}
			if e.CreatedAt > maxObserved {
				maxObserved = e.CreatedAt
			}
		}
		if newestUser != "" {
			newest = newestUser
		}
		query := surfaceQuery(newest)
		if query == "" {
			return nil
		}
		texts := []string{query}

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
		// Best first: by similarity where raglit gave one (a hit without
		// sorts after every hit with), then by score. The score is RRF in a
		// hybrid search, so it ranks within an index but sets no bar.
		// Similarity is per document (its best fragment's), so every
		// fragment of a document sorts together.
		metaOf := map[string]hitMeta{}
		hasMeta := map[string]bool{}
		simIndex := map[string]bool{} // an index that gave a similarity this pass
		for _, h := range kept {
			m, ok := src.metaOf(h.DocID)
			metaOf[h.DocID], hasMeta[h.DocID] = m, ok
			if m.HasSim {
				simIndex[src.indexOf(h.DocID)] = true
			}
		}
		sort.SliceStable(kept, func(i, j int) bool {
			mi, mj := metaOf[kept[i].DocID], metaOf[kept[j].DocID]
			if mi.HasSim != mj.HasSim {
				return mi.HasSim
			}
			if mi.HasSim && mi.Sim != mj.Sim {
				return mi.Sim > mj.Sim
			}
			return kept[i].Score > kept[j].Score
		})
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

		// Relevance: the TOP document only, and only when it clears a bar.
		// Measured in M0 (2026-10-20): "any hit, two slots" made 11 of 21
		// surfaced notes the wrong one and surfaced two on every negative
		// probe. A group that fails surfaces nothing and marks nothing seen,
		// so the same documents can win a later, clearer pass.
		//
		// The bar was a RATIO of BM25 scores (1.5× the runner-up: 14 of 19
		// right hits and 3 of 30 noise in M0), because BM25 scores have no
		// fixed range. It broke when raglit's search went hybrid (e66a3df):
		// the score is then a rank fusion, at most ~0.033, so two candidates
		// almost never differ by 1.5×; and a lone candidate passed with no
		// test at all (item 24 live: 3 lone candidates, 2 of them wrong).
		// Now, per index kind:
		//   - with embeddings: the cosine similarity, which has a fixed range
		//     (nomic-embed-text, 0.45–0.85 in practice). The top hit needs
		//     surfaceFloor, and surfaceDelta over the runner-up's; a lone
		//     candidate needs the floor. A hit with no similarity (the vector
		//     side did not rank it) never surfaces.
		//   - without: the share of the query's words the hit holds
		//     (coverageFloor), and the ratio over the runner-up's score; the
		//     coverage alone let 22 of 42 off-topic questions surface.
		//   - a raglit older than e66a3df (no similarity, no terms): the
		//     ratio, as before.
		// The values and the run they came from: plan/done.md, "Surfacing
		// floor".
		//
		// The rule runs once per GROUP (memory notes, skills, rules,
		// everything else) and index kind, because a score is only comparable
		// within the index that produced it. Measured live (M2, 2026-10-21):
		// the stash note scored 8.5e-6 in the 1-note memory index and
		// suggest.go 2.3e-5 in the code index for "should I stash before
		// rebasing" — one ranking let the code file win every time.
		ruleOf := func(docID string) surfaceRule {
			if !hasMeta[docID] {
				return ruleScore
			}
			meaning, known := src.meaningOf(docID)
			if !known {
				// Not in the index list (an attached index of another
				// project): a similarity on any of its hits says it has
				// embeddings.
				meaning = simIndex[src.indexOf(docID)]
			}
			if meaning {
				return ruleSimilarity
			}
			return ruleCoverage
		}
		key := func(docID string) string {
			g := ""
			if group != nil {
				g = group(docID)
			}
			return fmt.Sprintf("%s\x00%d", g, ruleOf(docID))
		}
		type win struct {
			h           agent.DocHit
			n           int     // candidates in its group
			runnerUp    float64 // the second score (score rule), 0 when alone
			runnerUpSim float64 // the second similarity, 0 when alone or none
		}
		var wins []win
		for _, g := range groupHits(kept, key) {
			top := metaOf[g[0].DocID]
			switch ruleOf(g[0].DocID) {
			case ruleSimilarity:
				if !top.HasSim || top.Sim < surfaceFloor {
					continue
				}
				w := win{h: g[0], n: len(g)}
				if len(g) > 1 {
					// A runner-up with no similarity is weak on meaning:
					// nothing to beat.
					if r := metaOf[g[1].DocID]; r.HasSim {
						if top.Sim-r.Sim < surfaceDelta-1e-9 {
							continue
						}
						w.runnerUpSim = r.Sim
					}
				}
				wins = append(wins, w)
			case ruleCoverage:
				if top.Terms == 0 || float64(top.Matched) < coverageFloor*float64(top.Terms) {
					continue
				}
				if len(g) == 1 {
					wins = append(wins, win{h: g[0], n: 1})
				} else if g[0].Score >= surfaceMargin*g[1].Score {
					wins = append(wins, win{h: g[0], n: len(g), runnerUp: g[1].Score})
				}
			default:
				if len(g) == 1 {
					wins = append(wins, win{h: g[0], n: 1})
				} else if g[0].Score >= surfaceMargin*g[1].Score {
					wins = append(wins, win{h: g[0], n: len(g), runnerUp: g[1].Score})
				}
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
				m := metaOf[h.DocID]
				info := DocHitInfo{Title: h.Title, DocID: h.DocID, Line: h.Line, Score: h.Score,
					Path: src.show(h.DocID), Index: index, Candidates: w.n, RunnerUp: w.runnerUp,
					Matched: m.Matched, QueryTerms: m.Terms, RunnerUpSim: w.runnerUpSim}
				if m.HasSim {
					info.Similarity = m.Sim
				}
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
