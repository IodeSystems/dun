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
// The script is embedded in the binary so it works regardless of where dun
// was installed. The temp file is cleaned up when the server stops (or when
// the process exits — the OS reclaims it).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// findAgentsMD returns the workspace-relative paths of AGENTS.md / agents.md
// files under root, walking up to depth levels deep. Bounded on purpose: a
// monorepo with hundreds of subdirs should not make compaction scan the whole
// tree. Skips .git, node_modules, and hidden dirs.
func findAgentsMD(root string, depth int) []string {
	if root == "" {
		return nil
	}
	var out []string
	skip := map[string]bool{".git": true, "node_modules": true}
	var walk func(dir string, d int)
	walk = func(dir string, d int) {
		if d > depth {
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() {
				if skip[name] || (len(name) > 0 && name[0] == '.') {
					continue
				}
				walk(filepath.Join(dir, name), d+1)
				continue
			}
			if name == "AGENTS.md" || name == "agents.md" {
				rel, _ := filepath.Rel(root, filepath.Join(dir, name))
				out = append(out, rel)
			}
		}
	}
	walk(root, 0)
	return out
}

// agentsMDReminder returns a one-line note to append to a compaction note when
// the workspace has AGENTS.md files, so the model knows the rules may have been
// folded out of context. Cheap by design: it names the files, it does not
// re-supply their content — re-reading is one tool call and never blocked.
func agentsMDReminder(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return " · AGENTS.md rules may have been compacted (" +
		strings.Join(paths, ", ") + ") — re-read before continuing"
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
