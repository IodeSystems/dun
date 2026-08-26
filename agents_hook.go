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
[[ "$(basename "$target")" == "AGENTS.md" || "$(basename "$target")" == "agents.md" ]] && exit 0
root=""
dir="$(cd "$(dirname "$target")" 2>/dev/null && pwd || echo "")"
while [[ -n "$dir" && "$dir" != "/" ]]; do
  [[ -e "$dir/.git" ]] && { root="$dir"; break; }
  dir="$(dirname "$dir")"
done
[[ -z "$root" ]] && exit 0
found=""
dir="$(cd "$(dirname "$target")" 2>/dev/null && pwd || echo "")"
while [[ -n "$dir" && "$dir" != "$root" ]]; do
  for n in AGENTS.md agents.md; do
    [[ -f "$dir/$n" ]] && { found="$dir/$n"; break 2; }
  done
  parent="$(dirname "$dir")"
  [[ "$parent" == "$dir" ]] && break
  dir="$parent"
done
if [[ -z "$found" ]]; then
  for n in AGENTS.md agents.md; do
    [[ -f "$root/$n" ]] && { found="$root/$n"; break; }
  done
fi
[[ -z "$found" ]] && exit 0
echo "AGENTS.md guard: read ${found#"$root"/} before $op-ing $(basename "$target"). It contains project-specific rules, conventions, or constraints that apply to this path. Read it, then retry the operation."
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

// agentsHookEnv returns the POLY_LSP_HOOK env entry for the code server, or
// nil if the hook could not be written.
func agentsHookEnv() []string {
	path, err := writeAgentsHook()
	if err != nil {
		return nil
	}
	return []string{"POLY_LSP_HOOK=" + filepath.Clean(path)}
}
