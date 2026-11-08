# Retrieval in dund (ragcore)

Status: slices 2, 3 and 5 are built (below, "Built: slice 2", "Built: slice
3", "Built: slice 5"); the rest is a proposal.
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

**Jobs**: one `raglit.Runner` over all open indexes (a map as the
`StoreSet`; `Existing` never opens one). Each store gets `SetEmbedder` and
`SetIdentifier` when it opens. On stop and on re-exec: `Drain(10s)`, then
`CutOff`; a cut-off job goes back on the queue.

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

An index the project does not keep is refused. A hit's path is its source
URL (`file:///abs/path`). Hybrid with no embed model, or with one that
fails, runs as FTS and says so in `note`. Tags are normalized on write and
on read; a tag filter is whole-tag AND. Tags given on ingest are written
as a supplied caption (named by the title, else the file name), and that
caption is also a searchable fragment: a search can hit it beside the
document's own text.

**Not pinned by raglit's api_test.go, used anyway**: `NewEmbedder` (no
other way to make the `*Embedder` that `SetEmbedder` takes),
`Models.Identifier` (raglit's ragcore.md says to use it), and
`Store.SupplyIdentity` with `DocIdentity` (the one way a caller writes
tags). Ingest is always a queued job; a direct `Store.Ingest` needs
`Document` and `Fragment` fields, which are not pinned either.

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
from (raglit's `index_meta` `embed_model`, which raglit already keeps) and
their dimension (`index_meta` `embed_dim`, dund's key; raglit has none).
The dimension is the probed `embed.dimension`; never probed, only the
model id is compared. On open:

- no record: the index adopts the embed role (raglit's rule);
- record matches: the embedder is attached; fragments with no vector
  (ingested while embed was off, or a cut-off re-embed) are embedded in the
  background;
- record differs (model id, or a known dimension): no embedder. No vector is
  written to it and a hybrid search runs as FTS with a note naming both
  models. dund logs it once.

`dun dund rag reembed --index …` (socket `rag_reembed` → `rag_reembedding`)
drops the old vectors (`SyncEmbedModel`), records the role's model and
dimension, attaches the embedder and embeds every fragment in the
background (`BackfillVectors`, 64 at a time). Not a raglit queue job: the
pinned API has none. It resumes across restarts because open backfills
whatever has no vector. A dimension change under the same model id is
refused: `SyncEmbedModel` drops vectors only for another model id.

**raglit exports used, not pinned by api_test.go** (new in slice 3):
`Store.EmbedModel`, `Store.SyncEmbedModel`, `Store.BackfillVectors`,
`Store.MissingVectors`, `Store.Meta`, `Store.SetMeta`, `NewSegmenter`,
`Segmenter.SegmentText`, `SegResult.Degraded`.

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
  used none before. Project and global hits are one group (memory): under
  the coverage rule that compares bm25 scores of two indexes, which do not
  compare (`memory.md`, "Per group"). Not measured.
- **Double hit.** A tagged note is found up to three times in one search:
  its frontmatter, its text, and the caption its tags are written as
  (measured live, FTS, 2026-11-08). dun keeps one hit per note path, with
  the best similarity and term count of its fragments.
- **dund down** (no socket, or a dund with no retrieval): the note is
  written and the tool result says it is not indexed yet; a void goes
  ahead and says the same. Recall is a word search of the note files
  (question, tags, text; stop words left out), under the coverage rule.
  The session says once that it is degraded.
- **Sync.** The first call that reaches dund (at start, and after any
  failure) sends `rag_sync` with every note and every voided note. dund
  queues a note it lacks, or holds with another source hash (or, without
  a hash, from before the file's mtime), and removes the voided ones.
- **Re-embed guard.** `rag_reembed` is refused while the index has a
  running ingest job: raglit's `SetEmbedder` is a plain field write a
  running worker reads (raglit-a4 has the fix).

Not built: the strategies index (rules with a short name and optional
detail; global, project or important).

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

- Does code and document indexing move into dund too, or only memory? This
  proposal assumes both.
- ragcore API stability and versioning between raglit and dun.
- Merge for branches (raglit P6): needed when a sub-agent's index changes should
  land in the parent.
