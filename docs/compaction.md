# Compaction and recap: triggers, measurement, principles

Investigation of 2026-11-14 (USER: "what are good points, how we measure, and
what is generally good instead of benchmaxing"). Related: `decisions.md`
(recap, clip, compaction rules), `providers-and-cost.md` (shaper policy,
re-heading, cache statistics), `reading.md` (whole reads), `memory.md` (the
cache-parity fork).

Tags: **[M]** measured (here, or a cited paper with numbers) · **[D]** from
dun's docs and code, or a vendor's docs · **[V]** vendor claim with no
published method · **[O]** opinion or inference.

## What shrinks context today

The Shaper's rungs, per build (agentkit v0.29.0 `agent/shaper.go:302-423`):
render, then LOD, then the host's Rehead, then a fold (at most 4 attempts).
Every rung waits for the **target**: budget minus 10k headroom
(`shaper.go:137`), where budget = window − (32,768 output + 2,048 margin)
(`internal/harness/shaping.go:221-235`, `internal/harness/outbudget.go:42,48,77`). On a 188k window that is
about 143k. On a window of 1M or so (DeepSeek V4.1 flash reached 279k
without a fold) only the bounding rungs ever run.

- **Bound what enters [D, M].** exec clips at 20k characters, head and tail,
  the whole output in a file (`internal/harness/exec.go:130`; one task 456,772 → 32,916
  tokens, `decisions.md:25-35`). `MaxToolResultChars` 64k bounds one result
  of any age (`agent/policy.go:48`). Grep returns a window around a match;
  `file_read at:N` pages unlined output. The whole-read gate asks for a
  recap past a quarter of the room (`reading.md:49-110`). None of these
  touch the cached prefix.
- **recap (`internal/harness/recap.go`) [D, M].** The model replaces a span of the current
  turn; no summarizer call; the cache breaks only from the span's start,
  which is recent. Removed calls stay as one-line stubs (`internal/harness/recapstub.go`),
  the span goes to `<session>.recapN.jsonl`. Cues: the same tool 3+ times in
  a row costing 4k+ characters, a 20k+ result the model has moved past, a
  window of 20k+ tokens (then +15k) holding a 4k+ item (`internal/harness/recap.go:675-805`).
  Measured unprompted: active context 65,056 → 1,057 tokens
  (`decisions.md:113-118`). Stubs exist because cheap models re-ran commands
  after a recap in Terminal-Bench (`decisions.md:101-112`).
- **LOD [D].** Older results over 4,000 characters (`internal/harness/window.go:113`) become
  stubs; exec keeps both ends (`internal/harness/shaping.go:259`). The boundary moves 24
  entries at a time (`shaper.go:120`), so the cache breaks about once per
  eight turns, not every turn.
- **Rehead (`internal/harness/rehead.go`) [M].** Older copies of a re-read target become
  stubs, just before a fold, so the cache breaks once for both. Basis: 42%
  of 3,530 read calls re-read the exact target, 46% of read bytes
  (`tools/rereads.py`).
- **Fold (agentkit `compactOldest`, `shaper.go:426-541`) [D].** Keeps at
  least the pristine tail: the last 6 user/assistant messages and 4 tool
  calls (`internal/harness/shaping.go` `shaperPolicy`), widened so no result
  loses its call.
  **Fold to 55% (USER 2026-11-18; agentkit v0.37.0 `FoldKeepPct`).** dun
  sets `FoldKeepPct = 55`: `foldKeep` keeps the newest entries verbatim, the
  most for which the rebuilt prompt (the prefix beside the entries, a
  summary reserved at `SummaryCap(8192)`, the pinned and kept entries, no
  LOD) fits 55% of target, found by halving, then widened for calls. Only
  the oldest entries are folded, so the window is half full after a fold,
  not nearly empty, and the next fold is far off. agentkit's choices, which
  differ from the first proposal: `fits()` is checked once more after the
  widening, and when the widening passes the band the fold falls back to
  the pristine tail; `tokensBefore <= 0` (a fold with no measured size)
  also keeps the pristine tail, as does a fold with no target (`Compact`)
  or a band that cannot hold more than the tail (little history). A huge
  newest result over the band by itself falls back the same way.
  `CompactionInfo` reports `SummaryTokens`, `PinnedTokens` and
  `KeptCount`/`KeptTokens`, all measured on the store after the fold, so
  summary + pinned + kept = `TokensAfter` (agentkit v0.39.0; before it the
  kept figure was taken before the summary call and could exceed
  `TokensAfter`). `TokensAfter` is what the next build sends, and both
  `TokensBefore` and `TokensAfter` leave out the system prompt and the
  tools (`CompactionStart.TokensBefore` still holds them).
  The fold notice: `compacted: oldest 429 entries → summary (3.1k), newest
  131 kept (45.5k) · history 273.3k → 48.6k tokens · over budget · 25m
  since the last`, the arrow at `TokensAfter`; `, pinned (1.2k)` follows
  the summary only when pinned entries are left beside it (cause: over
  budget, the endpoint cut a reply, or turn end or cold cache; `3rd fold
  this turn` and `26m since the last` when they apply)
  (`internal/harness/compaction.go`).
  **Stubs after a fold (USER 2026-11-20; agentkit v0.39.0
  `FoldMidLOD`).** dun sets `FoldMidLOD = true`: once a compaction marker
  exists, a build under target stubs old big tool results as the LOD rung
  does, so the kept band does not fill the window again soon. The boundary
  moves in `LODStep` steps, so the cache breaks once per step; pins,
  markers and the pristine tail stay verbatim. `foldKeep` measures with the
  stubs, so a fold keeps more entries. Not checked live. Measured in
  `internal/harness/foldkeep_test.go` (agentkit v0.39.0, `FoldMidLOD`
  on): 434 entries, target 190k, 235.8k history tokens before, 281
  folded, summary 6.0k, 154 kept (83.4k), the next round sends 97.2k with
  the system prompt (51% of target); without the band the same fold keeps
  7.
  Everything older becomes one summary of folded/8 tokens within [500, 8192], at most
  60% of target (`shaper.go:794-801`). The summarizer is told the open plan
  and the work in flight (`internal/harness/fold.go:31-41`); after the fold an aside lists
  what is still running (`internal/harness/fold.go:79-125`); the folded span goes to the
  session index: a file `<session>.folds/<key>.md` beside the log, synced
  into dund's `sessions` index, read back with `session_search`
  (`internal/harness/sessionindex_dund.go`, `rag.md` "Built: sessions index"). The first user message is pinned for
  children only (`internal/harness/harness.go:833`).
  **The current ask (2026-11-17).** On a long tool-heavy turn the pristine
  tail fills with tool entries, so the turn's user message lands in the older
  part and the fold subsumed it ("Your message carried no task"). dun side
  (`internal/harness/fold.go`): `noteAsk` remembers the latest `KindUser` entry (asides,
  notices and settle prompts are notifications, so they never count) and
  adds it to the plan as a node in progress, once per ask id, before the fold
  runs (`foldTask` is the hook); the fold is told the ask and must write
  `## Ask / Done / Learned / Next`, short, with the ask copied word for word;
  `foldRunner` checks each summary and asks once more with the failure stated
  (agentkit v0.29.0 has no hook between summary and commit, so the check
  wraps the Shaper's Runner); `foldRestoreNote` always carries the ask
  verbatim (cap 8000) and the open plan, keyed so a second fold in the turn
  replaces the first note. Open: pin the latest user entry in the Shaper
  itself (request to the agentkit owner). A fold's first request sends the
  session's prefix (agentkit v0.33.1 `Shaper.SessionPrefix`, set per build
  by `measuredBuild`): the round's system, tools and PromptCacheKey, then
  the history, so it reads the history **from the cache** up to the pinned
  latest user entry (`decisions.md`, "A fold shares the session's prompt
  cache"). A fold too big for one request is split (agentkit v0.34.0): the
  first request covers the head that fits, and the later stages keep the
  worker's system prompt ("You are a compaction worker") and read
  uncached. The next round re-reads the new prompt uncached from the marker
  on.
- **Rescue and overflow [D].** `internal/harness/rescue.go` folds when the endpoint refuses a
  prompt (≤2 passes, each entry's head only); `internal/harness/overflow.go` folds on a
  length cut only when the room left is under one reservation.
- **Sub-agents [D].** Context offloading first (`internal/harness/subagent.go:23-34`): the
  child's tokens die with it.

Not present: a `/compact`, a fold at a task boundary, a quality ceiling
below the window, a cold-cache trigger, a staged fold that reuses the
cache.

## What is measured

- `docs/measured/first-turn.md`: first request against pi, Claude Code and
  opencode. It says itself that a session's later context is not measured.
- `docs/measured/session-short-*.json`: short scripted sessions, prompt and
  cached tokens per request.
- `internal/harness/cachestats.go`: per build, the prefix kept and the rewrite cause; append
  rounds hit 98.5% on llama.cpp (`providers-and-cost.md:139-162`).
- **Terminal-Bench trials (`examples/harbor/jobs`) [M, 2026-11-14].** 357
  trials in 25 jobs: **1 compaction in all of them**; median peak context
  5k–44k per job, maximum 179k. The benchmark does not exercise folds.
  `sweep-…-base` vs `sweep-…-recap` scored 19/30 vs 24/30: Fisher exact
  p = 0.25, noise at this n; the recap arm made 2 recap calls in all.
- **Round ledger (`~/.dun/stats/rounds-2026-1{0,11}.jsonl`, 2,265 rounds)
  [M].** Total spend $2.58; the two long DeepSeek-flash sessions $1.12 (356
  rounds, peak 279,506) and $0.76 (279 rounds, peak 191,775). Cache share by
  prompt size: under 32k 88%, 32–100k 94%, 100–200k 98.7%, over 200k 94.9%.
  The first session spent 276 rounds over 100k ($0.91 at 97% cache). The two
  sessions have 9 prompt drops of more than 30% with `recaps` 0; the ledger
  has no cause field, so they cannot be attributed.
- **Session files [M].** 88 non-tmp session logs: 1 holds a fold marker, 8
  hold recap entries; 75 recap sidecars.

The canon (`~/doc`) says nothing about LLM context. What transfers:
`fleet.md` names cache hit rate as a value measure and counts retries to
tell a flake from a break; LEN-3 and LEN-8 (`standards.md`) say a run
records only what was seen and must be repeatable to be compared;
`observability.md` reports p50/p95/p99, not means.

## Outside evidence

Long context degrades quality [M]:

- Lost in the Middle: accuracy is U-shaped by the evidence's position.
  https://arxiv.org/abs/2307.03172
- RULER: of 17 models claiming 32k or more, about half hold quality at 32k.
  https://arxiv.org/abs/2404.06654
- NoLiMa: 11 of 13 models claiming 128k fall below half their short-context
  score at 32k; GPT-4o 99.3% → 69.7%. https://arxiv.org/abs/2502.05167
- Chroma, Context Rot: every one of 18 models degrades with length;
  distractors hurt; focused prompts of ~300 tokens beat ~113k full ones.
  https://www.trychroma.com/research/context-rot
- Length alone costs 13.9–85%, even with perfect retrieval and the
  irrelevant tokens masked. https://arxiv.org/abs/2510.05381

Ablations of agent context management [M]:

- JetBrains, "The Complexity Trap": observations are ~84% of a turn's
  tokens. Masking old observations (window 10) cuts cost 50–57%; LLM
  summaries 0–55%. Solve rate masking ≥ summary (Qwen3-Coder 480B 54.8 vs
  53.8; Gemini Flash 36.4 vs 31.4). **Summaries lengthen trajectories
  13–15%**; summary calls are up to 7.2% of a task's cost; a hybrid is 7–11%
  cheaper than either. https://arxiv.org/html/2508.21433v3
- ACON: peak tokens −26–54% with task success kept; small models gain up to
  46%. https://arxiv.org/abs/2510.00615
- Factory's probe evaluation (vendor-run, method published): Factory 3.70,
  Anthropic 3.44, OpenAI 3.35 of 5; **which files changed is the weakest
  dimension for all three (2.19–2.45)**.
  https://factory.com/news/evaluating-compression

What other agents do:

- Anthropic API context editing [D]: `clear_tool_uses_20250919`, default
  trigger 100k input tokens, keeps 3 tool pairs; `clear_at_least` exists to
  weigh the cache break.
  https://platform.claude.com/docs/en/build-with-claude/context-editing
  Its +29% (+39% with the memory tool) and −84% tokens are [V].
  https://claude.com/blog/context-management
- Anthropic's guide [D]: compaction keeps the summary and the 5 most recent
  files; clearing tool results is "the safest lightest touch"; sub-agents
  return 1–2k tokens.
  https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents
- Claude Code: auto-compact at ~83–89% of the window, old tool results first
  (third-party, version-dependent).
  https://codex.danielvaughan.com/2026/04/10/context-compaction-showdown-coding-agents/
- Codex [D]: `model_auto_compact_token_limit`, `compact_prompt`, a server
  compaction item. https://developers.openai.com/api/docs/guides/compaction
- OpenHands [V]: up to 2× cheaper per turn, 54% vs 53% solved.
  https://openhands.dev/blog/openhands-context-condensensation-for-more-efficient-ai-agents
- opencode ~96–99%, old tool output first, last 40k protected (third
  party). pi: window − 16k, keeps the last 20k. Hermes: 50–75% of the
  window, keeps the first 3 and last 20 messages. Aider [D]: a soft
  `--max-chat-history-tokens`, summarized by the weak model.
  https://aider.chat/docs/config/options.html
- Amp [D] removed compaction for a written handoff: summaries of summaries
  lost too much. https://ampcode.com/news/handoff
- Manus [V]: cache hit rate is the first metric; append-only; keep errors
  in; compress only what can be restored.
  https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus

## Trigger points and strategies

- **Elide before summarizing [M].** Tool output is the mass (84% in
  JetBrains; 122k of 180k in `internal/harness/rescue.go:47`). Removing an old result with a
  path to it costs no call and does not lengthen the run; a summary is
  lossy, costs a call, and does.
- **A token ceiling for quality, not a fraction of the window [O].** 80% of
  a 1M window is 800k, far past where every measured model degrades.
  Trigger at `min(window − reserve, Q)`, Q a per-model quality ceiling that
  is measured, not picked; the window rung stays as the safety rung.
- **At boundaries [O].** The best moment is a turn's end, a plan step done,
  a child's return: nothing in flight, and the next user message often
  starts a new cache run. Recap covers the current turn; a boundary fold
  covers earlier turns, which `decisions.md:94-95` gives to compaction.
- **When the cache is cold [O].** After a gap longer than the cache's TTL
  (Anthropic 5 min, 1 h optional; Fireworks not documented), the next
  request pays the whole prompt anyway, so a fold costs only its summary.
  `internal/harness/cachestats.go` already knows the gap.
- **Always keep [D, M]:** the task or current objective verbatim, the plan,
  in-flight work, user corrections, **the files changed** (Factory's weakest
  dimension, and the re-run finding behind stubs), unresolved errors, the
  last calls. What dun can compute (edits, commits, the last test verdict,
  the folded span's path) goes in from records, not from the summarizer,
  as `foldRestoreNote` does for jobs.

### When a fold pays back

C = prompt before, S = prompt after (prefix, summary, tail), O = summary
tokens; prices per token: p_i input, p_c cached read, p_w cache write (= p_i
under automatic caching, 1.25 p_i at Anthropic 5 min), p_o output, p_f the
price at which the fold reads C (p_i with its own system prompt, as now;
about p_c when it reuses the session's cache).

    saving per round = (C − S) · p_c
    overhead         = C · p_f + O · p_o + S · (p_w − p_c)
    break-even N*    = overhead / ((C − S) · p_c)

When the cache is cold anyway, subtract C · (p_w − p_c) from the overhead;
it is usually negative, so fold at once.

C = 200k, S = 20k, O = 4k:

| model | fold now (uncached read) | fold that reuses the cache |
|---|---|---|
| DeepSeek V4.1 flash, Fireworks ($0.30 / $0.006 / $1.20 per M) | $0.071, **N\* ≈ 65** | $0.012, **N\* ≈ 11** |
| same, C = 280k | N\* ≈ 61 | N\* ≈ 8 |
| Anthropic, read 0.1×, write 1.25×, output 5× | 243k·P, **N\* ≈ 13.5** | 63k·P, **N\* ≈ 3.5** |
| Anthropic, read 0.05× (Opus/Sonnet 5.5) | N\* ≈ 27 | N\* ≈ 6 |

Prices: https://docs.fireworks.ai/serverless/pricing.md,
https://platform.claude.com/docs/en/build-with-claude/prompt-caching [D].

The fold's own uncached read is 85% of its overhead on Fireworks. On flash
the corrallm session cost $1.12 in all: **compaction there is a quality
and latency decision, not a cost one**, and thresholds must not be tuned for
dollars.

## How to measure

Metrics, strongest signal first [O; method sources tagged]:

1. Task success on replayed real sessions (the outcome the user accepted:
   commit, green tests, "done").
2. Redo after a shrink: within 20 rounds of a fold, recap or rehead, calls
   that repeat an earlier (tool, normalized arguments). `tools/rereads.py`
   computes the unconditioned version.
3. Lost-fact probes [M, Factory]: questions whose answers were in the
   folded span (which file holds X, what the test said, what the user ruled
   out, what is running), graded by exact match against the sidecar.
4. User corrections after a shrink.
5. Trajectory length against the no-shrink arm [M, JetBrains].
6. Cost per completed task; latency per round and per task, p50/p95.
7. Cache recovery: tokens re-processed per shrink.

Token counts are diagnostics, not results.

### The harness: three levels

- **Corpus.** Session logs (entries; blobs over 8 KiB), recap sidecars, the
  round ledger, Harbor trials with rewards. Split held-out sessions by a
  hash of the id before looking; keep the two long sessions out of tuning.
- **Level 0, offline, no model calls** (the shape of `TestLearn_Replay`).
  Run the Shaper over each recorded prefix with candidate policies; record
  prompt size, prefix kept, shrink events, cost at ledger prices. Exact for
  cost and cache; says nothing about quality.
- **Level 1, fork and continue.** Cut long sessions where the prompt
  crosses 64k, 128k and 200k and at turn ends. Per policy: run the probes,
  then let the model take the next real user message for N rounds in a
  scratch worktree at the recorded commit, and compare with what was
  recorded (redo, divergence, the next commit or test pass). Deterministic
  graders first; an LLM judge, blind to the arm, only for intent.
- **Level 2, whole tasks.** Harbor, with an arm at a low ceiling
  (`DUN_CONTEXT_TOKENS` 32k/64k, stated in the result) so folds happen, and
  long scripted sessions of several tasks in one.
- **Repeats.** 3–5 seeds per (task, arm); paired per-task differences with
  bootstrap intervals or McNemar, not pooled means.

### What dun logs and lacks

Logged: per round prompt, cached, cache write, completion, latency, cost,
prefix, recaps (`internal/harness/roundstats.go:41-72`); per build the prefix kept and the
rewrite cause (`internal/harness/cachestats.go`, not in the ledger); the compaction note
(`internal/harness/compaction.go:12-20`); sidecars; Harbor rewards.

Lacking: a shrink event in the ledger (kind, tokens before and after, the
prefix the next build keeps); a turn outcome; a repeat marker on calls; the
window, budget and target in force per round. Not checked: whether the
Shaper's fold call reaches the ledger under a role of its own.

## Generally good instead of benchmaxing

Principles that hold across the evidence:

- Bound input at the source. Model-independent, restorable, no cache cost.
- Elide observations before summarizing reasoning [M].
- Every removal leaves a path the model can open [V Manus; D sidecars]. The
  fold is the one rung that does not put one in its summary.
- Shrink at boundaries, once, in one pass; append-only between shrinks [M
  cachestats 98.5%]. No tool-set changes mid-session.
- Keep what is cheap to keep and costly to rediscover.
- Length hurts quality before it hurts the budget [M].

Against overfitting: tune on some sessions and report on held-out ones;
two or more models (the ledger has DeepSeek flash and a local Qwen) and two
task shapes (short Terminal-Bench, long multi-task); repeats and paired
comparisons; distributions, not one mean; flakes counted apart. Prefer knobs
that follow from something (a share of what is folded, break-even rounds,
turn end) to bare constants; sweep a constant and take a flat stretch of the
curve, not its peak. Report what the policy's own calls cost. Keep a
regression set of sessions where a fold once lost something.

What is policy, what is measured:

- **Policy (decided, not tuned):** nothing destroyed (sidecar or abort);
  tool pairs whole; recap inside its turn; task, plan, corrections and
  changed files kept; every shrink reported; shrink at boundaries.
- **Measured per model (`/context config`):** the quality ceiling Q; the
  kept tail; the stub length (4,000); the summary share (1/8); the recap
  nudge sizes (20k, +15k).
- **Computed:** break-even rounds from prices; the cache TTL from
  cachestats' gap and hit data.

## Recommendations, ranked

1. **Shrink events and turn outcomes in the round ledger.** Makes the rest
   measurable and explains the 9 unattributed drops. Verify: each drop over
   30% in the long sessions has a cause. Risk: low; keep a line under
   PIPE_BUF.
2. **Level-0 offline replay.** Exact cost and cache curves for any policy,
   free. Verify: the current policy reproduces the ledger's prompt sizes
   within a few percent. Risk: blobs, resumes, the estimator against the
   provider's count.
3. **A fold that reuses the cache** (agentkit, through its owner): the
   session's system prompt, byte-identical tools, the instruction as the
   last user message, `tool_choice` none (still cached, `memory.md:89-94`).
   N\* 65 → 11 on flash, 13.5 → 3.5 on Anthropic. Verify: the fold round is
   90%+ cached. Risk: the model acts instead of summarizing; a staged fold
   cannot reuse the cache.
4. **A per-model quality ceiling Q**, elision first, fold last; start near
   128k for flash. The corrallm session carried over 100k for 276 rounds.
   Verify: Level 1 on held-out sessions. Risk: overfitting to two sessions.
   Depends on 1 and 2.
5. **Boundary and cold-cache folds**: at a turn's end fold earlier turns
   over 0.7·Q; on the first request after the TTL, fold over 0.5·Q. Verify:
   Level 0. Risk: Fireworks' and corrallm's TTLs are unknown. Depends on 1,
   2 and 4.
6. **The fold keeps more, from records**: files changed, commits, the last
   test verdict, the folded span's path, the latest objective pinned with a
   size cap, a tail measured in tokens. Verify: probes. Risk: a pinned "hi"
   or pasted log rides every round (`providers-and-cost.md:195-197`).
   Depends on 1 and 2.
7. **Redo-after-shrink** from `tools/rereads.py`, excluding reads after an
   edit to the same path.
8. **Harbor hygiene**: 3+ seeds, paired results with intervals, a
   low-ceiling arm, a held-out task list.

Leave alone: the exec clip, the 64k cap, recap with stubs, Rehead before the
fold, LOD's step and headroom. They follow the measured principles; changing
them for a benchmark would be benchmaxing.

Unknown: DeepSeek V4.1 flash's real window; Fireworks' cache TTL; Claude
Code's exact trigger (third-party sources only).
