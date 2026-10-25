# Providers, cost and context shaping

Why providers, the spend meter and the prompt-size machinery work as they do.
Related: `surfacing.md` (what is injected), `decisions.md` ("Bounding what the
model reads").

## Providers are logins

(USER 2026-10-22.) Code: `cmd/dun/config.go`, `cmd/dun/model.go`,
`cmd/dun/login.go`, `cmd/dun/nologin.go`.

- **There is no built-in provider.** A hard-coded fallback endpoint was removed.
  Logged out, or with no model, the engine still starts, so a client can log in
  through it, and it refuses a turn with what to do. It contacts nothing.
- `config.json` holds `providers: [{name, url, key, kind}]` and `provider`. The
  top-level `url`/`key`/`model` stay the active provider's and are rewritten on
  every save, so an old config means the same thing and an old binary still
  runs. A legacy top-level url/key counts as one logged-in provider. `kind`
  exists for later; `openai` is the only one. Every provider is assumed to be
  OpenAI-compatible with a Bearer key, so a gateway needing other headers
  (Azure `api-key`, an org header) will not list models or chat.
- `/model` asks every provider for `/v1/models` at once and filters as you
  type; every word must appear in "provider model". A failing provider shows
  one line with the HTTP status and body or the network error. `/model use
  <p> <id>` saves a model typed by hand, for a gateway with no list.
- **One implementation for all clients.** `applyProviderOp` does add, edit,
  remove and use. The TUI calls it directly because the reason it exists was a
  bad provider entry on a corporate gateway that stopped the engine, and with
  it `/model`. Web and phone go through the engine (control `provider`). Edit
  keeps the saved key unless a new one is typed. Removing the default moves it
  to the next provider; only the last provider is refused. Keys never leave the
  machine: events carry "key set", not the key, and a typed line containing a
  key is refused on the web. Provider ops and listings run under one lock, or a
  stale listing could land after an edit.
- Sub-agents use the parent's provider: `clientCache` is built from the
  parent's url and key and picks only a model.
- `dun doctor` is read-only and prints a fix per finding: providers (logged in,
  model chosen, each model list with its real failure), tools on PATH and on
  the service's PATH, whether dund's socket answers this user (tests peer
  credentials), the service, a self-updating build's Go version.

## Spend meter

Code: `spend.go`, `ledger.go`. Source: agentkit reads what a gateway reports
(`Usage.Cost`, `Limits`, `KeyBudget`) and calls `Client.OnUsage` per response.

- `SpendMeter.Note` is attached to the main client **and every client the
  per-model cache builds**, so the session total covers turn rounds, side calls
  that borrow the session's runner (suggest, rephrase, commit, rescue, memory
  fork) and sub-agents, on whatever model. A runner that is not an
  `*llm.Client` is not seen. "Last turn" means since the previous turn ended.
- **"Not reported" is never $0.** A provider that says nothing about money must
  not read as free; this holds in the meter, the ledger and the wire
  (`proto.Spend`, absent = not reported).
- Limits are one latest snapshot across models. Key budget merges per-response
  LiteLLM headers with a poll; only the poll supplies the reset time. Stream
  headers carry key spend from before the call.
- Poll rules (`PollBudget`): at start, every 5 min, and after a turn when the
  last read is at least 30 s old; it stops once the endpoint has no budget.
- The status line warns under 10% left (dim) and colours under 2% or empty.

## Cache statistics

Code: `cachestats.go`. The question behind it (USER 2026-10-20): was a cache
miss ours (we rewrote the prefix) or the server's (the slot was taken)?

- Each build is compared with the previous one: leading byte-identical messages
  kept, append-only or rewrite, and the rewrite cause (fold, rehead, recap,
  tools, other). Each round's usage delta is filed under the build before it:
  first, back-to-back append (under 1 s since the last response), append after
  a gap, rewrite. The 1 s line comes from aw4's measurement: 68% of next
  requests leave within 0.1 s, 78.8% of gaps are under 1 s, under 2% end past
  about 0.75 s.
- Tools are compared by hash; the kept prefix is measured over messages only,
  because where tools land in the rendered prompt is the chat template's
  business.
- Measured live (llama.cpp behind corrallm): append rounds hit 98.5%, even
  after a 70 s gap with no other traffic. **Toggling `/rag on` mid-session was
  a full re-read** (tool set changed, message 0 changed because the system
  prompt names the tool families; 0 of 5049 chars kept). Tool-family toggles
  therefore cost a whole prompt; keep them rare. aw4 found name-sorted tools
  broke its cache for the same reason: keep tool order stable and order the
  prefix most-shared to least-shared.
- Usage `Cached` depends on the provider reporting it. The prefix comparison
  costs one sha256 per message per build (not measured).

## Token meter

- The meter (`tokencal.go`, `promptMeter`) maps chars to tokens by fitting
  prompt chars against reported prompt tokens. **Before the fit has three
  points it counts the tool-schema bytes too** (`noteToolChars`): the one-point
  ratio is (message + schema chars) / prompt tokens, and the pre-fit overhead is
  schema chars over that ratio. Why: the first live round read 4910 chars ->
  4830 tokens = 1.02 chars per token, because about 2500 of those tokens were
  schemas, and every early estimate (shaping budget, `/context`, surfacing stats)
  ran about 4x high. Asking the endpoint for an exact count was not an option:
  this endpoint does not count exactly (`system_exact` false in every run). The
  schema bytes are known, so they are used.
- Without a context window (no `DUN_CONTEXT_TOKENS` and none stated by the
  server) dun does no shaping at all.

## Shaper policy

- `shaperPolicy(window)` (`harness.go`) builds the policy by hand instead of
  using agentkit's `PolicyFor`: `PolicyFor(0)` is an empty policy, and dun
  builds its Shaper before the window is known, which would drop the
  LOD (truncate old results) rung for sessions that start without a window.
  `TestShaperPolicy_CoversPolicyFor` fails when `PolicyFor` sets a field dun
  leaves at zero, so a new agentkit knob is noticed. Reason for the guard: aw4
  blew its window twice in one day by forgetting one field.
- `MaxToolResultChars` (64k) is the backstop for one result wherever it sits.
  LOD skips the preserved tail, so a huge recent result would ride every round
  until it aged out (aw4: one 191 623-char read). exec clips at 20k itself;
  `node_read` and MCP results did not.
- `ToolLOD`: exec uses `LODEnds` (head and tail), because a head-only stub
  drops the verdict line of a test run. The map is keyed by real tool names and
  a test fails on a rename. `PinFirstUser` is set for sub-agents only: their
  first message is the brief, while a root session's may be "hi" or a pasted
  log, and a pinned entry rides every round.

## Re-heading (`rehead.go`)

Measured first, over dun's own sessions (`tools/rereads.py`, 99 sessions,
3 530 read calls: exec `cat/head/tail/sed/nl`, `node_read`, `get_document`):

- A path already read in the session: 63% of calls (overstated: two
  `node_read` symbols of one file count).
- The exact same target again: 42% of calls and 46% of read bytes (9.5 of
  20.7 MB); `node_read` repeated 60% of the time, exec 8%.

Some repeats follow an edit, so the older copy is stale as well as large. The
older copy becomes a one-line stub, the original goes to a sidecar beside the
session file (nothing is destroyed; a sidecar that cannot be written aborts the
stub), and the newest copy stays whole. It runs through `Shaper.Rehead`, only
when the prompt is still over budget after LOD and just before a fold, because
rewriting earlier context breaks the cache from that point and the fold breaks
it anyway. Only plain reads count: `node_read`, or one `cat/head/tail/nl/sed -n`
with no pipe, chain or redirect, and results of at least 1000 chars. Live check:
over budget with two reads of an 18k file in the preserved tail, re-heading
stubbed the older copy and the prompt fit with no fold.
