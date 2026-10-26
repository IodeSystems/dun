# dun.iodesystems.com and the registry

USER 2026-10-26: a site for dun, like homelab-horizon.iodesystems.com, that
is also where dun gets its extension manifests, MCP presets and tool pins
from, so a change to one of them is not a dun release. Owner's calls:
served by hz, as homelab-horizon's own page is (first GitHub Pages; the
owner changed it the same day), source in `site/` in this repo, and the
registry carries extensions, MCP presets and tool pins.

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
- Every build reads the registry, a dev build too (you run one): the key
  and the registry's address are compiled into all builds
  (`release/key.go`). That is not the download location the release-only
  rule keeps out of dev builds: the tool files an entry names are
  downloaded by release builds only, as deps.json's are; a dev build's
  `doctor --fix` keeps using `go install`.
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
- `make site` builds `dist/site` (the page, the index), signs the index
  with the release key (`MINISIGN_KEY`) and verifies it with
  `release/minisign.pub`; `make site-push` uploads it with `hz-client site
  push` (`HZ_URL`, `HZ_TOKEN`), an atomic swap with rollback. It runs here:
  hz's API is on the home network, out of CI's reach.
- Owner's step, once: in hz, a static service `dun-site` with domain
  `dun.iodesystems.com`, `external_dns` on and
  `"proxy": {"static_root": "/var/lib/homelab-horizon/dun"}`; its token is
  in the service's Integration panel.

## Order of work

1. Done: `internal/registry`: the format, npm ranges (Masterminds/semver),
   verify, the cache, which entry fits.
2. Done: dun reads it (`registry.go`, `cmd/dun/registry.go`). The cached
   index is installed at process start; an engine refreshes it in the
   background for the next start, so a session's manifests never change
   under it. Only the bundled extensions' ids are overlaid (a registry
   entry for another id is not loaded). `/ext` shows a manifest's source
   (`registry: raglit 1.2.0 (for raglit ~0.2.1)`) and an entry that fit but
   did not load.
3. Done: `site/index.html`; the entries for today's built-in manifests
   (by `manifest_file`, so the repo's file is the one source), presets and
   pins (downloads `files_from` release/deps.json); `tools/mkregistry`,
   whose test builds the real entries in `make test`; `make site` and
   `make site-push`. Checked locally: the built index signed with the
   minisign CLI verifies with dun's own verifier.
4. The owner adds the hz static service; then `make site-push` publishes
   the first registry, and a dun build is checked against it.
