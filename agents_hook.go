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
// this session (by path+mtime) in a state file at $root/.dun/agents_read.
// Once an AGENTS.md has been read, subsequent accesses to files under it
// proceed without the guard. The state file is per-workspace (not global),
// so two dun sessions on the same repo do not share it.
//
// Two layers, access-driven by design. The ROOT AGENTS.md is standing context:
// rootAgentsMDBlock puts it in the system prompt so the model has the project's
// rules from the first message, and a system block survives compaction so a fold
// never silently drops them. NESTED AGENTS.md files are the guard's job — they
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
state="$root/.dun/agents_read"

# is_read: returns 0 if the given AGENTS.md path+mtime is in the state file.
is_read() {
  local p="$1"
  local mt
  mt="$(stat -c %Y "$p" 2>/dev/null || echo 0)"
  [[ -f "$state" ]] || return 1
  grep -qxF "$p $mt" "$state" 2>/dev/null
}

# mark_read: record that an AGENTS.md was just read.
mark_read() {
  local p="$1"
  local mt
  mt="$(stat -c %Y "$p" 2>/dev/null || echo 0)"
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

// rootAgentsMDBlock returns the workspace's root AGENTS.md as a system-prompt
// block, so the model has the project's standing rules in context from the
// first message rather than only when a guard fires. Empty when there is no
// root AGENTS.md (a nested one is the guard's job to surface on access, not the
// system prompt's). The block is re-read on every system rebuild, so a rule
// edited mid-session is picked up on the next turn without a restart.
//
// It goes in the system prompt, not a conversation entry: the rules are about
// the WORKSPACE, not about what was said. A system block also survives
// compaction, which is the whole point — the rules are always in context, so a
// fold never silently drops them.
func rootAgentsMDBlock(workspace string) string {
	if workspace == "" {
		return ""
	}
	for _, name := range []string{"AGENTS.md", "agents.md"} {
		p := filepath.Join(workspace, name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		return "\n\n## Project rules (AGENTS.md)\n\n" +
			"These rules apply to this workspace. Nested directories may have their " +
			"own AGENTS.md; the code tool will ask you to read one before you touch " +
			"files under it.\n\n" + string(data)
	}
	return ""
}

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

// agentsHookEnv returns the POLY_LSP_HOOK env entry for the code server, or
// nil if the hook could not be written.
func agentsHookEnv() []string {
	path, err := writeAgentsHook()
	if err != nil {
		return nil
	}
	return []string{"POLY_LSP_HOOK=" + filepath.Clean(path)}
}
