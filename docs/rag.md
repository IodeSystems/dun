# Retrieval in dund (ragcore)

A proposal, not built. dun's retrieval (memory notes, proactive surfacing of
docs and code) runs on raglit today, as a separate server and a bundled
extension that is off unless `/rag on`. So persistent memory is effectively off:
the `memory` tool exists only while the docs server runs (`memory.md`), and in
practice recap, which is in-session only, is the memory dun has. This file is
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
   a job when the model changes. Never mix spaces.
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

**Probes** (extending `/model probe`):

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

## Slices

1. raglit: split attest into types and service; semver tags (raglit's call).
2. dund hosts ragcore: the socket API for search and branch, the job runner, config.
3. Model-role configuration and the four probes.
4. Field filters before ranking (needed by memory).
5. dun memory and surfacing move to dund; `/rag on` stops being needed for memory.
6. ANN vector index, when an index outgrows the scan.

## Open

- **Trust domain: decided (USER 2026-11-07, relayed by raglit-a4).** dund is
  its own trust domain, so embedding raglit's engine is approved in principle.
  raglit's rule (2026-10-30: always through the daemon, no embedded mode) keeps
  holding for raglitd and raglit web, with ACL, keys, Registry and OIDC; none of
  that goes into dund. dund opens its Stores directly (`raglit.OpenIndex` /
  `Open`) and is the single writer; a dund index is not reachable by raglit
  clients.
- **Scope: decided.** dund keeps exactly four indexes: **code, documentation,
  memories, strategies**. Strategies is new: the learned, reusable approaches
  from the learning-phase design (churn → resolved candidate → lesson), kept
  apart from memories so a lesson's retrieval is not crowded by project facts.
- **Organization: dund's own** (USER, relayed by raglit-a4). dund uses none of
  raglit's organization: no Registry, no Home layout, no `project:<ns>`
  namespaces, no `/mcp/{project}` keys. dund decides which Stores exist (for
  example per project, plus a global set of code, docs, memory and strategy)
  and where their files live, and opens each with `raglit.Open(path)`. What
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
