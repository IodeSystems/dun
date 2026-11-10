---
name: release
description: Prepare and run a dun release cascade — check dependencies and in-flight work, ask each owner session, pin the new versions, test, and (only when Carl asks in this conversation) tag, sign and publish. Use for "/release", "prepare a release", "cut vX.Y.Z", "release cascade".
---

# /release — the dun release cascade

dun builds on Go modules and spawns tools; each lives in its own repo with its
own owner session. A dun release is the last step of a cascade: dependencies
first, then dun. The rules for each pin are in `docs/dependencies.md`; read it
first. This skill is the procedure.

**Hard rule.** Nothing in another repo is tagged, pushed or edited by this
session: its owner does it, with Carl's OK for that repo. dun itself is tagged,
pushed, signed and published only when Carl asks for it in this conversation
(an earlier "go" does not carry over).

## 0. Branches (USER 2026-11-09: "pick a system and keep it")

- **`main` is the development line.** Work we have committed to goes on
  `main`, merged once its checks pass. There is no `next`, `dev`, `final` or
  staging branch.
- **Work not yet committed to** goes on one short-lived topic branch per
  piece, named for the work (`ragcore`, `fix-dead-server`), never for a
  stage (`next`, `final`, `-v2`). It is either merged into `main` or deleted.
  Delete a branch (and its worktree) once it is merged.
- **A release is an annotated tag on `main`** (`vX.Y.Z`). Tags are
  branchable: a patch for a past release goes on `release/vX.Y`, cut from
  its tag only when a patch is needed, with fixes cherry-picked from `main`,
  and tagged `vX.Y.Z+1` from there.

## 1. What dun depends on

| dependency | kind | pinned in | owner session (check `ListAgents`) |
|---|---|---|---|
| agentkit | Go module (engine) | `go.mod` | agentkit-f2 |
| openai-claude-api-shim | Go module (dun imports it directly; also under agentkit) | `go.mod` | agentkit-f2 |
| gwag | Go module | `go.mod` | — (ask Carl) |
| mcpshell | spawned binary | `release/supported.go` + `release/deps.json` | mcpshell-5c |
| poly-lsp-mcp | spawned binary (shared daemon) | same | poly-lsp-mcp-1c |
| raglit | spawned binary (bundled extension) | same | raglit-a4 |
| corrallm | the LLM gateway (runtime, not pinned) | — | corrallm-2c |

Session names change: list them with `ListAgents` and match by repo. A repo
with no live owner session: ask Carl.

## 2. Check in-flight work yourself first

For each repo under `~/local/src/iodesystems/<repo>`, so the question to its
owner is specific:

```sh
t=$(git -C $d describe --tags --abbrev=0)
git -C $d rev-list --count $t..HEAD          # commits since the last tag
git -C $d rev-list --count $t..@{u}          # of them pushed
git -C $d status --short                      # dirty?
git -C $d log --oneline $t..HEAD | grep -E '!:|BREAKING'   # breaking changes
(cd $d && gh run list -L 3)                   # CI (it runs on push only)
```

And dun's own: `git log --oneline <last dun tag>..main`, the branches not yet on
main, and whether main builds against its pinned versions **without** a local
`go.work` (`GOWORK=off go build ./...`): a branch built against an untagged
agentkit cannot land until that agentkit is tagged.

## 3. Ask each owner (one message each)

Send each owner session one message (`SendMessage`), with what you found:

- what is in flight or not done, and whether it goes in this release;
- the release it plans: version and commit (a `!` commit means a breaking
  bump, a minor one in 0.x);
- anything that breaks dun's use (name how dun uses it: the flags it passes,
  the API it imports).

Say the order and that nothing is tagged without Carl's OK. Wait for every
answer; record them (versions, commits, breaking notes) before going on.

## 4. The cascade order

1. **shim**, then **agentkit** (agentkit pins the shim). The owner tags after
   Carl's OK. Then in dun: `go get github.com/iodesystems/agentkit@vX.Y.Z` and
   the shim if dun imports it directly; `go mod tidy`.
2. **The tools** (independent of each other). The owner tags and uploads the
   tarballs (mcpshell: `make release` + `gh release upload` from a clean
   checkout of the tag; its CI only tests). Then in dun:
   - `Recommended` in `release/supported.go`, with a comment saying what the
     version brings and any break; raise `Min` only if dun now depends on a
     flag or behaviour;
   - `release/deps.json`: url, sha256 and size of each published tarball.
     Download them and hash them yourself (`sha256sum`, `stat -c %s`); do not
     copy a hash you did not compute;
   - the tool's registry entry (`site/registry/v1/tools/<tool>/*.json`)
     `recommended`;
   - dun's notes on the tool's surface if it changed (e.g. `mcp.go`'s eval
     notes for mcpshell).
3. **dun.** All pins committed as `chore(release): …`. Then:
   - `GOWORK=off go build ./... && GOWORK=off go vet ./...`;
   - `make test` and `go test ./release/` (check exit codes; never pipe a
     test run through `tail`);
   - optional smoke: a few Terminal-Bench tasks on a build of the pinned set
     (`examples/harbor`), within the budget Carl set.

corrallm is not pinned; its release is its own. Ask it only what changes
reach dun's use of the gateway.

## 5. Land, tag, sign, publish — only when Carl asks

When Carl says to release vX.Y.Z (in this conversation):

1. The release is cut from `main` (section 0): the tested commit is on
   `main` already; a patch release is cut from `release/vX.Y`. Never rebase
   or force a pushed branch.
2. Pick the version: minor for features or breaking changes in 0.x, patch for
   fixes only. Last tags: `git tag --sort=-creatordate | head`.
3. Tag the clean commit: `git tag -a vX.Y.Z -m "dun vX.Y.Z"`; push main and the
   tag: `git push origin main vX.Y.Z`.
4. **Publish, signed by CI** (preferred):
   `gh workflow run release.yml -f version=vX.Y.Z`. It tests with the pinned
   tools (deps.json's linux_amd64 tarballs, checked by sha256), builds, signs
   the manifest with the `MINISIGN_SECRET_KEY` secret (key 2CC27A171DE03D42)
   and publishes. Watch it: `gh run watch`.
   **Local fallback** (only if Carl says so): `make release
   RELEASE_VERSION=vX.Y.Z MINISIGN_KEY=<path to the secret key>` builds
   `dist/vX.Y.Z/` and signs `dun-manifest.json`; then `gh release create
   vX.Y.Z dist/vX.Y.Z/*`. A manifest without a signature is useless: no
   release build accepts it.
5. **Verify** the published manifest:
   `minisign -V -p release/minisign.pub -m dun-manifest.json`
   (download the asset and its `.minisig` first). Report the release URL and
   the verification result.

## 6. Report

A table per repo: last tag → new tag (or "no change"), commit, what it brings,
breaking notes for dun, who tagged. Then dun's pins, test results, and the
published release and its verification — or exactly which step is waiting and
on whom.
