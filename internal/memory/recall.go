package memory

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/iodesystems/agentkit/agent"

	"github.com/iodesystems/dun/internal/raglit"
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
// dund down (a standalone `dun --engine` with no dund, or dund running no
// retrieval): the memory tool still writes notes, and recall is a plain
// word search of the note files, under the coverage rule. The session says
// so once. Nothing written is lost: on the next contact with dund (the first
// call that reaches it, at the start and after any failure) the whole note
// set is sent as one rag_sync, which queues each note the index lacks or
// holds stale and removes the voided ones. Ingest is idempotent, so sending
// the whole set is safe.

// Link is a session's way to its notes' indexes. It is the harness's for
// its life: a tool rebuild makes a new Store over the same dirs, not a
// new link, so the sync runs once per contact, not once per rebuild.
//
// When the session has a registered remote project (raglit), ingest also
// uploads each note to the private layer, or the public layer when the
// note's frontmatter says public: true. The layer is the cross-machine
// record; dund's index is the local search. A layer upload failure is
// logged and the dund ingest proceeds: the file is the record, the layer
// catches up on the next contact.
type Link struct {
	rag     Rag
	project string       // the project's slug in dund
	notice  func(string) // says, once, that recall is degraded

	// raglitClient and raglitReg are set when the session has a registered
	// remote project (the registration slice, on /rag on). nil client → no
	// remote layer: notes are local only.
	raglitClient raglitLayer
	raglitReg    raglit.Registration

	mu      sync.Mutex
	synced  bool // a sync reached dund since the last failure
	syncing bool
	said    bool
	meta    map[string]Hit // per document path, from the latest searches
}

// Rag is the way to dund's indexes the notes go into (harness.RagLink). An
// error that wraps ErrDown means dund is not there, or runs no retrieval:
// the notes degrade to the files on disk, they do not fail.
type Rag interface {
	Search(ctx context.Context, q proto.RagSearch) (*proto.RagHits, error)
	Ingest(ctx context.Context, q proto.RagIngest) (*proto.RagIngested, error)
	Forget(ctx context.Context, q proto.RagForget) (*proto.RagForgotten, error)
	Sync(ctx context.Context, q proto.RagSync) (*proto.RagSynced, error)
	Tag(ctx context.Context, q proto.RagTag) (*proto.RagTagged, error)
}

// ErrDown is a Rag's error when dund cannot be reached, or answers that it
// runs no retrieval (harness.ErrRagDown is this error).
var ErrDown = errors.New("dund is not reachable")

// syncTimeout bounds a rag_sync of the note set: harness's ragReplyTimeout.
const syncTimeout = 30 * time.Second

// Measures is what raglit says about a hit besides its score: the
// harness's Measures, field for field.
type Measures struct {
	Sim     float64
	HasSim  bool
	Matched int
	Terms   int
}

// Hit is what a memory search says about one note.
type Hit struct {
	Meta    Measures
	Meaning bool   // its index searches by meaning
	Index   string // where it was found
}

// NewLink is a session's link to its notes' indexes in dund, through rag.
// notice (may be nil) is told, once, that recall is degraded.
func NewLink(rag Rag, project string, notice func(string)) *Link {
	return &Link{rag: rag, project: project, notice: notice, meta: map[string]Hit{}}
}

// raglitLayer is the raglit daemon's projects API, as the memory notes use
// it: a document to a layer, and a document out of one. *raglit.Client
// implements it; a test fake implements it without a daemon.
type raglitLayer interface {
	Upload(ctx context.Context, index string, doc raglit.UploadDoc) (raglit.UploadResult, error)
	Forget(ctx context.Context, index, path string) error
}

// NewLinkWithRaglit is like NewLink, but also uploads each note to the
// registered project's private or public layer on ingest, and drops it from
// the layer on forget. A nil client is the same as NewLink: no remote layer.
func NewLinkWithRaglit(rag Rag, project string, notice func(string), rc raglitLayer, reg raglit.Registration) *Link {
	l := NewLink(rag, project, notice)
	l.raglitClient = rc
	l.raglitReg = reg
	return l
}

// Contact is the first contact with dund: files written while no dund was
// there get indexed now, or the session learns it is degraded.
func (l *Link) Contact(m *Store) { l.saw(m, nil) }

// ragCallTimeout bounds a memory write's call to dund: it only queues.
const ragCallTimeout = 10 * time.Second

func (l *Link) ingest(m *Store, path string) error {
	n, _ := readNote(path)
	ctx, cancel := context.WithTimeout(context.Background(), ragCallTimeout)
	defer cancel()
	_, err := l.rag.Ingest(ctx, proto.RagIngest{Project: m.scopeOf(path, l.project), Index: m.indexName(),
		Path: path, Title: noteTitle(path, n.meta), Tags: ragTags(n.meta), Text: noteText(path, n)})
	l.saw(m, err)
	l.raglitUpload(path, n)
	return err
}

// raglitUpload sends a note to the registered project's layer: public to
// reg.Public, private to reg.Private (the default — leak is irreversible,
// so a note only leaves the user's own layer when public: true). On success
// the daemon-assigned path is recorded in the note's frontmatter as
// raglit_path, so a later void can forget the right document. A failure is
// logged and not returned: the file is the record, the layer catches up on
// the next contact (same down-note semantics as dund).
func (l *Link) raglitUpload(path string, n note) {
	if l.raglitClient == nil || l.raglitReg.Private == "" {
		return
	}
	layer := l.raglitReg.Private
	if n.meta["public"] == "true" {
		layer = l.raglitReg.Public
	}
	name := filepath.Base(path)
	ctx, cancel := context.WithTimeout(context.Background(), ragCallTimeout)
	defer cancel()
	res, err := l.raglitClient.Upload(ctx, layer, raglit.UploadDoc{
		Name:  name,
		Text:  noteText(path, n),
		Title: noteTitle(path, n.meta),
	})
	if err != nil {
		log.Printf("dun: memory: %s not uploaded to %s: %v", name, layer, err)
		return
	}
	setNoteField(path, "raglit_path", res.Path)
}

// setNoteField sets one scalar field in a note's frontmatter, keeping every
// other line, and writes the file back. A failure is logged, not returned:
// the field is for the layer (raglit_path), and the layer catches up.
func setNoteField(path, key, value string) {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("dun: memory: %s: %v", filepath.Base(path), err)
		return
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 2 || lines[0] != "---" {
		return
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			for j, l := range lines[1:i] {
				if k, _, _ := strings.Cut(l, ":"); k == key {
					lines[j+1] = key + ": " + value
					if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
						log.Printf("dun: memory: %s: %v", filepath.Base(path), err)
					}
					return
				}
			}
			lines = append(append(append([]string{}, lines[:i]...), key+": "+value), lines[i:]...)
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
				log.Printf("dun: memory: %s: %v", filepath.Base(path), err)
			}
			return
		}
	}
}

// noteText is what a note is indexed as: a memory's question and answer, or
// an item's name and detail. The frontmatter is not: its metadata goes as
// tags (ragTags), so a word in the name and the detail is one hit.
func noteText(path string, n note) string {
	head := noteNameOf(path, n.meta)
	if noteKind(n.meta) == KindMemory {
		head = n.meta["question"]
	}
	return strings.TrimSpace(head + "\n\n" + strings.TrimSpace(n.body))
}

// tag sends a note's tags to its index with no ingest (raglit's SetTags):
// for a change that leaves its text as it was, such as marking it
// important. Its indexed text (noteText) is unchanged, so the next sync
// finds it current.
func (l *Link) tag(m *Store, path string) error {
	n, _ := readNote(path)
	ctx, cancel := context.WithTimeout(context.Background(), ragCallTimeout)
	defer cancel()
	_, err := l.rag.Tag(ctx, proto.RagTag{Project: m.scopeOf(path, l.project), Index: m.indexName(),
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

func (l *Link) forget(m *Store, path string) error {
	n, _ := readNote(path)
	ctx, cancel := context.WithTimeout(context.Background(), ragCallTimeout)
	defer cancel()
	_, err := l.rag.Forget(ctx, proto.RagForget{Project: m.scopeOf(path, l.project), Index: m.indexName(), Path: path})
	l.saw(m, err)
	l.raglitForget(path, n.meta["raglit_path"])
	return err
}

// raglitForget drops a note from the registered project's layer on a void.
// path is the daemon-assigned path in the note's frontmatter, set by
// raglitUpload; "" (the upload never succeeded) means the layer holds
// nothing to drop. A failure is logged and not returned: the file is the
// record, and a doc left in a layer is found only by search.
func (l *Link) raglitForget(path, layerPath string) {
	if l.raglitClient == nil || layerPath == "" {
		return
	}
	n, _ := readNote(path)
	layer := l.raglitReg.Private
	if n.meta["public"] == "true" {
		layer = l.raglitReg.Public
	}
	ctx, cancel := context.WithTimeout(context.Background(), ragCallTimeout)
	defer cancel()
	if err := l.raglitClient.Forget(ctx, layer, layerPath); err != nil {
		log.Printf("dun: memory: %s not forgotten from %s: %v", filepath.Base(path), layer, err)
	}
}

// saw records how a call to dund went: down marks the index behind, and a
// call that reached dund syncs the notes if it is.
func (l *Link) saw(m *Store, err error) {
	if errors.Is(err, ErrDown) {
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

func (l *Link) down() {
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
func (l *Link) sync(m *Store) {
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
func (l *Link) syncOnce(m *Store) bool {
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
			q.Files = append(q.Files, proto.RagSyncFile{Path: p, Title: noteTitle(p, n.meta), Tags: ragTags(n.meta), Text: noteText(p, n)})
		}
		voided, _ := filepath.Glob(filepath.Join(sc.dir, "*.md"+voidSuffix))
		for _, p := range voided {
			q.Gone = append(q.Gone, strings.TrimSuffix(p, voidSuffix))
		}
		// The legacy directory (the notes' home before the project id keyed
		// it) is gone too, so the index holds each note at its new path,
		// not at both. A voided note is its file with .void on it.
		if sc.project != "" && m.legacy != "" {
			for _, pat := range []string{"*.md", "*.md" + voidSuffix} {
				old, _ := filepath.Glob(filepath.Join(m.legacy, pat))
				for _, p := range old {
					q.Gone = append(q.Gone, strings.TrimSuffix(p, voidSuffix))
				}
			}
		}
		if len(q.Files) == 0 && len(q.Gone) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		r, err := l.rag.Sync(ctx, q)
		cancel()
		switch {
		case errors.Is(err, ErrDown):
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

// Finder is the proactive search's finder for one store's files: the
// memory notes, or the strategies and constraints. skip (may be nil) leaves
// out a file the standing list already shows (kinds.go).
// Candidates (lesson.go) are left out unless Candidates is set: a tag
// filter in dun, after the search (raglit's filter keeps tags; it has no
// way to exclude one). raglit is asked for an exclude-tag filter (NotTags,
// USER 2026-11-09); when dund passes it, the search can leave candidates
// out and stop spending its hits on them. Keep this drop until then.
type Finder struct {
	l          *Link
	m          *Store
	skip       func(path string) bool
	Candidates bool
}

// NewFinder is the finder for m's files through l; skip may be nil.
func NewFinder(l *Link, m *Store, skip func(path string) bool) *Finder {
	return &Finder{l: l, m: m, skip: skip}
}

// Hit is what the latest search said of docID, when it is one of the
// store's files.
func (f *Finder) Hit(docID string) (Hit, bool) {
	if h, ok := f.l.hit(docID); ok && f.m.Holds(docID) {
		return h, true
	}
	return Hit{}, false
}

// Holds reports whether docID is one of the store's files.
func (f *Finder) Holds(docID string) bool { return f.m.Holds(docID) }

// memSearchLimit is the hits asked of each memories index per pass.
const memSearchLimit = 8

func (f *Finder) Find(ctx context.Context, texts []string) ([]agent.DocHit, error) {
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
			r, err := f.l.rag.Search(ctx, proto.RagSearch{Project: p, Index: f.m.indexName(), Query: q, Limit: memSearchLimit})
			res[i] = result{index: name + "/" + f.m.indexName(), hits: r, err: err}
		}()
	}
	wg.Wait()
	down, reached := 0, false
	var firstErr error
	for _, r := range res {
		switch {
		case errors.Is(r.err, ErrDown):
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
			if !f.m.Holds(path) {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				continue // voided or gone, and the index not yet told
			}
			if f.skip != nil && f.skip(path) {
				continue // in the standing list of every request already
			}
			if !f.Candidates && IsCandidate(path) {
				continue // proposed, not accepted (lesson.go)
			}
			hm := Measures{Matched: h.Matched, Terms: h.Terms}
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
			f.l.record(path, Hit{Meta: hm, Meaning: meaning, Index: r.index})
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

func (l *Link) record(path string, h Hit) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.meta[path] = h
}

// merge folds another fragment's measures into a note's: the higher
// similarity, the most query words.
func (l *Link) merge(path string, m Measures) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.meta[path]
	if m.HasSim && (!h.Meta.HasSim || m.Sim > h.Meta.Sim) {
		h.Meta.Sim, h.Meta.HasSim = m.Sim, true
	}
	if m.Matched > h.Meta.Matched {
		h.Meta.Matched = m.Matched
	}
	if m.Terms > h.Meta.Terms {
		h.Meta.Terms = m.Terms
	}
	l.meta[path] = h
}

func (l *Link) hit(path string) (Hit, bool) {
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
func (f *Finder) local(q string) []agent.DocHit {
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
			if err != nil || (!f.Candidates && slices.Contains(noteTags(n.meta), CandidateTag)) {
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
			f.l.record(p, Hit{Meta: Measures{Matched: matched, Terms: len(terms)}, Index: localIndex})
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
