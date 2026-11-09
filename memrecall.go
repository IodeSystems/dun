package dun

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/iodesystems/agentkit/agent"

	"github.com/iodesystems/dun/proto"
)

// Memory recall through dund (docs/rag.md, slice 5).
//
// A note's file is the record (memory.go); dund's memories indexes, one per
// project and one global, are how it comes back. The proactive search asks
// both with the query docsPreparer builds, and the surfacing rule decides as
// it does for any group (notify.go): a hit carries its similarity and the
// query words it holds, so an index with embeddings runs the similarity rule
// and one without the coverage rule.
//
// dund down (a standalone `dun -p` with no dund, or dund running no
// retrieval): the memory tool still writes notes, and recall is a plain
// word search of the note files, under the coverage rule. The session says
// so once. Nothing written is lost: on the next contact with dund (the first
// call that reaches it, at the start and after any failure) the whole note
// set is sent as one rag_sync, which queues each note the index lacks or
// holds stale and removes the voided ones. Ingest is idempotent, so sending
// the whole set is safe.

// memLink is a session's way to its notes' indexes. It is the harness's for
// its life: a tool rebuild makes a new memoryStore over the same dirs, not a
// new link, so the sync runs once per contact, not once per rebuild.
type memLink struct {
	link    RagLink
	project string       // the project's slug in dund
	notice  func(string) // says, once, that recall is degraded

	mu      sync.Mutex
	synced  bool // a sync reached dund since the last failure
	syncing bool
	said    bool
	meta    map[string]memHit // per document path, from the latest searches
}

// memHit is what a memory search says about one note.
type memHit struct {
	meta    hitMeta
	meaning bool   // its index searches by meaning
	index   string // where it was found
}

func newMemLink(link RagLink, project string, notice func(string)) *memLink {
	return &memLink{link: link, project: project, notice: notice, meta: map[string]memHit{}}
}

// ragCallTimeout bounds a memory write's call to dund: it only queues.
const ragCallTimeout = 10 * time.Second

func (l *memLink) ingest(m *memoryStore, path string) error {
	n, _ := readNote(path)
	ctx, cancel := context.WithTimeout(context.Background(), ragCallTimeout)
	defer cancel()
	_, err := l.link.Ingest(ctx, proto.RagIngest{Project: m.scopeOf(path, l.project), Index: m.indexName(),
		Path: path, Title: noteTitle(path, n.meta), Tags: ragTags(n.meta)})
	l.saw(m, err)
	return err
}

// tag sends a note's tags to its index with no ingest (raglit's SetTags):
// for a change that leaves its text as it was, such as marking it
// important. The index keeps the document's old bytes; the next sync sees
// they differ from the file's and queues it once.
func (l *memLink) tag(m *memoryStore, path string) error {
	n, _ := readNote(path)
	ctx, cancel := context.WithTimeout(context.Background(), ragCallTimeout)
	defer cancel()
	_, err := l.link.Tag(ctx, proto.RagTag{Project: m.scopeOf(path, l.project), Index: m.indexName(),
		Path: path, Tags: ragTags(n.meta)})
	l.saw(m, err)
	return err
}

// ragTags is the tags a note is filed under in its index: its own, then
// kind:<kind> (kind:memory, kind:strategy, kind:constraint), then
// important when the user marked it. Tags are filter data, not text.
func ragTags(meta map[string]string) []string {
	out := append(noteTags(meta), "kind:"+noteKind(meta))
	if meta["important"] == "true" {
		out = append(out, "important")
	}
	return out
}

func (l *memLink) forget(m *memoryStore, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), ragCallTimeout)
	defer cancel()
	_, err := l.link.Forget(ctx, proto.RagForget{Project: m.scopeOf(path, l.project), Index: m.indexName(), Path: path})
	l.saw(m, err)
	return err
}

// saw records how a call to dund went: down marks the index behind, and a
// call that reached dund syncs the notes if it is.
func (l *memLink) saw(m *memoryStore, err error) {
	if errors.Is(err, ErrRagDown) {
		l.down()
		return
	}
	l.mu.Lock()
	start := !l.synced && !l.syncing
	if start {
		l.syncing = true
	}
	l.mu.Unlock()
	if start {
		go l.sync(m)
	}
}

func (l *memLink) down() {
	l.mu.Lock()
	l.synced = false
	say := !l.said
	l.said = true
	l.mu.Unlock()
	if say {
		msg := "memory: dund is not reachable — notes, strategies and constraints are saved on disk and recalled by a plain word search of them; they are indexed when dund is back"
		log.Printf("dun: %s", msg)
		if l.notice != nil {
			l.notice(msg)
		}
	}
}

// sync sends the note set to dund: every current note to queue if the index
// lacks it or holds it stale, every voided one to remove.
func (l *memLink) sync(m *memoryStore) {
	ok := l.syncOnce(m)
	l.mu.Lock()
	l.syncing = false
	if ok {
		l.synced = true
	}
	l.mu.Unlock()
}

// syncOnce reports whether dund was reached for every scope. A refusal (a
// project slug dund does not take) counts as reached: retrying it every
// pass would not change the answer.
func (l *memLink) syncOnce(m *memoryStore) bool {
	scopes := []struct{ project, dir string }{{l.project, m.dir}}
	if m.global != "" {
		scopes = append(scopes, struct{ project, dir string }{"", m.global})
	}
	for _, sc := range scopes {
		q := proto.RagSync{Project: sc.project, Index: m.indexName()}
		notes, _ := filepath.Glob(filepath.Join(sc.dir, "*.md"))
		for _, p := range notes {
			n, err := readNote(p)
			if err != nil {
				continue
			}
			q.Files = append(q.Files, proto.RagSyncFile{Path: p, Title: noteTitle(p, n.meta), Tags: ragTags(n.meta)})
		}
		voided, _ := filepath.Glob(filepath.Join(sc.dir, "*.md"+voidSuffix))
		for _, p := range voided {
			q.Gone = append(q.Gone, strings.TrimSuffix(p, voidSuffix))
		}
		if len(q.Files) == 0 && len(q.Gone) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), ragReplyTimeout)
		r, err := l.link.Sync(ctx, q)
		cancel()
		switch {
		case errors.Is(err, ErrRagDown):
			l.down()
			return false
		case err != nil:
			log.Printf("dun: memory: sync of %s: %v", sc.dir, err)
		case r.Queued > 0 || r.Removed > 0:
			log.Printf("dun: memory: %s: %d notes queued for the index, %d removed, %d current", sc.dir, r.Queued, r.Removed, r.Current)
		}
	}
	return true
}

// memFinder is the proactive search's finder for one store's files: the
// memory notes, or the strategies and constraints. skip (may be nil) leaves
// out a file the standing list already shows (notekinds.go).
type memFinder struct {
	l    *memLink
	m    *memoryStore
	skip func(path string) bool
}

// memSearchLimit is the hits asked of each memories index per pass.
const memSearchLimit = 8

func (f *memFinder) Find(ctx context.Context, texts []string) ([]agent.DocHit, error) {
	q := strings.TrimSpace(strings.Join(texts, " "))
	if q == "" {
		return nil, nil
	}
	type result struct {
		index string
		hits  *proto.RagHits
		err   error
	}
	scopes := []string{f.l.project}
	if f.m.global != "" {
		scopes = append(scopes, "")
	}
	res := make([]result, len(scopes))
	var wg sync.WaitGroup
	for i, p := range scopes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := p
			if name == "" {
				name = "global"
			}
			r, err := f.l.link.Search(ctx, proto.RagSearch{Project: p, Index: f.m.indexName(), Query: q, Limit: memSearchLimit})
			res[i] = result{index: name + "/" + f.m.indexName(), hits: r, err: err}
		}()
	}
	wg.Wait()
	down, reached := 0, false
	var firstErr error
	for _, r := range res {
		switch {
		case errors.Is(r.err, ErrRagDown):
			down++
		case r.err != nil:
			reached = true
			if firstErr == nil {
				firstErr = r.err
			}
		default:
			reached = true
		}
	}
	if !reached {
		f.l.down()
		return f.local(q), nil
	}
	f.l.saw(f.m, nil)
	var out []agent.DocHit
	seen := map[string]int{}
	ok := false
	for _, r := range res {
		if r.err != nil {
			continue
		}
		ok = true
		meaning := r.hits.Mode == "hybrid"
		for _, h := range r.hits.Hits {
			path := strings.TrimPrefix(h.Path, "file://")
			if !f.m.holds(path) {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				continue // voided or gone, and the index not yet told
			}
			if f.skip != nil && f.skip(path) {
				continue // in the standing list of every request already
			}
			hm := hitMeta{Matched: h.Matched, Terms: h.Terms}
			if h.Similarity != nil {
				hm.Sim, hm.HasSim = *h.Similarity, true
			}
			// One hit per note. A tagged note has two fragments, its text
			// and the caption its tags are written as (internal/ragd), so a
			// search can find it twice; its measures are the best of them.
			if i, dup := seen[path]; dup {
				f.l.merge(path, hm)
				if h.Score > out[i].Score {
					out[i].Score = h.Score
				}
				continue
			}
			seen[path] = len(out)
			f.l.record(path, memHit{meta: hm, meaning: meaning, index: r.index})
			title := h.Title
			if title == "" {
				title = filepath.Base(path)
			}
			line, _, _ := strings.Cut(strings.TrimSpace(h.Text), "\n")
			out = append(out, agent.DocHit{DocID: path, Title: title, Score: h.Score, Line: line})
		}
	}
	if !ok {
		return nil, firstErr
	}
	return out, nil
}

func (l *memLink) record(path string, h memHit) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.meta[path] = h
}

// merge folds another fragment's measures into a note's: the higher
// similarity, the most query words.
func (l *memLink) merge(path string, m hitMeta) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.meta[path]
	if m.HasSim && (!h.meta.HasSim || m.Sim > h.meta.Sim) {
		h.meta.Sim, h.meta.HasSim = m.Sim, true
	}
	if m.Matched > h.meta.Matched {
		h.meta.Matched = m.Matched
	}
	if m.Terms > h.meta.Terms {
		h.meta.Terms = m.Terms
	}
	l.meta[path] = h
}

func (l *memLink) hit(path string) (memHit, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.meta[path]
	return h, ok
}

// localIndex names a hit of the search on disk.
const localIndex = "notes on disk (dund down)"

// local is recall with dund down: each note scored by how many of the
// query's words it holds (its question, text and tags), for the coverage
// rule. No ranking by meaning, no stemming.
func (f *memFinder) local(q string) []agent.DocHit {
	terms := queryTerms(q)
	if len(terms) == 0 {
		return nil
	}
	var out []agent.DocHit
	for _, d := range f.m.dirs() {
		paths, _ := filepath.Glob(filepath.Join(d, "*.md"))
		for _, p := range paths {
			if f.skip != nil && f.skip(p) {
				continue
			}
			n, err := readNote(p)
			if err != nil {
				continue
			}
			words := map[string]bool{}
			for _, w := range wordsOf(n.meta["question"] + " " + n.meta["name"] + " " + n.meta["tags"] + " " + n.body) {
				words[w] = true
			}
			matched := 0
			for _, t := range terms {
				if words[t] {
					matched++
				}
			}
			if matched == 0 {
				continue
			}
			f.l.record(p, memHit{meta: hitMeta{Matched: matched, Terms: len(terms)}, index: localIndex})
			title := noteTitle(p, n.meta)
			line, _, _ := strings.Cut(n.body, "\n")
			out = append(out, agent.DocHit{DocID: p, Title: title, Score: float64(matched), Line: line})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// stopWords are left out of a local query, as raglit leaves them out of its
// term counts: a note does not match "how do I" by holding "how".
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be but by can could did do does for from had has have how
		i if in is it its me my no not of on or our should so than that the their them then there these they this
		to us was we were what when where which who why will with would you your`) {
		stopWords[w] = true
	}
}

func wordsOf(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// queryTerms is the distinct words of q that count: two letters or more,
// not a stop word.
func queryTerms(q string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range wordsOf(q) {
		if len([]rune(w)) < 2 || stopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// fetchNote reads a note whole for inlining: the file, when it is at most
// max chars.
func fetchNote(path string, max int) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 || len(b) > max {
		return "", false
	}
	return string(b), true
}

// memLink is the harness's link for its notes, nil without a RagLink. A
// child shares its parent's, so a sub-agent does not sync the notes again.
func (h *Harness) memLink() *memLink {
	h.memLinkOnce.Do(func() {
		h.memLinkV = h.newStoreLink(func(p *Harness) *memLink { return p.memLink() }, memoryFor, h.cfg.OnNotify)
	})
	return h.memLinkV
}

// stratLink is the same for the strategies and constraints (notekinds.go):
// its own link, so each index syncs on its own. It gives no notice: the
// notes' link says dund is down for both.
func (h *Harness) stratLink() *memLink {
	h.stratLinkOnce.Do(func() {
		h.stratLinkV = h.newStoreLink(func(p *Harness) *memLink { return p.stratLink() }, strategyFor, nil)
	})
	return h.stratLinkV
}

func (h *Harness) newStoreLink(parent func(*Harness) *memLink, store func(*memLink, string) *memoryStore, notice func(string)) *memLink {
	if h.cfg.Rag == nil {
		return nil
	}
	if p := h.cfg.Parent; p != nil {
		if l := parent(p); l != nil {
			return l
		}
	}
	l := newMemLink(h.cfg.Rag, raglitProject(h.cfg.Workspace), notice)
	// The first contact: files written while no dund was there get
	// indexed now, or the session learns it is degraded.
	go l.saw(store(l, h.cfg.Workspace), nil)
	return l
}

// withMemFinder adds the memory finder to the proactive search: its hits
// beside the docs server's (inner, nil without one), read whole from their
// files, and described from what dund said of them. src is changed in
// place to look a note up here first.
func withMemFinder(mf *memFinder, inner agent.DocFinder, fetch docBodyFunc, src *hitSources) (agent.DocFinder, docBodyFunc) {
	index, meta, meaning := src.index, src.meta, src.meaning
	src.index = func(docID string) string {
		if h, ok := mf.l.hit(docID); ok && mf.m.holds(docID) {
			return h.index
		}
		if index != nil {
			return index(docID)
		}
		return ""
	}
	src.meta = func(docID string) (hitMeta, bool) {
		if h, ok := mf.l.hit(docID); ok && mf.m.holds(docID) {
			return h.meta, true
		}
		if meta != nil {
			return meta(docID)
		}
		return hitMeta{}, false
	}
	src.meaning = func(docID string) (bool, bool) {
		if h, ok := mf.l.hit(docID); ok && mf.m.holds(docID) {
			return h.meaning, true
		}
		if meaning != nil {
			return meaning(docID)
		}
		return false, false
	}
	readAll := func(ctx context.Context, docID string, max int) (string, bool) {
		if mf.m.holds(docID) {
			return fetchNote(docID, max)
		}
		if fetch != nil {
			return fetch(ctx, docID, max)
		}
		return "", false
	}
	if inner == nil {
		return mf, readAll
	}
	return bothFinders{mf, inner}, readAll
}

// bothFinders asks the memory finder and the docs server's at once. One
// failing leaves the other's hits; both failing is the docs server's error.
type bothFinders struct {
	mem, docs agent.DocFinder
}

func (b bothFinders) Find(ctx context.Context, texts []string) ([]agent.DocHit, error) {
	var memHits []agent.DocHit
	var memErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		memHits, memErr = b.mem.Find(ctx, texts)
	}()
	docHits, docErr := b.docs.Find(ctx, texts)
	<-done
	if memErr != nil && docErr != nil {
		return nil, docErr
	}
	return append(memHits, docHits...), nil
}
