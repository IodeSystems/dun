# Proactive retrieval ("surfacing")

Before each model round, dun searches raglit with the user's message and may
inject the best match into the prompt. A wrong injection costs context and
misleads the model, so the rule is strict: a miss is cheaper than noise. Code:
`notify.go` (rule and query), `surfacing.go` (labels), `extension.go`
(`indexedFinder`). Constants: `surfaceFloor`, `surfaceDelta`, `coverageFloor`,
`surfaceQueryWords`.

## Rule

The rule runs per group (memory notes, skills, rules, MCP docs, workspace
docs, attached docs) and surfaces only the top document of a group that
clears the bar. A group that fails surfaces nothing and marks nothing seen, so
the same documents can win a later, clearer pass. Early measurement (2026-10-20):
"any hit, two slots" made 11 of 21 surfaced notes the wrong one and surfaced
two on every negative probe.

By index kind:

- **With embeddings:** cosine similarity has a fixed range (nomic-embed-text,
  0.45 to 0.85 in practice). Top similarity >= 0.68, and >= 0.01 over the
  runner-up if there is one; a lone candidate needs only the floor. A hit with
  no similarity (the vector side did not rank it) never surfaces.
- **Without embeddings (bm25):** the top hit holds >= 0.5 of the query's words
  and scores >= 1.5x the runner-up. Coverage alone let 22 of 42 off-topic
  questions surface; with the ratio, 6.
- **Older raglit** (before `e66a3df`, no similarity or term counts): the old
  1.5x score ratio.

Why the old ratio was dropped: BM25 scores have no fixed range, and once
raglit's search went hybrid the score became a rank fusion (at most ~0.033),
so two candidates almost never differed by 1.5x, and a lone candidate passed
with no test at all.

## Evidence for the numbers (2026-10-23)

85 questions (43 on-topic, 42 off-topic) over the dun code, skills, memory and
rules indexes. Right hits scored 0.62 to 0.84; off-topic top hits 0.47 to 0.73
(37 of 42 under 0.67).

| floor / delta | right | related | wrong | missed | off-topic surfaced |
|---|---|---|---|---|---|
| 0.66 / 0.01 | 21 | 5 | 3 | 14 | 4 |
| 0.68 / 0.00 | 20 | 6 | 4 | 13 | 3 |
| **0.68 / 0.01** | 18 | 4 | 3 | 18 | 1 |
| 0.68 / 0.02 | 16 | 4 | 2 | 21 | 1 |
| 0.70 / 0.01 | 12 | 3 | 1 | 27 | 0 |

0.68 / 0.01 was chosen: it trades misses for near-zero noise. A per-kind floor
differed by one or two questions, which 12 questions per kind cannot resolve.
Adding a word-coverage test to the similarity rule changed nothing.

## Query

The query is the first 50 words of the newest user message (later passes in the
same turn: the newest entry with text). Over 8 long messages, the right
document ranked first for 4 with the first 50 words (as many as the whole
message), 2 with the first 30, 0 with the first 20, and 1 with the old query
(the model's ~110-word reply joined to the message), which pushed the right
document out of the top 20 for 4 of 8. Latency per pass fell from a median of
4.9 s (over the 3 s preparer timeout, so passes failed open) to 1.2 s.

`mode` is left unset so raglit picks hybrid on an index with embeddings and
bm25 on one without. The search role's `list` tool tells dun which indexes have
embeddings (`indexMeaning`).

## Presentation

- **Every surfaced item says where it came from (USER, 2026-10-23).** The
  owner asked "who is poisoning or pointlessing my agent" after a TUI row read
  `skill docs (0.00)` with no origin; it was a claude.ai-synced skill. Each hit
  carries kind, path, index, root (for skills: user, claude.ai sync, project,
  extension) and a `why` string (`sim 0.71 · 3/4 terms · +0.07 over runner-up
  (of 4)`, `only candidate · ...`). The model also gets a `Source:` line.
  Scores print with `%.2g`; `%.2f` showed 0.00 for small fused scores.
- **A surfaced skill is a load cue, not a fragment.** A hit on a skill's
  SKILL.md (or a reference file in it) is written as "A skill matches ... call
  skill name=X and follow it before you act" and never inlined.
- **Rules imports can be indexed** (`/rules index`); hits from them are their
  own group, framed as imported rules.

## Known gaps

- With many skills or a polluted index, a lone candidate still passes on the
  floor alone; duplicates (worktree copies, `node_modules`) must be kept out of
  the index (see the "tool must not index its own state" rule in
  `decisions.md`).
- The bm25 branch was exercised only by tests and the sweep; every live index
  had embeddings. Whether coverage 0.5 plus ratio (built) or coverage 0.67
  alone (more right, 6 wrong vs 1) is better is an open owner decision.
- Code questions surface less on a small code-only index than in the
  calibration (similarity of a right file ~0.59).
- The calibration searched one index at a time; dun searches all of a
  project's indexes merged with limit 8, so a group often has 1 or 2 candidates.
