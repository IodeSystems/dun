package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/agentkit/ragnotify"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// A session's side of extensions (manifests: internal/mcpserver/extension.go):
// which are loaded, their hooks and index sinks, and surfacing their hits.

// Extensions is every loaded extension, bundled first.
func (h *Harness) Extensions() []mcpserver.Extension { return h.extensions() }

// ExtensionErrors are the manifests that did not load.
func (h *Harness) ExtensionErrors() []mcpserver.ExtError { return h.extErrs }

// extensions is the loaded list, or the bundled ones for a harness built
// without Start (tests).
func (h *Harness) extensions() []mcpserver.Extension {
	if h.exts != nil {
		return h.exts
	}
	return mcpserver.BundledExtensions()
}

// extension is the extension whose server is id.
func (h *Harness) extension(id string) (mcpserver.Extension, bool) {
	for _, e := range h.extensions() {
		if e.ID == id {
			return e, true
		}
	}
	return mcpserver.Extension{}, false
}

// hasTool reports whether tools has server id's tool name.
func hasTool(tools []mcpmgr.MCPTool, id, name string) bool {
	for _, t := range tools {
		if t.ServerID == id && t.Name == name {
			return true
		}
	}
	return false
}

// searchExts are the extensions taking turn.before whose search tool is in
// tools (running, and not hidden), in load order.
func searchExts(exts []mcpserver.Extension, tools []mcpmgr.MCPTool) []mcpserver.Extension {
	var out []mcpserver.Extension
	for _, e := range exts {
		if e.Subscribes(mcpserver.HookTurnBefore) && hasTool(tools, e.ID, e.Roles.Search.Tool) {
			out = append(out, e)
		}
	}
	return out
}

// hookExt is the first extension taking hook whose server has tools in
// tools, i.e. is running.
func hookExt(exts []mcpserver.Extension, hook string, tools []mcpmgr.MCPTool) (mcpserver.Extension, bool) {
	for _, e := range exts {
		if !e.Subscribes(hook) {
			continue
		}
		for _, t := range tools {
			if t.ServerID == e.ID {
				return e, true
			}
		}
	}
	return mcpserver.Extension{}, false
}

// hookStarted reports whether an extension taking hook has its server up.
func (h *Harness) hookStarted(hook string) bool {
	if h.mgr == nil {
		return false
	}
	for _, e := range h.extensions() {
		if e.Subscribes(hook) && h.mgr.ServerStarted(e.ID) {
			return true
		}
	}
	return false
}

// indexSink is where e's index role sends documents, or nil.
//
// THE SEAM: dun sends documents in batches (500 paths a call) and forgets
// them again. raglit's MCP `ingest` takes one URL and it has no `forget`
// tool (asked of raglit, 2026-10-23), so its manifest names the CLI
// fallback ("cli": "raglit") and dun runs the server's own binary with the
// store flags of its serve args. Once the server's index tool takes a
// `paths` array and its forget tool exists, the MCP tools are used instead,
// with no change to dun or to the manifest: {"index", "paths"} to each.
func (h *Harness) indexSink(e mcpserver.Extension) *mcpDocsSink {
	r := e.Roles.Index
	if r == nil {
		return nil
	}
	h.srvMu.Lock()
	spec, ok := h.spec(e.ID)
	h.srvMu.Unlock()
	if !ok {
		return nil
	}
	if r.Tool != "" && h.mgr != nil && h.mgr.ServerStarted(e.ID) {
		tools := h.mgr.GetTools()
		if toolTakes(tools, e.ID, r.Tool, "paths") && hasTool(tools, e.ID, r.Forget) {
			return mcpIndexSink(h.mgr, e.ID, r.Tool, r.Forget)
		}
	}
	if r.CLI != "" && filepath.Base(spec.Command) == r.CLI {
		return raglitDocsSink(spec, h.raglitEnv())
	}
	return nil
}

// toolTakes reports whether server id's tool has the parameter param.
func toolTakes(tools []mcpmgr.MCPTool, id, tool, param string) bool {
	for _, t := range tools {
		if t.ServerID != id || t.Name != tool {
			continue
		}
		props, _ := t.InputSchema["properties"].(map[string]any)
		_, ok := props[param]
		return ok
	}
	return false
}

// mcpIndexSink sends documents through an extension's MCP tools, in the
// same batches the CLI gets.
func mcpIndexSink(mgr *mcpmgr.Manager, id, ingest, forget string) *mcpDocsSink {
	call := func(tool, index string, paths []string) error {
		for _, batch := range chunkArgs(paths, ingestBatch) {
			if _, err := mgr.CallTool(context.Background(), id, tool, map[string]any{"index": index, "paths": batch}); err != nil {
				return fmt.Errorf("%s %s: %w", id, tool, err)
			}
		}
		return nil
	}
	return &mcpDocsSink{
		ingest: func(index string, paths []string) error { return call(ingest, index, paths) },
		forget: func(index string, paths []string) error { return call(forget, index, paths) },
	}
}

// surfacer is the proactive search over the turn.before extensions: their
// finders, merged; which one each hit came from; and the reader of a short
// hit. One extension is that extension's finder as it is.
type surfacer struct {
	finder agent.DocFinder
	fetch  docBodyFunc
	from   func(docID string) string // the extension a hit came from
	idx    *hitIndexes               // the index each hit came from
	means  *indexMeaning             // which indexes search by meaning
	sel    map[string]string         // each extension's attached-index selector
}

// meaning reports whether a hit's index searches by meaning; known is false
// when its extension's index list does not name it.
func (s *surfacer) meaning(docID string) (meaning, known bool) {
	return s.means.of(s.from(docID), s.idx.get(docID))
}

// attached reports whether index is one of the attached indexes of an
// extension's selector: "<own>:*,<proj>:<idx>,…" names them after the
// first. Own-project attachments are not in the selector; their hits read
// as the project's own.
func (s *surfacer) attached(index string) bool {
	for _, sel := range s.sel {
		for _, part := range strings.Split(sel, ",") {
			if part == index && !strings.HasSuffix(part, ":*") {
				return true
			}
		}
	}
	return false
}

// hitIndexes remembers the index each surfaced hit came from, and what
// raglit measured about it in the latest search (hitMeta): raglit tags every
// hit with both, and agent.DocHit has no field for them.
type hitIndexes struct {
	mu   sync.Mutex
	m    map[string]string
	meta map[string]hitMeta
}

// note records the indexes and measures of a search result's hits, and
// returns the indexes named. Hits are best first, so a document in two
// indexes keeps the index of its best hit. A document's measures are its
// fragments' best: the highest similarity, the most query words. A hit with
// no query_terms is from a raglit older than e66a3df and has none.
func (x *hitIndexes) note(raw string) []string {
	type hit struct {
		DocID        string   `json:"doc_id"`
		ID           string   `json:"id"`
		Index        string   `json:"index"`
		Similarity   *float64 `json:"similarity"`
		MatchedTerms []string `json:"matched_terms"`
		QueryTerms   *int     `json:"query_terms"`
	}
	var rows []hit
	if s := strings.TrimSpace(raw); strings.HasPrefix(s, "[") {
		_ = json.Unmarshal([]byte(s), &rows)
	} else {
		var wrap struct{ Hits, Results, Documents []hit }
		_ = json.Unmarshal([]byte(s), &wrap)
		rows = append(append(wrap.Hits, wrap.Results...), wrap.Documents...)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.m == nil {
		x.m = map[string]string{}
		x.meta = map[string]hitMeta{}
	}
	done := map[string]bool{}
	named := map[string]bool{}
	var indexes []string
	pass := map[string]hitMeta{}
	for _, r := range rows {
		id := firstNonEmptyStr(r.DocID, r.ID)
		if id == "" {
			continue
		}
		if r.QueryTerms != nil {
			m := pass[id]
			m.Terms = *r.QueryTerms
			m.Matched = max(m.Matched, len(r.MatchedTerms))
			if s := r.Similarity; s != nil && (!m.HasSim || *s > m.Sim) {
				m.Sim, m.HasSim = *s, true
			}
			pass[id] = m
		}
		if r.Index != "" && !named[r.Index] {
			named[r.Index] = true
			indexes = append(indexes, r.Index)
		}
		if r.Index == "" || done[id] {
			continue
		}
		done[id] = true
		x.m[id] = r.Index
	}
	for id, m := range pass {
		x.meta[id] = m
	}
	return indexes
}

func (x *hitIndexes) get(docID string) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.m[docID]
}

// metaOf is a document's measures from the latest search that returned it;
// ok is false when raglit gave none.
func (x *hitIndexes) metaOf(docID string) (hitMeta, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	m, ok := x.meta[docID]
	return m, ok
}

// indexMeaning is which indexes search by meaning (have embeddings), read
// from each extension's list role (raglit's list_indexes, `meaning`). It is
// read once per surfacer — a new surfacer is built when a server restarts or
// the attach set changes (applyTools) — and once more for an index the list
// did not name. An index it does not know is left to docsPreparer.
type indexMeaning struct {
	mu    sync.Mutex
	known map[string]map[string]bool // extension → index → meaning
	asked map[string]bool            // extension, and extension+index after a miss
}

// load reads ext's index list when it has not, or when one of indexes is
// not in it and has not been asked about. Fails open: an index it cannot
// read stays unknown.
func (x *indexMeaning) load(ctx context.Context, mgr *mcpmgr.Manager, ext, tool string, indexes []string) {
	if x == nil || tool == "" {
		return
	}
	x.mu.Lock()
	if x.known == nil {
		x.known, x.asked = map[string]map[string]bool{}, map[string]bool{}
	}
	ask := !x.asked[ext]
	x.asked[ext] = true
	for _, ix := range indexes {
		if _, ok := x.known[ext][ix]; !ok && !x.asked[ext+"\x00"+ix] {
			x.asked[ext+"\x00"+ix] = true
			ask = true
		}
	}
	x.mu.Unlock()
	if !ask {
		return
	}
	raw, err := mgr.CallTool(ctx, ext, tool, map[string]any{})
	if err != nil {
		return
	}
	type row struct {
		Name    string `json:"name"`
		Meaning *bool  `json:"meaning"`
	}
	var rows []row
	if s := strings.TrimSpace(raw); strings.HasPrefix(s, "[") {
		_ = json.Unmarshal([]byte(s), &rows)
	} else {
		var wrap struct{ Indexes []row }
		_ = json.Unmarshal([]byte(s), &wrap)
		rows = wrap.Indexes
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.known[ext] == nil {
		x.known[ext] = map[string]bool{}
	}
	for _, r := range rows {
		if r.Name != "" && r.Meaning != nil {
			x.known[ext][r.Name] = *r.Meaning
		}
	}
}

func (x *indexMeaning) of(ext, index string) (meaning, known bool) {
	if x == nil {
		return false, false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	meaning, known = x.known[ext][index]
	return meaning, known
}

// indexedFinder is ragnotify's MCP finder that also records each hit's
// index and measures in idx, and reads which of those indexes search by
// meaning (list, the search role's list tool) into means.
type indexedFinder struct {
	mgr   *mcpmgr.Manager
	id    string
	tool  string
	list  string
	extra map[string]any
	idx   *hitIndexes
	means *indexMeaning
}

// Find searches without a mode: raglit then searches each index by meaning
// and by words (hybrid) when it has embeddings, by words when not. An
// explicit hybrid or vec skips every index without embeddings.
func (f *indexedFinder) Find(ctx context.Context, texts []string) ([]agent.DocHit, error) {
	args := map[string]any{"query": strings.Join(texts, "\n")}
	for k, v := range f.extra {
		args[k] = v
	}
	raw, err := f.mgr.CallTool(ctx, f.id, f.tool, args)
	if err != nil {
		return nil, fmt.Errorf("surfacer: call %s: %w", f.tool, err)
	}
	f.means.load(ctx, f.mgr, f.id, f.list, f.idx.note(raw))
	return ragnotify.ParseHits(raw)
}

// sel is each extension's attached-index selector (attach.go), "" for none.
func newSurfacer(mgr *mcpmgr.Manager, exts []mcpserver.Extension, sel map[string]string) *surfacer {
	if len(exts) == 0 {
		return nil
	}
	finders := make([]agent.DocFinder, len(exts))
	fetches := map[string]docBodyFunc{}
	idx := &hitIndexes{}
	means := &indexMeaning{}
	for i, e := range exts {
		var extra map[string]any
		getIndex := ""
		if s := sel[e.ID]; s != "" {
			extra = map[string]any{"index": s}
			if toolTakes(mgr.GetTools(), e.ID, e.Roles.Search.Get, "index") {
				getIndex = s
			}
		}
		finders[i] = &indexedFinder{mgr: mgr, id: e.ID, tool: e.Roles.Search.Tool, list: e.Roles.Search.List,
			extra: extra, idx: idx, means: means}
		if e.Roles.Search.Get != "" {
			fetches[e.ID] = docsBodyFetcher(mgr, e.ID, e.Roles.Search.Get, getIndex)
		}
	}
	if len(exts) == 1 {
		id := exts[0].ID
		return &surfacer{finder: finders[0], fetch: fetches[id], from: func(string) string { return id }, idx: idx, means: means, sel: sel}
	}
	m := &multiFinder{ids: make([]string, len(exts)), finders: finders, from: map[string]string{}}
	for i, e := range exts {
		m.ids[i] = e.ID
	}
	fetch := func(ctx context.Context, docID string, max int) (string, bool) {
		if f := fetches[m.source(docID)]; f != nil {
			return f(ctx, docID, max)
		}
		return "", false
	}
	return &surfacer{finder: m, fetch: fetch, from: m.source, idx: idx, means: means, sel: sel}
}

// multiFinder asks every finder and remembers which one answered each hit.
// Scores from two extensions do not compare, so each extension after the
// first is its own surfacing group (see group).
type multiFinder struct {
	ids     []string
	finders []agent.DocFinder
	mu      sync.Mutex
	from    map[string]string
}

func (m *multiFinder) Find(ctx context.Context, texts []string) ([]agent.DocHit, error) {
	var all []agent.DocHit
	var firstErr error
	ok := false
	for i, f := range m.finders {
		hits, err := f.Find(ctx, texts)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok = true
		m.mu.Lock()
		for _, h := range hits {
			if _, seen := m.from[h.DocID]; !seen {
				m.from[h.DocID] = m.ids[i]
			}
		}
		m.mu.Unlock()
		all = append(all, hits...)
	}
	if !ok {
		return nil, firstErr
	}
	return all, nil
}

func (m *multiFinder) source(docID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.from[docID]
}

// group wraps a surfacing group function: a hit from an extension other
// than the first is grouped by that extension.
func (s *surfacer) group(first string, inner func(string) string) func(string) string {
	return func(docID string) string {
		if id := s.from(docID); id != "" && id != first {
			return "ext:" + id
		}
		return inner(docID)
	}
}
