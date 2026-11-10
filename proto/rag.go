package proto

// Retrieval in dund (docs/rag.md, slice 2): dund hosts raglit's engine and
// keeps four indexes per project (code, docs, memories, strategies) and two
// global ones (memories, strategies). These commands go to dund itself, not
// to a session: they carry no session id.

func init() {
	command[RagSearch]("rag_search")
	command[RagIngest]("rag_ingest")
	command[RagJobs]("rag_jobs")
	command[RagReembed]("rag_reembed")
	command[RagForget]("rag_forget")
	command[RagSync]("rag_sync")
	command[RagTag]("rag_tag")
	command[RagIndex]("rag_index")
	command[RagGC]("rag_gc")
	event[RagHits]("rag_hits")
	event[RagIngested]("rag_ingested")
	event[RagJobList]("rag_jobs")
	event[RagReembedding]("rag_reembedding")
	event[RagForgotten]("rag_forgotten")
	event[RagSynced]("rag_synced")
	event[RagTagged]("rag_tagged")
	event[RagIndexed]("rag_indexed")
	event[RagGCed]("rag_gced")
}

// RagFilter limits a search before ranking. Every constraint must hold.
// PathPrefix compares with the document's path, which is its source URL
// (file:///abs/path for a local file). Tags match whole tags, after the same
// normalization as on write (lower case, spaces collapsed). Fields match
// extracted string fields exactly.
type RagFilter struct {
	PathPrefix string            `json:"path_prefix,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	Fields     map[string]string `json:"fields,omitempty"`
}

// RagSearch searches one index. Project is the project's slug (dun's
// raglitProject); empty is the global set. Mode is "hybrid" (the default:
// FTS and vectors, fused) or "fts"; hybrid with no embed model runs as fts.
// Root is the workspace the search is for (absolute): when it is a linked
// git worktree, a code or docs search reads the worktree's branch of the
// index (rag_index on it made one), and none when there is no branch yet.
// Answered with RagHits.
type RagSearch struct {
	Project string    `json:"project,omitempty"`
	Index   string    `json:"index"`
	Query   string    `json:"query"`
	Filter  RagFilter `json:"filter,omitzero"`
	Limit   int       `json:"limit,omitempty"`
	Mode    string    `json:"mode,omitempty"`
	Root    string    `json:"root,omitempty"`
}

// RagHit is one fragment a search found. Similarity is the cosine of the
// query and fragment vectors, nil when the vector side did not rank it (an
// FTS search, or a hybrid hit outside the vector pool); the score of a
// hybrid search is a fused rank and sets no bar. Matched is how many of
// the query's Terms (after stopwords) the fragment holds.
type RagHit struct {
	Path       string   `json:"path"`
	Title      string   `json:"title,omitempty"`
	Page       int      `json:"page,omitempty"`
	Text       string   `json:"text"`
	Score      float64  `json:"score"`
	Similarity *float64 `json:"similarity,omitempty"`
	Matched    int      `json:"matched,omitempty"`
	Terms      int      `json:"terms,omitempty"`
	// StartLine and EndLine are the fragment's half-open 1-based line span in
	// its source; absent when unknown.
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
	ContentID string `json:"content_id,omitempty"`
}

// RagHits answers RagSearch. Mode is the mode that ran; Note says why it is
// not the one asked for.
type RagHits struct {
	Mode string   `json:"mode"`
	Note string   `json:"note,omitempty"`
	Hits []RagHit `json:"hits"`
}

// RagIngest queues one source for an index: Path (a local file, absolute) or
// URL (http, https or file). Tags, when given, replace the document's tags
// (raglit's SetTags: filter data, not text); none leaves them as they are.
// Text, when given (with Path), is the document: one fragment, indexed at
// once with no job (Job is 0); the file is not read. Answered with
// RagIngested.
type RagIngest struct {
	Project string   `json:"project,omitempty"`
	Index   string   `json:"index"`
	Path    string   `json:"path,omitempty"`
	URL     string   `json:"url,omitempty"`
	Title   string   `json:"title,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Text    string   `json:"text,omitempty"`
}

// RagIngested answers RagIngest: the job and the URL it reads.
type RagIngested struct {
	Job int64  `json:"job"`
	URL string `json:"url"`
}

// RagJobs lists an index's ingest jobs, newest first. State filters
// (pending, running, done, error); empty is all.
type RagJobs struct {
	Project string `json:"project,omitempty"`
	Index   string `json:"index"`
	State   string `json:"state,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// RagJob is one ingest job.
type RagJob struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	Title     string `json:"title,omitempty"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	Fragments int    `json:"fragments,omitempty"`
}

// RagJobList answers RagJobs.
type RagJobList struct {
	Jobs []RagJob `json:"jobs"`
}

// RagReembed rebuilds an index's vectors with the embed model dund is
// configured with (facets.embed). An index records the embed model and
// dimension its vectors came from; when they differ from the configured
// one, dund searches that index by text only and writes no vectors to it
// until this runs. Every vector is dropped, then every fragment is
// embedded again in the background. Answered with RagReembedding.
type RagReembed struct {
	Project string `json:"project,omitempty"`
	Index   string `json:"index"`
}

// RagReembedding answers RagReembed: the model the index now records,
// whether its old vectors were dropped, and how many fragments wait for a
// vector (the background work).
type RagReembedding struct {
	Index   string `json:"index"`
	Model   string `json:"model"`
	Dim     int    `json:"dim,omitempty"`
	Dropped bool   `json:"dropped,omitempty"`
	Missing int    `json:"missing"`
}

// RagForget removes one document from an index: Path is the local file it
// was ingested from (absolute), or URL its source. Answered with
// RagForgotten; a document the index does not hold is not an error.
type RagForget struct {
	Project string `json:"project,omitempty"`
	Index   string `json:"index"`
	Path    string `json:"path,omitempty"`
	URL     string `json:"url,omitempty"`
}

// RagForgotten answers RagForget: whether the index held the document.
type RagForgotten struct {
	URL     string `json:"url"`
	Removed bool   `json:"removed"`
}

// RagSync brings an index in step with a set of local files (dun's memory
// notes, which are written while dund may be down). Each of Files is
// queued when the index does not hold it or holds other bytes (its source
// hash; without one, the file changed after the document was indexed). A
// file with Text is ingested at once when the index lacks it or holds
// other text (the hash dund recorded of the text it ingested); Queued
// counts both.
// the tags of each, current or not, are set as RagTag sets them. Each of
// Gone (absolute paths) is removed. Answered with RagSynced.
type RagSync struct {
	Project string        `json:"project,omitempty"`
	Index   string        `json:"index"`
	Files   []RagSyncFile `json:"files,omitempty"`
	Gone    []string      `json:"gone,omitempty"`
}

// RagSyncFile is one file of a RagSync, with the title, tags and text it
// is ingested with (as RagIngest).
type RagSyncFile struct {
	Path  string   `json:"path"`
	Title string   `json:"title,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Text  string   `json:"text,omitempty"`
}

// RagSynced answers RagSync: how many files were queued, how many were
// already current, and how many gone ones were removed.
type RagSynced struct {
	Queued  int `json:"queued"`
	Current int `json:"current"`
	Removed int `json:"removed"`
}

// RagTag replaces one document's tags without ingesting it again: Path is
// the local file it was ingested from (absolute), or URL its source. No
// tags clears them. On a document the index does not hold yet, the tags
// are held and its next ingest applies them. Answered with RagTagged.
type RagTag struct {
	Project string   `json:"project,omitempty"`
	Index   string   `json:"index"`
	Path    string   `json:"path,omitempty"`
	URL     string   `json:"url,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// RagTagged answers RagTag: the document's source URL.
type RagTagged struct {
	URL string `json:"url"`
}

// RagIndex brings a project's code and docs indexes in step with the files
// under Root (absolute), chosen by the workspace ingest rules (gitignore,
// size limits, binary skip; dun's rag_ingest.go and the "rag" section of
// dun.json). A prose file (Markdown, text, reStructuredText, AsciiDoc, PDF)
// goes to docs, every other file to code. A file the index lacks or holds
// with other bytes is queued; a document under Root whose file is no longer
// chosen is removed. Empty Project is Root's project slug (dun's
// raglitProject), not the global set: the global set has no code or docs.
// When Root is a linked git worktree, its files go to the worktree's
// branch of each index, over the main checkout's (which is indexed first
// when it is not yet): the branch holds only the files that differ.
// Answered with RagIndexed.
type RagIndex struct {
	Project string `json:"project,omitempty"`
	Root    string `json:"root"`
}

// RagGC removes the index branches of worktrees that no longer exist.
// Answered with RagGCed.
type RagGC struct{}

// RagGCed answers RagGC: how many branch homes were looked at and how
// many were removed.
type RagGCed struct {
	Branches int `json:"branches"`
	Removed  int `json:"removed"`
}

// RagIndexed answers RagIndex: the counts of each index and their sums.
// Summary is the file rules' line: what was chosen and what was skipped.
type RagIndexed struct {
	Project string `json:"project"`
	RagIndexCounts
	Code    RagIndexCounts `json:"code"`
	Docs    RagIndexCounts `json:"docs"`
	Summary string         `json:"summary,omitempty"`
}

// RagIndexCounts is what a RagIndex did: files queued, files the index
// already held as they are, documents removed, and files not queued
// because their last job failed (raglit refused them: minified, unreadable)
// after they last changed. A failed file is queued again when it changes.
// Deferred (a worktree only) is files not queued in the branch because the
// main checkout's index has a job for the same file pending: queued in the
// branch then, the branch would copy a file the parent is about to hold.
// The next RagIndex decides them.
type RagIndexCounts struct {
	Queued   int `json:"queued"`
	Current  int `json:"current"`
	Removed  int `json:"removed"`
	Failed   int `json:"failed,omitempty"`
	Deferred int `json:"deferred,omitempty"`
}
