package dun

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/proto"
)

// Code and docs search through dund (docs/rag.md, "Session code and doc
// search"; USER 2026-11-09).
//
// dund's code and docs indexes of the project hold the workspace's files
// (rag_index). The session keeps them in step and searches them:
//
//   - index: rag_index for the workspace when the session starts and again
//     when each turn ends, in the background (codeLink.reindex). rag_index is
//     incremental: a file the index holds with the same bytes is not read
//     again. This is the interim trigger, until raglit watches the tree
//     itself: reindex is the one seam a raglit watcher replaces.
//   - search: the `search` tool (when no docs server gives one), the docs
//     server's search (its hits after the server's own, when it names no
//     index), and the proactive search (codeFinder), each a rag_search of
//     both indexes, under the workspace's path, with the workspace as its
//     root: in a linked worktree dund searches the worktree's branch of
//     each index, over the main checkout's (docs/rag.md, "Worktrees").
//
// On by default, with no /rag on; DUN_CODE_SEARCH=0 turns it off (the
// Harbor adapter does). dund down: the session says once that search is
// degraded, and the model has what a session without it has (exec with
// grep, poly-lsp-mcp's node_query).

// codeLink is a session's way to the code and docs indexes. One per root
// harness; a child uses its parent's.
type codeLink struct {
	link   RagLink
	notice func(string) // says, once per outage, that search is degraded

	mu      sync.Mutex
	running bool // an index call is in flight
	again   string
	said    bool
	meta    map[string]codeHitMeta // per DocID, from the latest proactive searches
}

type codeHitMeta struct {
	meta    hitMeta
	meaning bool
	index   string // "<project>/code" or "<project>/docs"
	group   string
}

func newCodeLink(link RagLink, notice func(string)) *codeLink {
	return &codeLink{link: link, notice: notice, meta: map[string]codeHitMeta{}}
}

// codeIndexTimeout bounds one rag_index: dund walks the root and reads each
// file's bytes to compare them; the ingest itself is queued.
const codeIndexTimeout = 2 * time.Minute

// reindex sends rag_index for root in the background. A call while one is
// in flight is folded into one more run when it ends (the newest root), so
// turn ends that come quickly cost one walk, not a queue of them.
//
// The seam for raglit's tree watcher: when raglit watches a root, the
// session start sends one rag_index (or a watch request) and the turn-end
// call goes.
func (l *codeLink) reindex(root string) {
	if l == nil || root == "" {
		return
	}
	l.mu.Lock()
	if l.running {
		l.again = root
		l.mu.Unlock()
		return
	}
	l.running = true
	l.mu.Unlock()
	go func() {
		for {
			l.indexOnce(root)
			l.mu.Lock()
			next := l.again
			l.again = ""
			if next == "" {
				l.running = false
				l.mu.Unlock()
				return
			}
			l.mu.Unlock()
			root = next
		}
	}()
}

func (l *codeLink) indexOnce(root string) {
	ctx, cancel := context.WithTimeout(context.Background(), codeIndexTimeout)
	defer cancel()
	r, err := l.link.Index(ctx, proto.RagIndex{Project: raglitProject(root), Root: root})
	switch {
	case errors.Is(err, ErrRagDown):
		l.down()
	case err != nil:
		log.Printf("dun: code search: index %s: %v", root, err)
	default:
		l.up()
		if r.Queued > 0 || r.Removed > 0 {
			log.Printf("dun: code search: %s: %d files queued, %d removed, %d current", root, r.Queued, r.Removed, r.Current)
		}
	}
}

// codeDegraded is what the session says once when dund cannot be reached.
const codeDegraded = "code search: dund is not reachable — search of this project's code and docs is degraded: " +
	"find code with exec (grep) or node_query instead; it comes back when dund does"

func (l *codeLink) down() {
	l.mu.Lock()
	say := !l.said
	l.said = true
	l.mu.Unlock()
	if say {
		log.Printf("dun: %s", codeDegraded)
		if l.notice != nil {
			l.notice(codeDegraded)
		}
	}
}

func (l *codeLink) up() {
	l.mu.Lock()
	l.said = false
	l.mu.Unlock()
}

// codeHit is one fragment of a code or docs search.
type codeHit struct {
	index   string // "code" or "docs"
	path    string // absolute
	start   int    // 1-based; 0: no span (a window of text)
	end     int    // the last line, inclusive
	symbol  string
	text    string
	score   float64
	sim     *float64
	matched int
	terms   int
	meaning bool // its index searched by meaning
}

// docID is the hit as the proactive search names it: the file and its
// lines, so a pointer says where to read.
func (c codeHit) docID() string {
	if c.start > 0 {
		return fmt.Sprintf("%s:%d-%d", c.path, c.start, max(c.end, c.start))
	}
	return c.path
}

// label is path:start-end and the symbol, the path relative to root.
func (c codeHit) label(root string) string {
	p := c.path
	if rel, err := filepath.Rel(fsutil.Canon(root), p); err == nil && inside(rel) {
		p = rel
	}
	if c.start > 0 {
		p = fmt.Sprintf("%s:%d-%d", p, c.start, max(c.end, c.start))
	}
	if c.symbol != "" {
		p += " " + c.symbol
	}
	return p
}

// search asks the code and docs indexes for q, under root (or the
// subtree under root that sub names), at once. The hits of both, best
// first: by similarity where there is one, then by score. down is true
// when neither index could be reached.
func (l *codeLink) search(ctx context.Context, root, q, sub string, limit int) ([]codeHit, error) {
	root = fsutil.Canon(root) // as dund spells the paths it holds
	prefix := "file://" + root + "/"
	if sub = strings.TrimSpace(sub); sub != "" {
		if !filepath.IsAbs(sub) {
			sub = filepath.Join(root, sub)
		}
		prefix = "file://" + filepath.Clean(sub)
	}
	project := raglitProject(root)
	type result struct {
		index string
		hits  *proto.RagHits
		err   error
	}
	res := make([]result, 2)
	var wg sync.WaitGroup
	for i, ix := range []string{"code", "docs"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := l.link.Search(ctx, proto.RagSearch{Project: project, Index: ix, Query: q, Limit: limit,
				Filter: proto.RagFilter{PathPrefix: prefix}, Root: root})
			res[i] = result{ix, r, err}
		}()
	}
	wg.Wait()
	var out []codeHit
	var firstErr error
	reached := false
	for _, r := range res {
		if r.err != nil {
			if !errors.Is(r.err, ErrRagDown) {
				reached = true
			}
			if firstErr == nil || errors.Is(firstErr, ErrRagDown) {
				firstErr = r.err
			}
			continue
		}
		reached = true
		for _, h := range r.hits.Hits {
			out = append(out, codeHit{index: r.index, path: strings.TrimPrefix(h.Path, "file://"),
				start: h.StartLine, end: max(h.EndLine-1, h.StartLine), // half-open → the last line
				symbol: h.Symbol, text: h.Text, score: h.Score,
				sim: h.Similarity, matched: h.Matched, terms: h.Terms, meaning: r.hits.Mode == "hybrid"})
		}
	}
	if !reached {
		l.down()
		return nil, firstErr
	}
	l.up()
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.sim != nil) != (b.sim != nil) {
			return a.sim != nil
		}
		if a.sim != nil && *a.sim != *b.sim {
			return *a.sim > *b.sim
		}
		return a.score > b.score
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// codeLink is the harness's link to the code and docs indexes: nil without
// a RagLink, with DUN_CODE_SEARCH=0 (NoCodeSearch), with no workspace
// (--no-dir), or in the home directory or / (too much to index unasked). A
// child shares its parent's.
func (h *Harness) codeLink() *codeLink {
	h.codeLinkOnce.Do(func() {
		if p := h.cfg.Parent; p != nil {
			h.codeLinkV = p.codeLink()
			return
		}
		if h.cfg.Rag == nil || h.cfg.NoCodeSearch || h.cfg.NoDir || !codeSearchRoot(h.cfg.Workspace) {
			return
		}
		h.codeLinkV = newCodeLink(h.cfg.Rag, h.cfg.OnNotify)
	})
	return h.codeLinkV
}

// codeSearchRoot: a workspace that is indexed without asking. Not the home
// directory or the file system's root.
func codeSearchRoot(ws string) bool {
	if ws == "" || !filepath.IsAbs(ws) {
		return false
	}
	ws = filepath.Clean(ws)
	if ws == string(filepath.Separator) {
		return false
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == ws {
		return false
	}
	return true
}

// codeReindex is the trigger: at session start and at each turn end of a
// root session (a child's turns end inside its parent's). The root guard's
// count runs off the caller's goroutine, once per session (codeIndexRoot).
func (h *Harness) codeReindex() {
	if h.isChild() {
		return
	}
	if l := h.codeLink(); l != nil {
		go func() {
			if root := h.codeIndexRoot(); root != "" {
				l.reindex(root)
			}
		}()
	}
}

// codeIndexRoot is the root rag_index takes: the workspace, or "" when the
// root guard (rootguard.go) finds it too big — 3 or more repositories, or
// over 50,000 files — which is then not indexed wholesale, said once in the
// log. Such a workspace is no repository dund could take instead: one
// inside a repository is too big by its own files, and the repository
// holds them.
func (h *Harness) codeIndexRoot() string {
	h.codeRootOnce.Do(func() {
		ws := filepath.Clean(h.cfg.Workspace)
		scan := scanRootCached(ws)
		if !scan.TooBig() {
			h.codeRootV = ws
			return
		}
		log.Printf("dun: code search: %s holds %s: not indexed (dun's root guard; took %s)", ws, scanSummary(scan), scan.Took.Round(time.Millisecond))
	})
	return h.codeRootV
}

// codeSearchLimit is the hits a search tool call returns by default.
const codeSearchLimit = 8

// searchToolDef is the `search` tool over dund's code and docs indexes. Same
// name as the docs server's search, so a session has one search tool
// either way; it is listed only when no docs server gives one.
func searchToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "search"
	td.Function.Description = "Search this project's code and docs by meaning and words. Returns the best fragments " +
		"first, each as path:start-end, its symbol (a function, type or heading) and the start of its text; read " +
		"more of one with file_read. For an exact name or string, grep is faster."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "what to find, in words or names"},
			"path":  map[string]any{"type": "string", "description": "only under this file or directory (relative to the workspace)"},
			"limit": map[string]any{"type": "integer", "description": "most hits (default 8)"},
		},
		"required": []string{"query"},
	}
	return td
}

type searchArgs struct {
	Query string `json:"query"`
	Path  string `json:"path"`
	Limit int    `json:"limit"`
}

// withCodeSearch answers the `search` tool from the code and docs indexes.
func withCodeSearch(inner agent.ToolDispatcher, h *Harness, l *codeLink, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "search" {
			return inner(ctx, tc)
		}
		out := h.codeSearchCall(ctx, l, tc.Function.Arguments)
		if onCall != nil {
			var a map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
			onCall("search", a, out)
		}
		return out, nil
	}
}

func (h *Harness) codeSearchCall(ctx context.Context, l *codeLink, raw string) string {
	var a searchArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "search: arguments are not valid JSON: " + err.Error()
	}
	if strings.TrimSpace(a.Query) == "" {
		return "search: query is empty"
	}
	limit := a.Limit
	if limit <= 0 {
		limit = codeSearchLimit
	}
	root := filepath.Clean(h.cfg.Workspace)
	hits, err := l.search(ctx, root, a.Query, a.Path, limit)
	if errors.Is(err, ErrRagDown) {
		return "search: " + strings.TrimPrefix(codeDegraded, "code search: ")
	}
	if err != nil {
		return "search: " + err.Error()
	}
	return formatCodeHits(root, a.Query, hits)
}

// codeSnippetLines is how much of a hit's text a search result shows.
const codeSnippetLines = 6

// formatCodeHits is a search's hits as the model reads them: one block per
// hit, path:start-end and symbol, then the start of its text.
func formatCodeHits(root, q string, hits []codeHit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("search: nothing in this project's code or docs for %q (a file indexed in the last moments may not be in it yet)", q)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d hits for %q in this project's code and docs, best first:", len(hits), q)
	for _, c := range hits {
		b.WriteString("\n\n" + c.label(root))
		lines := strings.Split(strings.TrimSpace(c.text), "\n")
		if len(lines) > codeSnippetLines {
			lines = append(lines[:codeSnippetLines], "…")
		}
		for _, ln := range lines {
			if len(ln) > 200 {
				ln = ln[:200] + "…"
			}
			b.WriteString("\n  " + ln)
		}
	}
	return b.String()
}

// withCodeInDocsSearch adds the code and docs hits to the docs server's
// search (/rag on: raglit's own corpora) when the call names no index: the
// server's answer, then this project's code and docs. tool is its model
// name.
func withCodeInDocsSearch(inner agent.ToolDispatcher, h *Harness, l *codeLink, tool string) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != tool {
			return inner(ctx, tc)
		}
		var a struct {
			searchArgs
			Index string `json:"index"`
		}
		if json.Unmarshal([]byte(tc.Function.Arguments), &a) != nil || strings.TrimSpace(a.Index) != "" || strings.TrimSpace(a.Query) == "" {
			return inner(ctx, tc)
		}
		out, err := inner(ctx, tc)
		if err != nil {
			return out, err
		}
		code := h.codeSearchCall(ctx, l, tc.Function.Arguments)
		return out + "\n\nThis project's code and docs (dund): " + strings.TrimPrefix(code, "search: "), nil
	}
}

// describeCodeInDocsSearch says so in the docs server's search tool.
func describeCodeInDocsSearch(defs []llm.ToolDef, tool string) {
	for i := range defs {
		if defs[i].Function.Name == tool {
			defs[i].Function.Description += " With no `index`, this project's code and docs are searched too; " +
				"their hits follow the JSON, as path:start-end and symbol (read one with file_read)."
		}
	}
}

// The proactive search's groups of a code and a docs hit.
const (
	groupCode = "code"
	groupDocs = "workspace-docs"
)

// codeFinder is the proactive search's finder over the code and docs
// indexes: a hit is a fragment, named by its file and lines.
type codeFinder struct {
	l    *codeLink
	root func() string
}

// codeFindLimit is the hits asked of the two indexes per pass.
const codeFindLimit = 8

func (f *codeFinder) Find(ctx context.Context, texts []string) ([]agent.DocHit, error) {
	q := strings.TrimSpace(strings.Join(texts, " "))
	if q == "" {
		return nil, nil
	}
	root := f.root()
	hits, err := f.l.search(ctx, root, q, "", codeFindLimit)
	if err != nil {
		if errors.Is(err, ErrRagDown) {
			return nil, nil // said once; the other finders still answer
		}
		return nil, err
	}
	project := raglitProject(root)
	out := make([]agent.DocHit, 0, len(hits))
	for _, c := range hits {
		m := hitMeta{Matched: c.matched, Terms: c.terms}
		if c.sim != nil {
			m.Sim, m.HasSim = *c.sim, true
		}
		g := groupCode
		if c.index == "docs" {
			g = groupDocs
		}
		id := c.docID()
		f.l.record(id, codeHitMeta{meta: m, meaning: c.meaning, index: project + "/" + c.index, group: g})
		line, _, _ := strings.Cut(strings.TrimSpace(c.text), "\n")
		out = append(out, agent.DocHit{DocID: id, Title: c.label(root), Score: c.score, Line: line})
	}
	return out, nil
}

func (l *codeLink) record(id string, m codeHitMeta) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.meta[id] = m
}

func (l *codeLink) hit(id string) (codeHitMeta, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.meta[id]
	return m, ok
}

// readSpan reads a hit's lines (docID path:start-end, or a whole file) when
// they are at most max chars.
func readSpan(docID string, max int) (string, bool) {
	path, start, end := splitSpan(docID)
	if start == 0 {
		return fetchNote(path, max)
	}
	fh, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer fh.Close()
	var b strings.Builder
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for n := 1; sc.Scan() && n <= end; n++ {
		if n < start {
			continue
		}
		b.WriteString(sc.Text())
		b.WriteByte('\n')
		if b.Len() > max {
			return "", false
		}
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

// splitSpan is docID's path and lines; 0, 0 when it names a whole file.
func splitSpan(docID string) (string, int, int) {
	i := strings.LastIndexByte(docID, ':')
	if i < 0 {
		return docID, 0, 0
	}
	var s, e int
	if n, _ := fmt.Sscanf(docID[i+1:], "%d-%d", &s, &e); n != 2 || s <= 0 || e < s {
		return docID, 0, 0
	}
	return docID[:i], s, e
}

// withCodeFinder adds the code finder to the proactive search, as
// withMemFinder does the memory finder's.
func withCodeFinder(cf *codeFinder, inner agent.DocFinder, fetch docBodyFunc, src *hitSources, group func(string) string) (agent.DocFinder, docBodyFunc, func(string) string) {
	index, meta, meaning := src.index, src.meta, src.meaning
	src.index = func(id string) string {
		if m, ok := cf.l.hit(id); ok {
			return m.index
		}
		if index != nil {
			return index(id)
		}
		return ""
	}
	src.meta = func(id string) (hitMeta, bool) {
		if m, ok := cf.l.hit(id); ok {
			return m.meta, true
		}
		if meta != nil {
			return meta(id)
		}
		return hitMeta{}, false
	}
	src.meaning = func(id string) (bool, bool) {
		if m, ok := cf.l.hit(id); ok {
			return m.meaning, true
		}
		if meaning != nil {
			return meaning(id)
		}
		return false, false
	}
	readAll := func(ctx context.Context, id string, max int) (string, bool) {
		if _, ok := cf.l.hit(id); ok {
			return readSpan(id, max)
		}
		if fetch != nil {
			return fetch(ctx, id, max)
		}
		return "", false
	}
	grouped := func(id string) string {
		if m, ok := cf.l.hit(id); ok {
			return m.group
		}
		if group != nil {
			return group(id)
		}
		return ""
	}
	if inner == nil {
		return cf, readAll, grouped
	}
	return bothFinders{cf, inner}, readAll, grouped
}
