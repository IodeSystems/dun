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
	event[RagHits]("rag_hits")
	event[RagIngested]("rag_ingested")
	event[RagJobList]("rag_jobs")
	event[RagReembedding]("rag_reembedding")
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
// Answered with RagHits.
type RagSearch struct {
	Project string    `json:"project,omitempty"`
	Index   string    `json:"index"`
	Query   string    `json:"query"`
	Filter  RagFilter `json:"filter,omitzero"`
	Limit   int       `json:"limit,omitempty"`
	Mode    string    `json:"mode,omitempty"`
}

// RagHit is one fragment a search found.
type RagHit struct {
	Path  string  `json:"path"`
	Title string  `json:"title,omitempty"`
	Page  int     `json:"page,omitempty"`
	Text  string  `json:"text"`
	Score float64 `json:"score"`
}

// RagHits answers RagSearch. Mode is the mode that ran; Note says why it is
// not the one asked for.
type RagHits struct {
	Mode string   `json:"mode"`
	Note string   `json:"note,omitempty"`
	Hits []RagHit `json:"hits"`
}

// RagIngest queues one source for an index: Path (a local file, absolute) or
// URL (http, https or file). Tags, when given, are written with the document
// (as its supplied caption, named Title or the file's name). Answered with
// RagIngested.
type RagIngest struct {
	Project string   `json:"project,omitempty"`
	Index   string   `json:"index"`
	Path    string   `json:"path,omitempty"`
	URL     string   `json:"url,omitempty"`
	Title   string   `json:"title,omitempty"`
	Tags    []string `json:"tags,omitempty"`
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
// until this runs. Vectors from another model are dropped, then every
// fragment with no vector is embedded in the background. Answered with
// RagReembedding.
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
