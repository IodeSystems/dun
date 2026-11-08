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

**ragcore**: a library extracted from raglit.

- **Ingest pipeline**: fetch → extract (PDF and image to text, kept: USER) →
  OCR → fragment → embed → commit. Each stage records the engine that ran it
  (raglit does this now).
- **Jobs**: the queue and workers, resumable, each stage's result recorded.
- **Search**: FTS (bm25) + vector + RRF hybrid; field filters applied before
  ranking (below).
- **Branches**: copy-on-write overlay (below).
- **Storage**: an interface with SQLite as the reference backend.
- **Model roles**: configuration and probing (below).

**Not in ragcore** (USER: "doesn't really need attestation"): attestation,
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
9. **Field filters inside the backend**: tags and frontmatter as filters before
   ranking. The memory design needs them (tags filter, embeddings rank: an
   embedding alone blurs scope; the first measurements found noise was the
   problem, 11 of 21 surfaced notes wrong). `doc_fields` already holds typed
   fields, so this is applying them before ranking, not a new table. Both
   backends must support it.

## Model roles

Each is configured, probed, and recorded in agentkit's capability record (the
same record as `/model probe`). An index records which models built it; a
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

1. raglit extracts ragcore: a code move plus the storage interface (raglit's work).
2. dund hosts ragcore: the socket API for search and branch, the job runner, config.
3. Model-role configuration and the four probes.
4. Field filters before ranking (needed by memory).
5. dun memory and surfacing move to dund; `/rag on` stops being needed for memory.
6. ANN vector index, when an index outgrows the scan.

## Open

- Does code and document indexing move into dund too, or only memory? This
  proposal assumes both.
- ragcore API stability and versioning between raglit and dun.
- Merge for branches (raglit P6): needed when a sub-agent's index changes should
  land in the parent.
