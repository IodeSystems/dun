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
- Sub-agents use the parent's provider unless the spawn names another
  logged-in one (`agent{provider, model}`, `--child-provider`; USER
  2026-10-30). `clientCache` keys its clients by endpoint and model, so
  children on one model share one client and one rate limit. A named provider
  that is not logged in fails the spawn: there is no fallback to the parent's
  client, which would run, and bill, where nobody asked. The child gets its own
  `Endpoint`, so its context window is its model's, not its parent's. Its
  usage counts in the session's spend, but its key's budget and limits are
  dropped (`NoteAs`): they are not the session key's.
- `dun doctor` is read-only and prints a fix per finding: providers (logged in,
  model chosen, each model list with its real failure), git on PATH and on
  the service's PATH, dun's own tools (pinned, your build, missing, and unused
  copies left on PATH or beside dun), whether dund's socket answers this user (tests peer
  credentials), the service, a self-updating build's Go version.

## Model facets

Code: `internal/dunconfig/facets.go`. Every model dun uses is a role in
`facets` in `config.json`: `main` (the session), `child` (sub-agents),
`vision` (describes chat images for a model that cannot read them, and OCRs
pages for dund), `embed`, `segment`, `identity` (dund's retrieval,
`rag.md`). Each is `{provider, model, effort}`: `provider` is a saved
provider's name (empty: the active one; for `child`, the session's), `effort`
an effort level (empty: the saved level; the levels are under "Roles and
effort" below).

```json
"facets": {
  "main":   {"provider": "work", "model": "chat-standard"},
  "child":  {"provider": "fast", "model": "chat-fast", "effort": "low"},
  "vision": {"provider": "openrouter", "model": "qwen2.5-vl-7b-instruct"},
  "embed":  {"provider": "corrallm", "model": "local-nomic-embed-text"}
}
```

- **One resolver.** The engine, dund's retrieval (`ragd.Resolve`), `/model`
  and `dun doctor` all call `DunConfig.Resolve`. Precedence, field by field
  (a layer that names only a model keeps the endpoint of the layer below):
  CLI flag > env > `facets.<role>` > the old top-level fields > the role's
  default. Flags: `--url`/`--model`/`--key` (main), `--child-provider`/
  `--child-model`/`--child-effort` (child). Env: `DUN_URL`/`DUN_MODEL`/
  `DUN_LLM_KEY` (main only). Old fields: `model` (main), `vision_model`/
  `vision_provider` (vision). Defaults: embed is `local-nomic-embed-text` on
  the active provider only if it lists that model (else off, search is
  full-text only); child is main's model; the rest are off.
- **Old configs keep working.** Loading reads the old fields as fallbacks
  and never rewrites the file. `/model use`, the picker, `dun --setup` and
  the web's provider `use` write `facets.main`; they also write the old
  `model` and make the provider active, so an older binary on the same
  config runs the same model. Logout of main's provider clears `facets.main`
  (as it cleared `model`); rename moves every facet on it.
- `facets.main.effort` applies only when `/effort` has saved no level
  (`effort.json`), so `/effort` keeps working as before.
- Bare `/model` opens the roles window (`tui/roles.go`): one row per role,
  its model on its provider and where it came from (`flag`, `env`, `config`,
  `config, old field`, `default`, or `session` for a `--session` pick), and
  the highlighted role's purpose. Keys: `enter` opens the picker for that
  role (main switches the session as before; another role is saved to
  `facets.<role>`, and `alt+enter`, the session-only pick, is refused for
  it), `esc` in that picker comes back to the window, `p` runs the role's
  probe, `x` resets it (removes `facets.<role>`; the row then shows its
  fallback), `esc` closes. What a save or a reset did is said on the
  window's hint row and put in the conversation at the close, into the rows
  the window gives back, so no row above the window moves (no bounce). The
  retrieval roles (`embed`, `vision`, `segment`, `identity`) take effect
  when dund restarts; there is no reload.
  The external raglit server has its own roles (`/rag setup`). Typing
  `/model ` completes `use`, `probe`, `caps`; after `use` the roles and
  `<provider>/<model>` (models from the picker's rows when fetched); after
  `probe` the roles. `/model use <facet>
  <provider>/<model>` sets one role; `/model use <provider>/<model>` and
  `/model use <provider> <id>` set main. The text before the first `/` is a
  provider only when one of that name is saved, so `anthropic/claude-x` on
  OpenRouter is a model id. `/model probe <facet>` runs the probes: chat
  probes for main and child, OCR for vision (`ragd/probe.go`). `dun doctor`
  prints one line per role.

## Model tiers

Code: `internal/dunconfig/tiers.go`; the decision is "Model tiers" in
`decisions.md`. Sub-agents and side calls run on one of three tiers, set in
`config.json` beside `facets`:

```json
"tiers": {
  "fast":  {"provider": "local", "model": "chat-fast", "effort": "low"},
  "think": {"provider": "work", "model": "chat-big", "effort": "high"}
},
"default_tier": "work",
"side_calls": {"suggest": "fast"}
```

- `agent{node, tier}` starts a child on a tier; `model` overrides it. A tier
  with no model (here: work) is the child default: `facets.child`,
  `--child-model`, else the session's model. With no tier set the agent
  tool is as it was.
- `use` (the agent tool's guidance per tier) and `default_tier` may also be
  set in a repo's dun.json; a provider, model or effort there is ignored
  and doctor warns. A repo's `default_tier` decides which of this
  machine's tiers a spawn that names none uses.
- The engine resolves the tiers at its start: a change in config.json
  applies from the next engine start. A tier on a provider that is not
  logged in is warned about then; its spawns run on the child default and
  say so in the result.
- Flags `--tier-fast`/`--tier-work`/`--tier-think` (`[provider/]model`) and
  `--default-tier`; env `DUN_TIER_FAST`… and `DUN_DEFAULT_TIER`.
- `side_calls` maps a side call (suggest, fold, remember, rescue,
  plan_summary, journal, worklog, commit, rephrase, rules, learn, repaint)
  to a tier or `session`. The defaults keep the calls sized from the
  session's window (suggest, fold, remember, rescue) on the session's
  model. One put on a tier runs there only when the tier model's window
  (its capability record, else its model list) holds the input; otherwise
  on the session's model, logged. A tier call refused before it replies is
  asked once more on the session's model. Spend from a side call on a tier
  is filed under the role `model`, as a child's is.
- `/model` lists the tiers the engine runs (its settings event); `dun doctor` the tiers, the side calls and the
  warnings.

## Roles and effort

Code: `internal/harness/roles.go` (resolver), `profile.go` (model
profiles), `effortclamp.go` (the clamp, over agentkit's `EffortFor`); the
decision is "Roles and model profiles" in `decisions.md`.

- Every request is for a role: `main`, `child:fast|work|think`, or a side
  call kind. Each resolves to a model and a wanted effort by one order:
  the call's argument > a flag or env var > the repo's dun.json (only
  `use`, `default_tier`) > `$DUN_HOME` (tiers, facets, side_calls,
  `/effort`) > the role's default.
- Defaults: main unset (nothing sent), fast `lowest`, work unset (the
  child loads the saved `/effort` level), think `high`, fold and rescue
  `lowest`, every other side call unset. A tier's default applies only
  when the tier names a model.
- Levels: `default`, `none` (`off` is read as it), `minimal`, `low`,
  `medium`, `high`, `xhigh`, `max`; config and flags also take `lowest`,
  the lowest level the model accepts (none, or minimal and up on a model
  that cannot turn thinking off).
- The wanted level is clamped to the model it runs on (agentkit's
  `Client.Effective`): `/effort`'s picker marks levels the session's model
  does not take, with agentkit's reason; `/status` shows the saved level
  and what is sent.
- `/model` and `dun doctor` show the role table: role, model, wanted,
  sent, reason, sources. The sub-agent strip and `dun ps` show each
  child's level as sent.

## Spend meter

Code: `internal/harness/spend.go`, `ledger.go`. Source: agentkit reads what a gateway reports
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
- Rollups in /usage (USER 2026-10-28; a screen since 2026-11-04): the
  Usage tab has the session's spend this hour, today and over the interval;
  the Spend tab has the machine's (the ledger, every key) this hour, today,
  this week, this month, this year, over the interval, all time, and this
  month by model. The
  **interval** is the key budget's period (one `Interval` back from
  `ResetAt`) when the gateway states both, else the last 7 days. Hour
  buckets, so an interval counts from the start of its first hour. "key
  spend" is the budget period's when a reset is stated, else all time
  (agentkit's `KeyBudget.Spend`).

## Cache statistics

Code: `internal/harness/cachestats.go`. The question behind it (USER 2026-10-20): was a cache
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

- `shaperPolicy(window)` (`internal/harness/harness.go`) builds the policy by hand instead of
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
- `FoldUserIntoToolResult` comes from the provider:
  `providers: [{…, "fold_user_into_tool_result": true}]`. Default off. A
  message the person types while a tool runs is a user message after that
  round's tool results (`midreply.go`). Some chat templates (some llama.cpp
  ones) reject a user message right after tool results; for such a
  provider, turn this on: the words then go at the end of the last tool
  result, as `[the user wrote, while this ran] <text>`. Read when the
  engine starts, for the endpoint it starts on (`foldUserIntoToolResult`,
  `cmd/dun/config.go`).

## Re-heading (`internal/harness/rehead.go`)

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
