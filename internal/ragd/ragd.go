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
// the embed model its vectors came from (raglit's index_meta embed_model)
// and their dimension (index_meta embed_dim, dund's key). An index whose
// record differs from the embed role gets no embedder: no vector is written
// to it and it is searched by text only, until Reembed.
package ragd

import (
	"context"
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

// Host owns every index and the job runner. It is a raglit.StoreSet.
type Host struct {
	root   string
	models raglit.Models
	embed  *raglit.Embedder // nil: FTS only
	dim    int              // the embed model's probed dimension; 0 when not probed
	ident  *raglit.Identifier
	runner *raglit.Runner
	logf   func(string, ...any)

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

// metaEmbedDim is the index_meta key of the vectors' dimension. raglit
// records the model (embed_model) but not the dimension.
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
	h.runner = &raglit.Runner{
		Stores: h,
		NewWorker: func(s *raglit.Store) (*raglit.Worker, error) {
			return raglit.NewWorker(s, h.models, nil), nil
		},
		Logf:  logf,
		Notef: func(f string, a ...any) { logf("dund: rag: "+f, a...) },
	}
	h.runner.Start(context.Background())
	return h, nil
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
	if have == "" {
		if _, err := st.SyncEmbedModel(h.embed.Model); err != nil {
			h.logf("dund: rag: %s: embed model record: %v", name, err)
		}
	}
	if haveDim == 0 && h.dim > 0 {
		if err := st.SetMeta(metaEmbedDim, strconv.Itoa(h.dim), time.Now().Unix()); err != nil {
			h.logf("dund: rag: %s: embed dimension record: %v", name, err)
		}
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

// Reembed brings an index's vectors to the embed role: vectors from another
// model are dropped, the record is set to the role's model and dimension, the
// embedder is attached, and the fragments with no vector are embedded in the
// background.
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
	// raglit's SetEmbedder is a plain field write that a running job's
	// worker reads (raglit-a4 has the fix), so the embedder is not swapped
	// under one.
	if running, err := st.Jobs("running", 0); err != nil {
		return nil, err
	} else if len(running) > 0 {
		flags := "--index " + q.Index
		if q.Project != "" {
			flags = "--project " + q.Project + " " + flags
		}
		return nil, fmt.Errorf("rag: %s has %d running ingest job(s): reembed when they end (`dun dund rag jobs %s --state running`)",
			name, len(running), flags)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errors.New("rag: dund is stopping")
	}
	if st.EmbedModel() == h.embed.Model {
		if d := metaInt(st, metaEmbedDim); d > 0 && h.dim > 0 && d != h.dim {
			// SyncEmbedModel drops vectors only for another model id.
			return nil, fmt.Errorf("rag: %s: vectors of dimension %d from %s, which now gives %d: raglit has no call that drops vectors of the same model", name, d, h.embed.Model, h.dim)
		}
	}
	dropped, err := st.SyncEmbedModel(h.embed.Model)
	if err != nil {
		return nil, err
	}
	dim := ""
	if h.dim > 0 {
		dim = strconv.Itoa(h.dim)
	}
	if err := st.SetMeta(metaEmbedDim, dim, time.Now().Unix()); err != nil {
		return nil, err
	}
	delete(h.stale, name)
	st.SetEmbedder(h.embed)
	missing, err := st.MissingVectors()
	if err != nil {
		return nil, err
	}
	if dropped {
		h.logf("dund: rag: %s: dropped vectors from another model; embedding %d fragments with %s", name, missing, h.embed.Model)
	}
	h.backfillLocked(name, st)
	return &proto.RagReembedding{Index: name, Model: h.embed.Model, Dim: h.dim, Dropped: dropped, Missing: missing}, nil
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
		out.Hits = append(out.Hits, proto.RagHit{Path: x.Path, Title: x.Title, Page: x.Page, Text: x.Text, Score: x.Score})
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
	if len(q.Tags) > 0 {
		// The one way the library takes tags from its caller: a supplied
		// caption, consumed by the document's next commit.
		cap := q.Title
		if cap == "" {
			cap = filepath.Base(src)
		}
		if err := st.SupplyIdentity(src, raglit.DocIdentity{Name: cap, ContentTags: q.Tags}, "dund"); err != nil {
			return nil, err
		}
	}
	id, err := st.Enqueue(src, q.Title)
	if err != nil {
		return nil, err
	}
	return &proto.RagIngested{Job: id, URL: src}, nil
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

// Close drains the runner (running jobs get grace to finish, then are cut
// off and go back on the queue) and closes every index. It reports whether
// every job stopped in time.
func (h *Host) Close(grace time.Duration) bool {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return true
	}
	h.closed = true
	h.mu.Unlock()
	ok := h.runner.Drain(grace)
	h.runner.CutOff() // the identity queue, which Drain leaves running
	h.stopBg()
	h.fills.Wait()
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
