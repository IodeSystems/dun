package dun

// agents_hook.go — the AGENTS.md guard for poly-lsp-mcp.
//
// When the code server (poly-lsp-mcp) is started, dun writes a small bash
// script to a temp file and arms it as the server's file-access hook. The
// script checks for an AGENTS.md in the target file's directory chain (up to
// the workspace root) and, if found, cancels the operation with a note telling
// the model to read the file first. This is the "onFileAccess" hook the
// poly-lsp-mcp Server supports via --hook / $POLY_LSP_HOOK.
//
// Session memory: the hook records which AGENTS.md files have been read in
// this session (by path+mtime) in a state file dun names in $DUN_AGENTS_STATE:
// the workspace's agents_read in $DUN_HOME/projects/<id>/ (or the workspace's
// overlay there, for a linked worktree — see project.go). It used to be
// $root/.dun/agents_read, a file in the repo. Once an AGENTS.md has been read,
// subsequent accesses to files under it proceed without the guard. The state
// file is per-workspace (not global), so a session in one worktree does not
// unlock the guard for another.
//
// Two layers, access-driven by design. The ROOT AGENTS.md is standing context:
// rules.go puts it (with CLAUDE.md and what they import) in the system prompt
// so the model has the project's rules from the first message, and a system
// block survives compaction so a fold never silently drops them. NESTED AGENTS.md files are the guard's job — they
// surface on access (read the one for the directory you are touching), not in
// the system prompt. The rules are only re-supplied when a path is accessed,
// never on an arbitrary event like a compaction.
//
// The script is embedded in the binary so it works regardless of where dun
// was installed. The temp file is cleaned up when the server stops (or when
// the process exits — the OS reclaims it).

import (
	"fmt"
	"os"
	"path/filepath"
)

// agentsHookScript is the policy executable. It must be bash-compatible and
// exit 0 (proceed) or 2 (cancel, stdout = note). See tools/agents_hook.sh for
// the human-maintained copy; this is the embedded runtime copy.
const agentsHookScript = `#!/usr/bin/env bash
set -euo pipefail
op="$1"; target="$2"
target_base="$(basename "$target")"

# Find the workspace root: the nearest ancestor with a .git file or dir.
root=""
dir="$(cd "$(dirname "$target")" 2>/dev/null && pwd || echo "")"
while [[ -n "$dir" && "$dir" != "/" ]]; do
  [[ -e "$dir/.git" ]] && { root="$dir"; break; }
  dir="$(dirname "$dir")"
done
[[ -z "$root" ]] && exit 0

# The state file tracks which AGENTS.md files have been read in this session.
# dun passes it in DUN_AGENTS_STATE ($DUN_HOME/projects/<id>/…/agents_read) —
# never a file in the repo. Run by hand without it: a per-user temp file.
state="${DUN_AGENTS_STATE:-${TMPDIR:-/tmp}/dun-agents-read-$(id -u)}"

# mtime: GNU stat (Linux) spells it -c %Y, BSD stat (macOS) -f %m.
mtime() { stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null || echo 0; }

# is_read: returns 0 if the given AGENTS.md path+mtime is in the state file.
is_read() {
  local p="$1"
  local mt
  mt="$(mtime "$p")"
  [[ -f "$state" ]] || return 1
  grep -qxF "$p $mt" "$state" 2>/dev/null
}

# mark_read: record that an AGENTS.md was just read.
mark_read() {
  local p="$1"
  local mt
  mt="$(mtime "$p")"
  mkdir -p "$(dirname "$state")" 2>/dev/null
  echo "$p $mt" >> "$state"
}

# Never block reading/editing the AGENTS.md itself.
# On read, mark it so subsequent accesses to files under it proceed.
if [[ "$target_base" == "AGENTS.md" || "$target_base" == "agents.md" ]]; then
  if [[ "$op" == "read" ]]; then
    mark_read "$target"
  fi
  exit 0
fi

# Walk from the target's directory up to the workspace root looking for
# AGENTS.md (case-sensitive first, then lowercase). Take the NEAREST one.
found=""
dir="$(cd "$(dirname "$target")" 2>/dev/null && pwd || echo "")"
while [[ -n "$dir" && "$dir" != "$root" ]]; do
  for n in AGENTS.md agents.md; do
    if [[ -f "$dir/$n" ]]; then
      found="$dir/$n"
      break 2
    fi
  done
  parent="$(dirname "$dir")"
  [[ "$parent" == "$dir" ]] && break
  dir="$parent"
done
if [[ -z "$found" ]]; then
  for n in AGENTS.md agents.md; do
    if [[ -f "$root/$n" ]]; then
      found="$root/$n"
      break
    fi
  done
fi

# No AGENTS.md in the chain: proceed.
[[ -z "$found" ]] && exit 0

# An AGENTS.md exists. If it has already been read in this session, proceed.
if is_read "$found"; then
  exit 0
fi

# Not yet read: cancel with a pointer.
relpath="${found#"$root"/}"
echo "AGENTS.md guard: read $relpath before $op-ing $(basename "$target"). It contains project-specific rules, conventions, or constraints that apply to this path. Read it, then retry the operation."
exit 2
`

// writeAgentsHook writes the embedded hook script to a temp file and returns
// its path. The caller is responsible for cleanup (os.Remove).
func writeAgentsHook() (string, error) {
	f, err := os.CreateTemp("", "dun-agents-hook-*.sh")
	if err != nil {
		return "", fmt.Errorf("agents hook: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(agentsHookScript); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("agents hook: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("agents hook: %w", err)
	}
	return f.Name(), nil
}

// agentsHookEnv returns the env entries that arm the hook on the code server
// for workspace — POLY_LSP_HOOK and the state file — or nil if the hook could
// not be written.
func agentsHookEnv(workspace string) []string {
	path, err := writeAgentsHook()
	if err != nil {
		return nil
	}
	env := []string{"POLY_LSP_HOOK=" + filepath.Clean(path)}
	if state := agentsStateFile(workspace); state != "" {
		env = append(env, "DUN_AGENTS_STATE="+state)
	}
	return env
}

// agentsStateFile is where the hook keeps what has been read for workspace,
// or "" when its project cannot be registered (the hook then falls back to a
// temp file — the guard still works, its memory just is not per-workspace).
func agentsStateFile(workspace string) string {
	p, err := EnsureProject(workspace)
	if err != nil {
		return ""
	}
	return p.AgentsReadFile()
}
