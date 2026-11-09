// Package ragd is retrieval in dund (docs/rag.md, slice 2): dund hosts
// raglit's engine, owns the indexes, and runs their ingest jobs.
//
// Layout, under root (<DUN_HOME>/dund/rag):
//
//	global/{memories,strategies}/
//	<project-slug>/{code,docs,memories,strategies}/
//
// Each index is its own raglit Home (raglit.OpenHome). An index is created on
// its first ingest. Indexes already on disk are opened at start, so jobs
// queued before a restart run again; a search or a job list of an index that
// does not exist answers empty and creates nothing.
//
// The index model record (docs/rag.md, storage item 4): each index records
// the embed model its vectors came from and their dimension (raglit's
// index_meta embed_model and embed_dim, written by SyncEmbedModel). An index
// whose record differs from the embed role gets no embedder: no vector is
// written to it and it is searched by text only, until Reembed.
package ragd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/raglit"

	"github.com/iodesystems/dun/proto"
)

// Global is the directory of the indexes that belong to no project.
const Global = "global"

var (
	projectIndexes = []string{"code", "docs", "memories", "strategies"}
	globalIndexes  = []string{"memories", "strategies"}
)

// DrainGrace is how long running jobs get to finish when dund stops before
// they are cut off (and go back on the queue).
const DrainGrace = 10 * time.Second

// stopWait bounds the wait for the runners' Done after the cut-off: a
// cancelled caption call or job writing its state back.
const stopWait = 10 * time.Second

// memoryRunners is how many ingest jobs the memory runner runs at once.
// Notes are small; two keep one slow source from holding up the rest.
const memoryRunners = 2

// Host owns every index and the job runners. It is a raglit.StoreSet.
//
// Two runners share one Channels (the per-model admission limits): one over
// the memories and strategies indexes of every scope, one over code and
// docs. A bulk code ingest then holds no runner a memory note waits for;
// model capacity is still shared (raglit ragcore.md, R8 "Priority").
type Host struct {
	root    string
	models  raglit.Models
	embed   *raglit.Embedder // nil: FTS only
	dim     int              // the embed model's probed dimension; 0 when not probed
	ident   *raglit.Identifier
	runners []*raglit.Runner // memories+strategies, code+docs
	logf    func(string, ...any)

	// bg bounds the vector backfills; Close cancels it and waits on fills.
	bg     context.Context
	stopBg context.CancelFunc
	fills  sync.WaitGroup

	mu      sync.Mutex
	stores  map[string]*raglit.Store // by "<dir>/<index>"
	stale   map[string]string        // index → why its vectors are not the embed role's
	filling map[string]bool          // index → a backfill runs
	closed  bool
}

// metaEmbedDim is raglit's index_meta key of the vectors' dimension. raglit
// writes it (SyncEmbedModel); dund only reads it. Before raglit v0.6, dund
// wrote this key itself, with the same name and meaning (the probed vector
// length), so an index from then needs no migration: raglit reads the value
// as its own.
const metaEmbedDim = "embed_dim"

// Open opens the indexes under root that exist and starts the runner. r is
// the model roles (Resolve); its Channels is set here when nil.
func Open(root string, r Roles, logf func(string, ...any)) (*Host, error) {
	if logf == nil {
		logf = log.Printf
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("rag: %w", err)
	}
	m := r.Models
	if m.Channels == nil {
		m.Channels = raglit.NewChannels()
	}
	h := &Host{root: root, models: m, logf: logf, stores: map[string]*raglit.Store{},
		stale: map[string]string{}, filling: map[string]bool{}}
	h.bg, h.stopBg = context.WithCancel(context.Background())
	if m.Embed.Model != "" {
		h.embed = raglit.NewEmbedder(llm.NewClient(m.Embed.BaseURL, m.Embed.APIKey, m.Embed.Model), m.Embed.Model)
		h.dim = embedDimension(r.CapsDir, m.Embed)
	}
	h.ident = m.Identifier()
	for _, name := range onDisk(root) {
		if _, err := h.open(name); err != nil {
			h.stopBg()
			h.fills.Wait()
			h.closeStores()
			return nil, err
		}
	}
	for _, set := range []struct {
		indexes []string
		runners int
	}{{[]string{"memories", "strategies"}, memoryRunners}, {[]string{"code", "docs"}, 0}} {
		r := &raglit.Runner{
			Stores: storeSubset{h, set.indexes},
			NewWorker: func(s *raglit.Store) (*raglit.Worker, error) {
				return raglit.NewWorker(s, h.models, nil), nil
			},
			Runners: set.runners,
			Logf:    logf,
			Notef:   func(f string, a ...any) { logf("dund: rag: "+f, a...) },
		}
		r.Start(context.Background())
		h.runners = append(h.runners, r)
	}
	return h, nil
}

// storeSubset is the Host's open indexes of some kinds ("memories" and so
// on), in every scope: one runner's raglit.StoreSet.
type storeSubset struct {
	h       *Host
	indexes []string
}

func (s storeSubset) Names() []string {
	var out []string
	for _, n := range s.h.Names() {
		if s.has(n) {
			out = append(out, n)
		}
	}
	return out
}

func (s storeSubset) Existing(name string) (*raglit.Store, error) {
	if !s.has(name) {
		return nil, os.ErrNotExist
	}
	return s.h.Existing(name)
}

func (s storeSubset) has(name string) bool {
	_, ix, _ := strings.Cut(name, "/")
	return slices.Contains(s.indexes, ix)
}

// onDisk is every "<dir>/<index>" under root that has an index file.
func onDisk(root string) []string {
	var out []string
	dirs, _ := os.ReadDir(root)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		for _, ix := range indexesOf(d.Name()) {
			name := d.Name() + "/" + ix
			if _, err := os.Stat(raglit.Home(filepath.Join(root, name)).IndexPath()); err == nil {
				out = append(out, name)
			}
		}
	}
	return out
}

func indexesOf(dir string) []string {
	if dir == Global {
		return globalIndexes
	}
	return projectIndexes
}

// Names is the open indexes (raglit.StoreSet).
func (h *Host) Names() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.stores))
	for n := range h.stores {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Existing is the open index called name (raglit.StoreSet). It never opens
// or creates one.
func (h *Host) Existing(name string) (*raglit.Store, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st, ok := h.stores[name]; ok {
		return st, nil
	}
	return nil, os.ErrNotExist
}

// Name is the index's key and directory under root: "<dir>/<index>", where
// dir is the project's slug or Global. It refuses an index the project does
// not keep, and a slug that is not one.
func Name(project, index string) (string, error) {
	dir := project
	if dir == "" {
		dir = Global
	} else if err := checkSlug(project); err != nil {
		return "", err
	}
	if !slices.Contains(indexesOf(dir), index) {
		if dir == Global {
			return "", fmt.Errorf("rag: no global index %q: global has %s", index, strings.Join(globalIndexes, ", "))
		}
		return "", fmt.Errorf("rag: no index %q: a project has %s", index, strings.Join(projectIndexes, ", "))
	}
	return dir + "/" + index, nil
}

// checkSlug: a project slug is lower-case letters, digits, '-', '_' and
// '.', not starting with '.', and not "global" (the global set's directory).
func checkSlug(s string) error {
	if s == Global {
		return errors.New(`rag: "global" is the global set's name, not a project's: leave project empty`)
	}
	if s == "" || s[0] == '.' || len(s) > 64 {
		return fmt.Errorf("rag: %q is not a project slug", s)
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return fmt.Errorf("rag: %q is not a project slug (a-z, 0-9, - _ .)", s)
		}
	}
	return nil
}

// store is the open index name; create opens (and makes) it when it is not.
// Without create, an index not on disk is (nil, nil).
func (h *Host) store(name string, create bool) (*raglit.Store, error) {
	if st, err := h.Existing(name); err == nil {
		return st, nil
	}
	if !create {
		if _, err := os.Stat(raglit.Home(filepath.Join(h.root, name)).IndexPath()); err != nil {
			return nil, nil
		}
	}
	return h.open(name)
}

func (h *Host) open(name string) (*raglit.Store, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errors.New("rag: dund is stopping")
	}
	if st, ok := h.stores[name]; ok {
		return st, nil
	}
	st, err := raglit.OpenHome(raglit.Home(filepath.Join(h.root, name)))
	if err != nil {
		return nil, fmt.Errorf("rag: open %s: %w", name, err)
	}
	// Both before the runner can see the store: search embeds queries through
	// the embedder, and the caption queue drains only where an identifier is.
	h.attachEmbed(name, st)
	st.SetIdentifier(h.ident)
	h.stores[name] = st
	return st, nil
}

// attachEmbed gives st the embedder when its record matches the embed role,
// under h.mu. An index with no record adopts the role's model (raglit's
// rule: its vectors are from the model configured when they were made). A
// match with fragments that have no vector (ingested while embed was off,
// or a re-embed cut off by a stop) starts a backfill.
func (h *Host) attachEmbed(name string, st *raglit.Store) {
	if h.embed == nil {
		st.SetEmbedder(nil)
		return
	}
	have := st.EmbedModel()
	haveDim := metaInt(st, metaEmbedDim)
	if have != "" && (have != h.embed.Model || haveDim > 0 && h.dim > 0 && haveDim != h.dim) {
		why := fmt.Sprintf("its vectors are from %s%s, the embed role is %s%s",
			have, dimSaid(haveDim), h.embed.Model, dimSaid(h.dim))
		h.stale[name] = why
		st.SetEmbedder(nil)
		h.logf("dund: rag: %s: %s: no vectors are written to it and it is searched by text only; `dun dund rag reembed` rebuilds them", name, why)
		return
	}
	// No record: adopt the role. A match: record a dimension not yet
	// recorded. Vectors of another length than h.dim (an index with no dim
	// record) cannot be compared with a query at all; raglit drops them, and
	// the backfill below rebuilds them.
	if dropped, err := st.SyncEmbedModel(h.embed.Model, h.dim); err != nil {
		h.logf("dund: rag: %s: embed model record: %v", name, err)
	} else if dropped {
		h.logf("dund: rag: %s: dropped vectors of another dimension than %s%s", name, h.embed.Model, dimSaid(h.dim))
	}
	st.SetEmbedder(h.embed)
	h.backfillLocked(name, st)
}

func dimSaid(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" (dimension %d)", n)
}

func metaInt(st *raglit.Store, key string) int {
	v, _ := st.Meta(key)
	n, _ := strconv.Atoi(v)
	return n
}

// backfillLocked embeds, in the background, every fragment of st that has no
// vector, under h.mu. One fill per index at a time; a failure stops it until
// the next open or Reembed.
func (h *Host) backfillLocked(name string, st *raglit.Store) {
	if h.filling[name] {
		return
	}
	if n, err := st.MissingVectors(); err != nil || n == 0 {
		return
	}
	h.filling[name] = true
	h.fills.Add(1)
	go func() {
		defer h.fills.Done()
		total := 0
		for h.bg.Err() == nil {
			n, err := st.BackfillVectors(h.bg, 64)
			if err != nil {
				if h.bg.Err() == nil {
					h.logf("dund: rag: %s: embedding stopped after %d fragments: %v", name, total, err)
				}
				break
			}
			if n == 0 {
				break
			}
			total += n
		}
		if total > 0 {
			h.logf("dund: rag: %s: embedded %d fragments that had no vector", name, total)
		}
		h.mu.Lock()
		delete(h.filling, name)
		h.mu.Unlock()
	}()
}

// Reembed rebuilds an index's vectors with the embed role: every vector is
// dropped (DropVectors), the record is set to the role's model and dimension
// (SyncEmbedModel), the embedder is attached, and every fragment is embedded
// in the background. A job running on the index meanwhile is not a problem:
// raglit's SetEmbedder is atomic and its vector writes check the embedder
// they were made with (TestReembedWhileAJobRuns, -race).
func (h *Host) Reembed(q proto.RagReembed) (*proto.RagReembedding, error) {
	name, err := Name(q.Project, q.Index)
	if err != nil {
		return nil, err
	}
	if h.embed == nil {
		return nil, errNoEmbed
	}
	st, err := h.store(name, false)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("rag: no index %s yet", name)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errors.New("rag: dund is stopping")
	}
	n, err := st.DropVectors()
	if err != nil {
		return nil, err
	}
	if _, err := st.SyncEmbedModel(h.embed.Model, h.dim); err != nil {
		return nil, err
	}
	delete(h.stale, name)
	st.SetEmbedder(h.embed)
	missing, err := st.MissingVectors()
	if err != nil {
		return nil, err
	}
	if n > 0 {
		h.logf("dund: rag: %s: dropped %d vectors; embedding %d fragments with %s", name, n, missing, h.embed.Model)
	}
	h.backfillLocked(name, st)
	return &proto.RagReembedding{Index: name, Model: h.embed.Model, Dim: h.dim, Dropped: n > 0, Missing: missing}, nil
}

// staleWhy is why name's vectors are not the embed role's; "" when they are.
func (h *Host) staleWhy(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stale[name]
}

// Search runs q on its index.
func (h *Host) Search(ctx context.Context, q proto.RagSearch) (*proto.RagHits, error) {
	name, err := Name(q.Project, q.Index)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(q.Query) == "" {
		return nil, errors.New("rag: an empty query")
	}
	mode := q.Mode
	switch mode {
	case "", "hybrid":
		mode = "hybrid"
	case "fts":
	default:
		return nil, fmt.Errorf("rag: no search mode %q (hybrid, fts)", q.Mode)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	out := &proto.RagHits{Mode: mode, Hits: []proto.RagHit{}}
	st, err := h.store(name, false)
	if err != nil || st == nil {
		return out, err
	}
	f := raglit.SearchFilter{PathPrefix: q.Filter.PathPrefix, Tags: q.Filter.Tags, Fields: q.Filter.Fields}
	var hits []raglit.Hit
	if mode == "hybrid" && h.embed == nil {
		mode, out.Note = "fts", "no embed model (facets.embed): searched by text only"
	}
	if why := h.staleWhy(name); mode == "hybrid" && why != "" {
		mode, out.Note = "fts", "searched by text only: "+why+"; `dun dund rag reembed` rebuilds the vectors"
	}
	if mode == "hybrid" {
		hits, err = st.HybridSearchFiltered(ctx, q.Query, f, limit)
		if err != nil {
			mode, out.Note = "fts", "the embed model failed ("+err.Error()+"): searched by text only"
		}
	}
	if mode == "fts" {
		if hits, err = st.SearchFiltered(q.Query, f, limit); err != nil {
			return nil, err
		}
	}
	out.Mode = mode
	for _, x := range hits {
		out.Hits = append(out.Hits, proto.RagHit{Path: x.Path, Title: x.Title, Page: x.Page, Text: x.Text, Score: x.Score,
			Similarity: x.Similarity, Matched: len(x.Matched), Terms: x.QueryTerms})
	}
	return out, nil
}

// Ingest queues one source on its index, creating the index if needed.
func (h *Host) Ingest(q proto.RagIngest) (*proto.RagIngested, error) {
	name, err := Name(q.Project, q.Index)
	if err != nil {
		return nil, err
	}
	src, err := source(q.Path, q.URL)
	if err != nil {
		return nil, err
	}
	st, err := h.store(name, true)
	if err != nil {
		return nil, err
	}
	if q.Text != "" {
		if q.Path == "" {
			return nil, errors.New("rag: text is ingested under a path, not a URL")
		}
		if err := ingestText(st, src, q.Title, q.Text, q.Tags); err != nil {
			return nil, err
		}
		return &proto.RagIngested{URL: src}, nil
	}
	id, err := enqueue(st, src, q.Title, q.Tags)
	if err != nil {
		return nil, err
	}
	return &proto.RagIngested{Job: id, URL: src}, nil
}

// textHashKey is the index_meta key of the sha256 (hex) of the text
// ingested for src by ingestText. raglit records a source hash only for
// what it reads itself, so dund records this one; rag_sync compares it.
func textHashKey(src string) string { return "dund_text_sha256 " + src }

func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ingestText indexes text as the one fragment of the document src, now, with
// no job (raglit's Store.Ingest), after setting its tags (none: left as they
// are). For a memory note: its name or question and its detail, so its
// frontmatter is never a fragment. The text's hash is recorded for rag_sync.
func ingestText(st *raglit.Store, src, title, text string, tags []string) error {
	if len(tags) > 0 {
		if err := st.SetTags(src, tags); err != nil {
			return err
		}
	}
	if err := st.Ingest(context.Background(), raglit.Document{Path: src, Title: title,
		Fragments: []raglit.Fragment{{Page: 0, Ord: 0, Text: text}}}); err != nil {
		return err
	}
	return st.SetMeta(textHashKey(src), textHash(text), time.Now().Unix())
}

// removeDoc removes src from st, with the text hash ingestText recorded.
func removeDoc(st *raglit.Store, src string) error {
	if err := st.RemoveDocument(src, false); err != nil {
		return err
	}
	if _, ok := st.Meta(textHashKey(src)); ok {
		return st.SetMeta(textHashKey(src), "", time.Now().Unix())
	}
	return nil
}

// enqueue queues src on st with its title, after setting its tags (none:
// left as they are). SetTags on a path not yet committed holds the tags for
// the commit of this job; on a committed one it writes them now, and the
// re-ingest leaves them alone.
func enqueue(st *raglit.Store, src, title string, tags []string) (int64, error) {
	if len(tags) > 0 {
		if err := st.SetTags(src, tags); err != nil {
			return 0, err
		}
	}
	return st.Enqueue(src, title)
}

// Tag replaces one document's tags (raglit's SetTags), with no ingest. The
// index is created if needed, so the tags can be held for a first ingest.
func (h *Host) Tag(q proto.RagTag) (*proto.RagTagged, error) {
	name, err := Name(q.Project, q.Index)
	if err != nil {
		return nil, err
	}
	src, err := forgetSource(q.Path, q.URL)
	if err != nil {
		return nil, err
	}
	st, err := h.store(name, true)
	if err != nil {
		return nil, err
	}
	if err := st.SetTags(src, q.Tags); err != nil {
		return nil, err
	}
	return &proto.RagTagged{URL: src}, nil
}

// Forget removes one document from its index. An index or a document that
// does not exist is not an error: Removed says whether one was there.
func (h *Host) Forget(q proto.RagForget) (*proto.RagForgotten, error) {
	name, err := Name(q.Project, q.Index)
	if err != nil {
		return nil, err
	}
	src, err := forgetSource(q.Path, q.URL)
	if err != nil {
		return nil, err
	}
	out := &proto.RagForgotten{URL: src}
	st, err := h.store(name, false)
	if err != nil || st == nil {
		return out, err
	}
	docs, err := documents(st)
	if err != nil {
		return nil, err
	}
	if _, ok := docs[src]; !ok {
		return out, nil
	}
	if err := removeDoc(st, src); err != nil {
		return nil, err
	}
	out.Removed = true
	return out, nil
}

// documents is st's documents by path (raglit's Documents: one query).
func documents(st *raglit.Store) (map[string]raglit.DocSummary, error) {
	ds, err := st.Documents()
	if err != nil {
		return nil, err
	}
	out := make(map[string]raglit.DocSummary, len(ds))
	for _, d := range ds {
		out[d.Path] = d
	}
	return out, nil
}

// forgetSource is source for a document that may no longer be on disk.
func forgetSource(path, u string) (string, error) {
	switch {
	case path != "" && u != "":
		return "", errors.New("rag: a path or a URL, not both")
	case path != "":
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("rag: %q is not an absolute path", path)
		}
		return "file://" + filepath.Clean(path), nil
	case u != "":
		return u, nil
	}
	return "", errors.New("rag: forget and tag need a path or a URL")
}

// current reports whether d, the index's document (ok: there is one), holds
// path's file as it is now. With a recorded source hash (raglit's
// ContentHash: sha256 hex of the raw bytes), the bytes decide: a rewrite with
// the same bytes is current, and a change committed under a job that read
// the old bytes is not. Without one, the document must have been ingested
// (AddedAt) after the file last changed.
func current(d raglit.DocSummary, ok bool, path string) bool {
	if !ok || d.AddedAt == 0 {
		return false
	}
	if d.ContentHash != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		sum := sha256.Sum256(b)
		return d.ContentHash == hex.EncodeToString(sum[:])
	}
	fi, err := os.Stat(path)
	return err == nil && d.AddedAt >= fi.ModTime().UnixNano()
}

// Sync queues each of q.Files the index does not hold, or holds from
// before the file last changed, and removes each of q.Gone. Each step is
// idempotent, so a caller may send the whole set every time.
func (h *Host) Sync(q proto.RagSync) (*proto.RagSynced, error) {
	name, err := Name(q.Project, q.Index)
	if err != nil {
		return nil, err
	}
	out := &proto.RagSynced{}
	st, err := h.store(name, len(q.Files) > 0)
	if err != nil || st == nil {
		return out, err
	}
	docs, err := documents(st)
	if err != nil {
		return nil, err
	}
	for _, f := range q.Files {
		src, err := source(f.Path, "")
		if err != nil {
			return nil, err
		}
		d, ok := docs[src]
		var cur bool
		if f.Text != "" {
			// Text: the hash of what was ingested decides, not the file's
			// bytes, so a change to the frontmatter alone (important) is
			// current.
			have, _ := st.Meta(textHashKey(src))
			cur = ok && have == textHash(f.Text)
		} else {
			cur = current(d, ok, f.Path)
		}
		if cur {
			// Tags change without the bytes changing (RagTag while dund was
			// down): set them anyway, it writes one column.
			if len(f.Tags) > 0 {
				if err := st.SetTags(src, f.Tags); err != nil {
					return nil, err
				}
			}
			out.Current++
			continue
		}
		if f.Text != "" {
			err = ingestText(st, src, f.Title, f.Text, f.Tags)
		} else {
			_, err = enqueue(st, src, f.Title, f.Tags)
		}
		if err != nil {
			return nil, err
		}
		out.Queued++
	}
	for _, p := range q.Gone {
		src, err := forgetSource(p, "")
		if err != nil {
			return nil, err
		}
		if _, ok := docs[src]; !ok {
			continue
		}
		if err := removeDoc(st, src); err != nil {
			return nil, err
		}
		out.Removed++
	}
	return out, nil
}

// source is the URL a job reads: file:// for a local path, which must be
// absolute and a regular file.
func source(path, u string) (string, error) {
	switch {
	case path != "" && u != "":
		return "", errors.New("rag: a path or a URL, not both")
	case path != "":
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("rag: %q is not an absolute path", path)
		}
		fi, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("rag: %w", err)
		}
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("rag: %s is not a file", path)
		}
		return "file://" + filepath.Clean(path), nil
	case u != "":
		pu, err := url.Parse(u)
		if err != nil {
			return "", fmt.Errorf("rag: %w", err)
		}
		switch pu.Scheme {
		case "http", "https", "file":
			return u, nil
		}
		return "", fmt.Errorf("rag: %q: the URL is http, https or file", u)
	}
	return "", errors.New("rag: ingest needs a path or a URL")
}

// Jobs lists an index's jobs, newest first.
func (h *Host) Jobs(q proto.RagJobs) (*proto.RagJobList, error) {
	name, err := Name(q.Project, q.Index)
	if err != nil {
		return nil, err
	}
	out := &proto.RagJobList{Jobs: []proto.RagJob{}}
	st, err := h.store(name, false)
	if err != nil || st == nil {
		return out, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	js, err := st.Jobs(q.State, limit)
	if err != nil {
		return nil, err
	}
	for _, j := range js {
		out.Jobs = append(out.Jobs, proto.RagJob{ID: j.ID, URL: j.URL, Title: j.Title, State: j.State, Error: j.Error, Fragments: j.Fragments})
	}
	return out, nil
}

// Close drains both runners (running jobs get grace to finish, then are
// cut off and go back on the queue), cuts off the caption loops, waits for
// every runner's Done, and then closes every index. It reports whether every
// job stopped in time. A runner that is still not done after stopWait
// leaves the indexes open: closing one under a running job
// or caption is worse than a process that exits with them open.
func (h *Host) Close(grace time.Duration) bool {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return true
	}
	h.closed = true
	h.mu.Unlock()
	ok := true
	var wg sync.WaitGroup
	var okMu sync.Mutex
	for _, r := range h.runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !r.Drain(grace) {
				okMu.Lock()
				ok = false
				okMu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, r := range h.runners {
		r.CutOff() // the caption loop, which Drain leaves running
	}
	h.stopBg()
	h.fills.Wait()
	wait := time.After(stopWait)
	for _, r := range h.runners {
		select {
		case <-r.Done():
		case <-wait:
			h.logf("dund: rag: a runner did not stop within %s; the indexes are left open", stopWait)
			return false
		}
	}
	h.closeStores()
	return ok
}

func (h *Host) closeStores() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for n, st := range h.stores {
		_ = st.Close()
		delete(h.stores, n)
	}
}
