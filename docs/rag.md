# Retrieval in dund (ragcore)

Status: slices 2, 3 and 5 are built (below, "Built: slice 2", "Built: slice
3", "Built: slice 5"), and the strategies index (`strategies.md`); the rest
is a proposal.
Status note: raglit is at v0.7.0; RagHit carries start_line, end_line, symbol,
content_id. The code and docs indexes are built by `rag_index` (below, "Built:
code and docs indexes"), with poly-lsp-mcp's segmenter, pure Go. Sessions do
not search them yet.
Memory notes are indexed and recalled through dund (slice 5), with or
without `/rag on`. Code and doc search still run on raglit, as a separate
server and a bundled extension that is off unless `/rag on`. This file is
why retrieval would move into dund, and what it would need from raglit.
Related: `memory.md` (write side, attestation), `surfacing.md` (the retrieval
rule), `sessions-and-dund.md` (dund).

## Why

(USER 2026-11-08.) "raglit is great, but it's big, it's a separate server,
it's more infrastructure." dund already runs as the one long-lived process per
user (`sessions-and-dund.md`), so it can host the index and its jobs with no UI
and no second daemon. raglit keeps its server, UI and auth for those who want
them, on the same engine.

## The split

**ragcore is raglit's root package, imported, not extracted** (raglit-a4,
2026-11-08, on raglit b008935). `github.com/iodesystems/raglit` is already the
library: store, branches, queue, worker, pipeline, extract, OCR, embed, identity
and search, about 33k lines with no HTTP server. HTTP/huma, OIDC, the MCP server
and the UI are in `cmd/raglit` (about 23k lines). Go links only the packages
imported, so dund would not carry them. What raglit changes first: split
`raglit/attest` into data types (no huma) and its HTTP service, because huma
reaches the root package only through attest; then semver tags, with
Store/Hit/the Job API as a public surface. **Not a nested module**: aw4 already
builds against `../raglit` through a replace and breaks on each dependency bump,
and a second module doubles that.

What ragcore means here:

- **Ingest pipeline**: fetch → extract (PDF and image to text, kept: USER) →
  OCR → fragment → embed → commit. Each stage records the engine that ran it
  (raglit does this now).
- **Jobs**: the queue and workers, resumable, each stage's result recorded.
- **Search**: FTS (bm25) + vector + RRF hybrid; field filters applied before
  ranking (below).
- **Branches**: copy-on-write overlay (below).
- **Storage**: SQLite, concrete. No storage interface until a second backend
  exists (raglit-a4: an interface now is guessing). Queries are sqlc-generated
  into raglit `internal/db`; a Postgres backend is a second sqlc engine plus
  rewrites of FTS5, the cosine scan and the overlay.
- **Model roles**: configuration and probing (below).

**Present but unused by dund**: the root package also holds `acl.go`,
`keys.go` and `Registry` (many indexes, ACL, admin). Store does not depend on
them (`Open(path)` and `OpenIndex(home, name)` work without a Registry), so dund
uses Store and the queue directly. Not checked: whether `worker.go`/`queue.go`
assume a Registry for multi-index scheduling; one index may need a small
single-store adapter. The `attestations` table stays in the schema, unused.

**Not used from ragcore** (USER: "doesn't really need attestation"): attestation,
HTTP API, auth, UI, MCP server. Attestation is a judgment layer above
retrieval. dun's memory already keeps its own log (`memattest.go` writes
`<note>.md.attest.jsonl` in raglit's `attest.Entry` shape), and the raglit
server keeps its own over ragcore. What attestation needs from the core is a
stable content id per fragment and "does this id still exist", which any
store needs anyway for dedup and re-embedding.

**Hosts.** dund embeds ragcore, owns every index and runs the jobs; sessions
query over dund's existing socket. The raglit server is ragcore plus its UI,
OIDC, MCP and attestation.

## Storage: what raglit has (read 2026-11-08)

One SQLite file per index (`modernc.org/sqlite`, pure Go, no cgo); schema in
raglit `sql/schema.sql`, applied by `store.go`.

| role | tables |
|---|---|
| content | `documents`, `fragments` |
| full text | `fragments_fts` (FTS5, bm25) |
| vectors | `fragment_vectors` (BLOB per fragment, brute-force cosine scan in Go); `media_vectors` (image space, not comparable with text) |
| jobs | `ingest_jobs`, `job_stages`, `identity_jobs` |
| OCR and pages | `page_readings`, `ocr_pages`, `ocr_page_cache`, `media` |
| near-duplicates | `shingle_pages`, `shingle_index` |
| typed fields | `doc_types`, `doc_fields` |
| meta | `index_meta` |
| attestation | `attestations` (leaves ragcore) |

FTS5 and the cosine scan are raw SQL; the rest is sqlc.

**Branches** (raglit `branch.go`): an overlay over a parent at document grain.
A branch stores only its diffs (changed or added documents, and tombstones), so
forking is cheap; FTS, vector and hybrid search, figures and page images read
through the overlay. Session branches (`code@ID`) exist from their first write.
Merge is not built (raglit `docs/design/daemon-stack.md` P6); a diff is. For dun,
a worktree or a sub-agent gets its own branch of the code index without a
re-index.

## Pluggable storage: considerations

1. **What is SQLite-specific**: FTS5/bm25, the cosine scan, and the branch
   overlay, which is written into those queries. A Postgres backend is
   `tsvector` + `pgvector` and the overlay rewritten. "Pluggable" means an
   interface implemented per backend, not a driver swap.
2. **One store per index if possible**, so a fragment and its vectors commit in
   one transaction. Separate FTS and vector stores are only eventually
   consistent and need repair jobs.
3. **The brute-force scan has a ceiling** (fine to roughly tens of thousands of
   fragments; an estimate, not measured). An ANN index (sqlite-vec, HNSW) is the
   likelier first storage change, before a second backend.
4. **The vector dimension belongs to the embedding model.** Record the model id
   and dimension per index, refuse a write that does not match, and re-embed as
   a job when the model changes. Never mix spaces. Built in slice 3 (below).
5. **Originals outside the database**: source PDFs and images on the
   filesystem or an object store, by content hash. The DB keeps text and
   metadata, and dedup comes free.
6. **Schema versions**: a migration per backend, and an export/import format,
   so SQLite → Postgres is supported.
7. **Deletion**: garbage-collect orphaned fragments and vectors as a job.
8. **One writer**: dund serializes writes, sessions read; SQLite in WAL mode.
9. **Field filters before ranking** (raglit-a4: the structure supports it). The
   path filter already does this: `pathPredicate` (raglit store.go:788) is a SQL
   predicate in the WHERE of both the FTS and the cosine query, and RRF fuses
   after. A field filter is a second predicate, e.g. `EXISTS (SELECT 1 FROM
   doc_fields f WHERE f.doc_id = d.id AND json_extract(f.fields,'$.k') = ?)`.
   `doc_fields` is one JSON row per document, indexed only on type, so
   `json_extract` runs per row: fine for a memory index; a large index wants
   expression indexes or a `doc_tags(doc_id, tag)` table. Branches: correct only
   if a field edit in a branch also writes the document into the branch layer
   (not checked). An ANN index loses free pre-filtering (filtered HNSW or
   over-fetch then filter), so fix filter semantics before an ANN swap.
   Earlier text of this item:: tags and frontmatter as filters before
   ranking. The memory design needs them (tags filter, embeddings rank: an
   embedding alone blurs scope; the first measurements found noise was the
   problem, 11 of 21 surfaced notes wrong). `doc_fields` already holds typed
   fields, so this is applying them before ranking, not a new table. Both
   backends must support it.

## Model roles

Each is configured, probed, and recorded in agentkit's capability record (the
same record as `/model probe`). raglit already configures roles (`modelchan.go`,
`ocrengine.go`, `captionprofile.go`): start from that format so dun and raglit
do not keep two. An index records which models built it; a
change re-queues the affected stages.

| role | for | absent |
|---|---|---|
| agentic (chat) | identity, summaries, fact extraction, the note fork | no summaries; plain text |
| embedding | vector search (text; optionally image) | FTS only (10 of 11 right on notes alone, `memory.md`) |
| OCR (vision model or tesseract) | scans, images → text | indexed by filename and caption, and said so |
| fragmenter | semantic segmentation of unstructured documents | rule-based: headings, code symbols, paragraphs |

**Probes** (extending `/model probe`; built, "Built: slice 3"):

- **embedding**: dimension, batch limit, latency, the same vector for the same input;
- **OCR**: a fixture image with known text, scored by character error rate;
- **fragmenter**: schema compliance on a fixture document;
- **agentic**: the existing chat probes.

**Defaults**: the gateway's embedding model when present (corrallm serves
`local-nomic-embed-text`); none otherwise, which degrades to FTS.

## Jobs and cost

- **Priority**: an interactive query outranks background ingest.
- **Gateway limits**: respect corrallm's per-model slots (8 running + 8 queued
  for the Fireworks model, measured 2026-11-08) and send its 429 ticket back as
  `X-Corrallm-Request-Id`; agentkit's client already does.
- **Budgets**: a per-day token and dollar budget per role, and idle-time
  scheduling for ingest. OCR and model fragmenting are the expensive stages.

## Built: slice 2

dund hosts raglit v0.5.1 (`internal/ragd`). Sessions use it for memory
notes (slice 5); `dun dund rag` is the manual client.

**Layout**, under `$DUN_HOME/dund/rag/`:

```
global/{memories,strategies}/
<project-slug>/{code,docs,memories,strategies}/
```

Each index is its own raglit Home (`raglit.OpenHome`). The project slug is
the caller's: dun's `raglitProject` (serverdefs.go, package `dun`), which
dund cannot import, so dund only checks it (`[a-z0-9._-]`, not `global`).
An index is made on its first ingest. At start, dund opens the indexes on
disk, so jobs queued before a restart run. A search or job list of an index
that does not exist answers empty and makes nothing.

**Jobs**: two `raglit.Runner`s over one `Channels`: one over the
memories and strategies indexes of every scope (2 jobs at once), one over
code and docs (raglit's default). A bulk code ingest then holds no runner a
memory note waits for; the shared `Channels` still limit each model across
both. Each runner's `StoreSet` is the open indexes of its kinds;
`Existing` never opens one. Each store gets `SetEmbedder` and
`SetIdentifier` when it opens. On stop and on re-exec: `Drain(10s)` of both
runners, then `CutOff` of both (the caption loop, which Drain leaves
running), then wait for both `Done` (raglit v0.6: Done covers the caption
loop) before the stores are closed. A runner not done 10s after the
cut-off leaves the stores open. A cut-off job goes back on the queue.

**Facets** (model roles), in `~/.dun/config.json`:

```json
"facets": {"embed": {"provider": "corrallm", "model": "local-nomic-embed-text"},
           "vision": {...}, "segment": {...}, "identity": {...}}
```

`provider` is a saved provider's name (its URL and key are used); empty is
the active provider. With no `embed` facet, embed is `local-nomic-embed-text`
on the active provider. A role not set, or on a provider that is not saved,
is off: no embed → FTS only; no vision → tesseract if installed, else no
OCR; no identity → no captions. dund logs each role once at start. The chat
roles (main, child, vision for chat) may move into this map later.

**Socket commands** (no session id; `proto/rag.go`):

| command → reply | fields |
|---|---|
| `rag_search` → `rag_hits` | project (empty = global), index, query, filter {path_prefix, tags, fields}, limit, mode (hybrid, default; fts) → mode, note, hits [path, title, page, text, score, similarity, matched, terms] |
| `rag_ingest` → `rag_ingested` | project, index, path (absolute) or url (http, https, file), title, tags → job, url |
| `rag_jobs` → `rag_jobs` | project, index, state, limit → jobs [id, url, title, state, error, fragments] |
| `rag_forget` → `rag_forgotten` | project, index, path or url → url, removed (slice 5) |
| `rag_sync` → `rag_synced` | project, index, files [path, title, tags], gone [path] → queued, current, removed (slice 5) |
| `rag_tag` → `rag_tagged` | project, index, path or url, tags → url |
| `rag_index` → `rag_indexed` | project (empty: root's slug), root → project, queued, current, removed, failed, code {…}, docs {…}, summary |

An index the project does not keep is refused. A hit's path is its source
URL (`file:///abs/path`). Hybrid with no embed model, or with one that
fails, runs as FTS and says so in `note`. Tags are raglit's caller-owned
tags (`Store.SetTags`, the `documents.tags` column): filter data, not text,
so no fragment. Normalized on write and on read; a tag filter is whole-tag
AND, and `kind:strategy` matches exactly. Tags given on ingest are set
before the job is queued: on a path not yet committed raglit holds them
for the job's commit; a re-ingest leaves them alone. `rag_tag` replaces a
document's tags with no ingest (no tags clears them). `rag_sync` sets the
tags of every file it is given, current or not.

**raglit calls** (raglit v0.6.1): every function and method dund calls is
pinned in raglit's `api_test.go`, except `Home.IndexPath` (dund checks
whether an index file exists before opening it). A file is ingested as a
queued job; text (memory notes) by `Store.Ingest` at once.

## Built: slice 3

**Probes**: `dun dund rag probe [embed|vision|segment|identity]` (no role:
all four), or `/model probe <role>` in a session. Each spends a few
requests on the role's model and writes facts with source `probed` into
agentkit's capability record for that (endpoint, model), the file
`/model probe` writes (`$DUN_HOME/capabilities`). The record is an open
keyed map, so these are new keys, not a new format:

| role | key | measures |
|---|---|---|
| embed | `embed.dimension` | length of one vector |
| | `embed.batch_max` | largest of 1, 8, 32, 64 inputs per request accepted; stops at the first failure (evidence names it) |
| | `embed.latency` | wall time per request, by batch size (`1:40ms 8:55ms …`) |
| | `embed.deterministic` | the same input twice gives cosine >= 0.9999 |
| vision | `ocr.cer` | character error rate reading a generated PNG (basicfont, 4x; no binary in the tree) of two known lines |
| segment | `segment.schema` | raglit's own `Segmenter` (its `emit_fragments` schema and fix loop) on a two-topic fixture: true when it validated without the fallback and the fragments hold >= 80% of the words; false says why |
| identity | the chat keys | agentkit's `ProbeCapabilities`, as `/model probe` |

The probe runs in the CLI, not in dund: roles are a pure function of
`config.json` (`Resolve`), and a probe should not need dund running. A
failure of one embed input stops the embed probe with nothing recorded.

Measured 2026-11-08, `local-nomic-embed-text` on the gateway: dimension
768, batch 64 accepted, latency 1:1552ms (first call, model load) 8:41ms
32:139ms 64:243ms, deterministic (cosine 1.000000).

**Index model record**: each index records the embed model its vectors came
from and their dimension (raglit's `index_meta` `embed_model` and
`embed_dim`, both written by `SyncEmbedModel(model, dim)`; dund only reads
them). The dimension is the probed `embed.dimension`; never probed, dim 0
is passed (unknown) and only the model id is compared. Before raglit v0.6
dund wrote `embed_dim` itself, with the same key and meaning, so an index
from then needs no migration: raglit reads it as its own. On open:

- no record: the index adopts the embed role (raglit's rule);
- record matches: `SyncEmbedModel` records a dimension not yet recorded
  (and drops stored vectors of another length, which no query can match);
  the embedder is attached; fragments with no vector (ingested while embed
  was off, or a cut-off re-embed) are embedded in the background;
- record differs (model id, or a known dimension): no embedder. No vector is
  written to it and a hybrid search runs as FTS with a note naming both
  models. dund logs it once.

`dun dund rag reembed --index …` (socket `rag_reembed` → `rag_reembedding`)
drops every vector (`DropVectors`), records the role's model and dimension
(`SyncEmbedModel`), attaches the embedder and embeds every fragment in the
background (`BackfillVectors`, 64 at a time). A dimension change under the
same model id is re-embedded the same way. Not a raglit queue job: the
pinned API has none. It resumes across restarts because open backfills
whatever has no vector. It runs while an ingest job runs on the index:
raglit's `SetEmbedder` is atomic (v0.6), and a vector made with a
replaced embedder is not written (`TestReembedWhileAJobRuns`, `-race`).

## Built: slice 5

The memory tool and memory recall use dund; `/rag on` is not needed for
them. Code and doc search are unchanged (raglit, `/rag on`). Code:
`raglink.go` (the socket client), `memrecall.go` (search, degraded mode,
sync), `memory.go` (the tool).

- **Link.** A session dials dund's socket (`Socket(DUN_HOME)`) for each
  call, under dund or as a standalone `dun -p`. `DUN_MEMORY=0`: no memory
  tool and no memory recall.
- **Write.** A note stays a file (`memory/<project>/`, or
  `memory/@global/` for `scope: global`) with its attestation log. The
  tool now takes `scope` and `tags`; tags go in the frontmatter and to
  `rag_ingest`. Index: `memories` of the project, or of the global set.
  A void sends `rag_forget`.
- **Recall.** The proactive search asks both memories indexes beside the
  docs server's, when that runs. A hit carries `similarity`, `matched` and
  `terms`, so the surfacing rule and thresholds are unchanged
  (`surfacing.md`). No tag or path filter is sent: the surfacing search
  used none before. Project and global notes are separate groups, each
  ranked in its own index; the project note comes first (USER 2026-11-08).
- **One hit.** Before raglit v0.6, a tagged note was found up to three
  times in one search: its frontmatter, its text, and the caption its tags
  were written as (measured live, FTS, 2026-11-08). A note is now indexed
  as text, not as its file: `rag_ingest` and `rag_sync` carry `text`, a
  memory's question and answer or an item's name and detail, and dund
  calls `Store.Ingest` with that one fragment, at once, with no job. The
  frontmatter is no fragment and the tags are `SetTags`, so a word in the
  name and the detail is one hit (`TestTaggedNoteIsOneHit`). Frontmatter
  words (`public`, the date) are not searchable. dun still keeps one hit
  per note path.
- **Tags.** A note is filed under its own tags, `kind:<kind>`
  (`kind:memory`, `kind:strategy`, `kind:constraint`) and `important` when
  the user marked it. A change that leaves the text as it was
  (`/memories important`, `unimportant`, a refine of the tags alone) sends
  `rag_tag` and no ingest.
- **Sync of text.** raglit records a source hash only for what it reads
  itself, so dund records the sha256 of the text it ingested, in the
  index's `index_meta` (key `dund_text_sha256 <url>`; cleared on forget).
  `rag_sync` re-ingests a note when that hash differs from its text now. A
  frontmatter change alone (important) is current.
- **dund down** (no socket, or a dund with no retrieval): the note is
  written and the tool result says it is not indexed yet; a void goes
  ahead and says the same. Recall is a word search of the note files
  (question, tags, text; stop words left out), under the coverage rule.
  The session says once that it is degraded.
- **Sync.** The first call that reaches dund (at start, and after any
  failure) sends `rag_sync` with every note and every voided note. dund
  queues a note it lacks, or holds with another source hash (or, without
  a hash, from before the file's mtime), and removes the voided ones.

Built since: the strategies index, holding strategies and constraints
(one file shape with memories: a short name, optional detail, global or
project, important). See `strategies.md`.

## Built: code and docs indexes

`rag_index {project, root}` → `rag_indexed`, and `dun dund rag index [ROOT]`.
Code: `internal/ragd/index.go`, `internal/ragd/codeseg.go`,
`internal/dund/ragindex.go`.

- **Pure Go (USER 2026-11-09).** dund imports
  `github.com/iodesystems/poly-lsp-mcp/segment` v0.5.0 without
  `-tags treesitter_cgo`: the parser is gotreesitter, and the release build is
  unchanged (`CGO_ENABLED=0`). `codeSeg` is a copy of raglit's
  `cmd/raglit/codesegment.go`; its `Runtime()` is
  `poly-lsp-mcp/segment@v0.5.0 gotreesitter@…`. It is `Models.Code` of the
  code+docs runner only; the memories+strategies runner has none. Binary:
  55.5 MB → 65.9 MB (stripped, linux/amd64, +10.4 MB).
- **Files.** The workspace ingest rules (`rag_ingest.go`: git's file list or
  a walk that honors `.gitignore`, dependency and build directories, the
  directory and file size limits, binary files, the `rag` section of
  dun.json). dund calls `dun.IndexFiles(root)`; ragd does not import package
  `dun`.
- **Code or docs: by extension.** `.md .markdown .mdx .txt .rst .adoc .pdf`
  go to docs; every other chosen file goes to code (`.gitignore`, `Makefile`
  and `.json` too). A file never moves between the two. raglit cuts a file
  the segmenter knows at its declarations (symbol, line span, doc comment
  in) and Markdown at its headings (symbol is the heading path, `A.B`); text,
  rst and the rest are read into windows.
- **Incremental.** A file is queued when the index does not hold it or holds
  other bytes (`DocSummary.ContentHash` against the file's sha256). A
  document under `root` whose file is no longer chosen is removed. A file
  whose newest job failed (raglit refuses minified files) after it last
  changed is not queued; `failed` counts it. The index records the
  segmenter's runtime (`index_meta` `dund_code_segmenter`); when it differs
  (the first index with the segmenter, or a new poly-lsp-mcp), every held
  file is queued Fresh, because raglit skips bytes it already holds. The
  reply counts `queued, current, removed, failed`, per index and summed;
  `project` empty is root's slug (`raglitProject`).
- **Jobs.** Every file is a job on the code+docs runner; the reply comes
  when the jobs are queued, not done (`rag jobs`).
- **Trigger: on demand only** (`dun dund rag index`). Proposed, not built:
  (1) at session start for the workspace, best-effort, in the background
  (a no-change run on this repo is 0.06 to 0.1 s, so it costs nothing when
  nothing changed); (2) dund re-runs it for each workspace a session has open
  when a turn ends, which catches the agent's own edits; (3) a file watcher
  (fsnotify is already in the module graph through poly-lsp-mcp), only if
  (2) proves too late.
- **Measured** (this repo, FTS only, no embed, 24 cores, 2026-11-09): 826
  files (778 code, 48 docs), 19,971 fragments (19,377 code, 594 docs); 705
  code files cut at declarations, 71 in windows; 36 Markdown files at
  sections. First index 66 s from the call to the last job done; the call
  itself 5 to 12 s (one queue insert per file, while the runner already
  commits). A no-change re-run: 63 ms, nothing queued, 2 failed
  (`xterm.js`, a test JSON: minified).
- **Search.** `rag_search` on code or docs returns `start_line`, `end_line`
  (half-open) and `symbol`; `dun dund rag search` prints
  `path:start-last symbol`, the last line inclusive.
- **Session search: not switched.** The session's code and doc tools are
  still raglit's docs server under `/rag on`. Proposed switch: the session
  sends `rag_index` for its workspace (trigger 1), and the docs tools and the
  proactive docs search send `rag_search` to the code and docs indexes
  through `raglink.go`, as memory recall does; `/rag on` would then be only
  for raglitd's own corpora (attached indexes, the user's documents, the
  raglit UI), and `/rag workspace` would decide whether dund indexes the
  workspace.

## Slices

1. raglit: split attest into types and service; semver tags (raglit's call).
2. dund hosts ragcore: the socket API for search, the job runner, config.
   Built (above); branch commands are not.
3. Model-role configuration and the four probes. Built (above).
4. Field filters before ranking (needed by memory).
5. dun memory and surfacing move to dund; `/rag on` stops being needed for memory.
   Built (above).
6. ANN vector index, when an index outgrows the scan.

## Open

- **Trust domain: decided (USER 2026-11-07, relayed by raglit-a4).** dund is
  its own trust domain, so embedding raglit's engine is approved in principle.
  raglit's rule (2026-10-30: always through the daemon, no embedded mode) keeps
  holding for raglitd and raglit web, with ACL, keys, Registry and OIDC; none of
  that goes into dund. dund opens its Stores directly (`raglit.OpenHome`, one
  Home per index) and is the single writer; a dund index is not reachable by raglit
  clients.
- **Scope: decided.** dund keeps exactly four indexes: **code, documentation,
  memories, strategies**. Strategies is new: the learned, reusable approaches
  from the learning-phase design (churn → resolved candidate → lesson), kept
  apart from memories so a lesson's retrieval is not crowded by project facts.
  The user's constraints share it, marked by `kind` (`strategies.md`). The
  learning phase is not built.
- **Organization: dund's own** (USER, relayed by raglit-a4). dund uses none of
  raglit's organization: no Registry, no Home layout, no `project:<ns>`
  namespaces, no `/mcp/{project}` keys. dund decides which Stores exist (for
  example per project, plus a global set of code, docs, memory and strategy)
  and where their files live, and opens each with `raglit.OpenHome` (a bare
  `Open` has no Home: no originals or page images). What
  dund needs from raglit: Store, Hit and the job API, stable and tagged.
- The four indexes are **for dund itself** (USER, relayed by raglit-a4): they
  serve dun's own work. The user's document corpora stay on raglitd. OCR and
  PDF/image extraction are in dund's first slice anyway (USER, relayed by
  raglit-a4).

- Does code and document indexing move into dund too, or only memory? The
  indexes are built (above); sessions do not search them yet.
- ragcore API stability and versioning between raglit and dun.
- Merge for branches (raglit P6): needed when a sub-agent's index changes should
  land in the parent.
