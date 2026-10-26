# dun.iodesystems.com and the registry (design)

USER 2026-10-26: a site for dun, like homelab-horizon.iodesystems.com, that
is also where dun gets its extension manifests, MCP presets and tool pins
from, so a change to one of them is not a dun release. Owner's calls:
GitHub Pages, source in `site/` in this repo, and the registry carries
extensions, MCP presets and tool pins.

## Why

An extension manifest (`examples/extensions/raglit/extension.json`) names
the tool's arguments, which of its tools are hidden, and the tools and CLI
verbs its slash commands call. It is compiled into dun (`extension.go`,
go:embed). When raglit renames a tool, every dun before the fix is broken
until a dun release ships the new manifest. The same holds for the MCP
presets (`mcppresets.go`) and the tool pins (`release/supported.go`,
`release/deps.json`): a raglit point release needed a dun release to be
recommended.

## The registry

One signed file, `https://dun.iodesystems.com/registry/v1/index.json`, its
minisign signature beside it (`index.json.minisig`), signed by the release
key (`release/minisign.pub`, with the updater's key rotation).

```json
{"schema": 1, "published": "2026-10-26T10:00:00Z",
 "extensions": [
   {"id": "raglit", "version": "1.0.0", "tools": ">=0.1.0 <0.3.0", "dun": ">=0.1.3",
    "manifest": { … extension.json … }}],
 "presets": [
   {"id": "playwright", "version": "1.0.0", "dun": ">=0.1.3",
    "preset": {"description": "…", "command": "npx", "args": ["@playwright/mcp@latest", "--headless"]}}],
 "tools": [
   {"name": "raglit", "version": "1.0.0", "dun": ">=0.1.3",
    "min": "v0.1.0", "recommended": "v0.2.1",
    "files": {"linux_amd64": {"url": "…", "sha256": "…", "size": 123}}}]}
```

- **Ranges are npm's syntax** (`^1.0.0`, `~1.1.1`, `1.1.x`, `>=1.0.0 <1.1.1`,
  `1.0.0 - 1.1.0`, `||`). `tools` is the tool versions an extension entry
  works with: one entry can cover raglit 1.0.0 to 1.1.0, and 1.1.1 gets
  its own entry only when the manifest has to change (owner: less churn).
  `dun` is the dun versions that read the entry's shape.
- **version** is the entry's own (semver): the newest that fits wins.
- Everything is in the one file: entries are small, and one fetch with one
  signature is one thing to verify.

## In dun

- **Fetch:** at engine start, in the background, at most once a day;
  cached in `$DUN_HOME/registry/` with the signature, verified on every
  read. `DUN_REGISTRY=off` turns it off; `DUN_REGISTRY_URL` points it
  elsewhere (tests, staging).
- **Extensions:** for each id, the newest entry whose `dun` range has this
  dun and whose `tools` range has the installed tool's version (read from
  its build info, as `dun doctor` does) replaces the built-in manifest. A
  tool whose version is unknown (a checkout, `devel`) gets the newest entry
  for this dun. With no entry that fits, or no registry, the built-in one
  is used: dun works offline and as shipped.
- **Presets:** the entries for this dun replace the built-in list.
- **Tool pins:** an entry for this dun raises Min and Recommended above the
  built-in ones (never lowers them); `dun doctor --fix` downloads from its
  files, sha256 checked, as from deps.json now.
- A dev build (`dun dev`) takes entries regardless of their `dun` range.
- `/ext` and `dun doctor` say where each manifest came from: built in, or
  registry entry and version.

## The site

`site/` in this repo:

- `site/index.html`: the dun page (what it is, install, the release, links),
  built like homelab-horizon's `docs/index.html`.
- `site/registry/v1/`: one JSON file per entry (`extensions/raglit/1.0.0.json`,
  `presets/playwright/1.0.0.json`, `tools/raglit/1.0.0.json`).
  `tools/mkregistry` checks each against this dun (it parses, its ranges
  parse, an extension manifest loads) and writes `index.json`.
- `.github/workflows/site.yml`: on a push to main that touches `site/`, or
  by hand: build the registry, sign it with `MINISIGN_SECRET_KEY`, verify
  it with the committed key, and deploy to GitHub Pages.
- Owner's steps: turn on Pages for the repo with GitHub Actions as its
  source and `dun.iodesystems.com` as its domain, and add the DNS record
  (CNAME `dun.iodesystems.com` → `iodesystems.github.io`).

## Order of work

1. `internal/registry`: the format, npm ranges, verify, cache, resolve.
2. dun reads it: extensions, presets, tool pins, and says where each came
   from.
3. `site/`: the page, the entries for today's built-in manifests, presets
   and pins, `mkregistry`, the workflow.
4. The owner turns on Pages and adds the DNS record; then the first
   registry is published and checked from a dun build.
