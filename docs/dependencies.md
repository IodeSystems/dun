# Dependencies and the release cascade

dun builds on one Go module and runs three tools. Each has its own repo and,
except agentkit, its own Claude session that owns it. dun asks the owner; it
does not tag, push or edit another repo unless the owner (Carl) says so.

| | kind | how dun pins it | owner session | aw4 project |
|---|---|---|---|---|
| agentkit | Go module (engine) | `go.mod` | none: dun-a8 edits it, with Carl's OK per change | — |
| mcpshell | binary, spawned | `release/supported.go` + `release/deps.json` | mcpshell-ea | mcpshell |
| poly-lsp-mcp | binary, spawned (shared daemon) | same | poly-lsp-mcp-78 | poly-lsp-mcp |
| raglit | binary, spawned | same | raglit-a2 | raglit |

Session names are as of 2026-10-24; `ListAgents` shows the current ones.

## What each pin means

- `go.mod`: the agentkit version dun compiles against. A `replace` to
  `../agentkit` is never committed (worktrees cannot resolve it, `decisions.md`).
- `supported.go` `Min`: the oldest tool version dun works with. Raise it only
  when dun starts depending on a flag or behaviour. The comment beside it
  names that dependency.
- `supported.go` `Recommended`: the version dun was tested with. `dun doctor
  --fix` installs it. It must be a tag or a pushed pseudo-version, never
  `latest`.
- `deps.json`: each tool's release tarballs (url, sha256, size) at its
  `Recommended`. Release builds embed it; `supported_test.go` checks the two
  agree. It is filled by hand (Makefile, `release:` comment).

dun also probes the tool at run time (`speaksIndex` markers in
`indexstate.go`), so a newer feature can be used before the pin moves.

## The cascade

Order: agentkit, then the three tools (independent of each other), then dun.

1. **Ask.** Send each owner session one message: pending or active work, a
   release planned, breaks for dun's use. Read `git log <last tag>..origin/main`
   and `gh run list` yourself first so the question is specific.
2. **agentkit.** If dun needs unreleased agentkit commits: Carl approves, push,
   tag `vX.Y.Z`, then `go get github.com/iodesystems/agentkit@vX.Y.Z` in dun.
3. **Tools.** The owner tags; its CI publishes the tarballs. Then in dun:
   set `Recommended` (and `Min` if needed) with a comment saying what the
   version brings, fill `deps.json` from the published tarballs, run
   `go test ./release/` and `make test`.
4. **dun.** Commit the pins (`chore(release): …`). Tagging dun is Carl's call.
   Then `gh workflow run release.yml -f version=vX.Y.Z`: it tests with the
   pinned tools, signs with the MINISIGN_SECRET_KEY secret and publishes.
   Check the published manifest with `minisign -V -p release/minisign.pub`.
   v0.1.1 (2026-10-25) was the first signed release; v0.1.0 is a tag with no
   release (its run failed in tests).

Nothing in the cascade pushes or tags without Carl saying so for that repo.
