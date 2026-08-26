#!/usr/bin/env bash
# agents_hook.sh — poly-lsp-mcp file-access hook: AGENTS.md guard.
#
# Invoked as:  agents_hook.sh <op> <absPath>
#   op        read | edit | create
#   absPath   the file about to be read/edited/created
# CWD is the file's directory (set by poly-lsp-mcp).
#
# Policy: if an AGENTS.md exists in the file's directory or any parent up to
# the workspace root (the nearest .git), and the file being accessed is NOT
# that AGENTS.md, cancel with a note telling the model to read it first.
#
# Exit codes:
#   0  proceed (no AGENTS.md found, or the file IS the AGENTS.md)
#   2  cancel — stdout is the note injected into the tool result

set -euo pipefail

op="$1"
target="$2"
target_base="$(basename "$target")"

# Never block reading/editing the AGENTS.md itself.
if [[ "$target_base" == "AGENTS.md" || "$target_base" == "agents.md" ]]; then
    exit 0
fi

# Find the workspace root: the nearest ancestor with a .git file or dir.
# .git can be a file (worktree pointer) or a directory (main repo).
root=""
dir="$(cd "$(dirname "$target")" 2>/dev/null && pwd || echo "")"
while [[ -n "$dir" && "$dir" != "/" ]]; do
    if [[ -e "$dir/.git" ]]; then
        root="$dir"
        break
    fi
    dir="$(dirname "$dir")"
done

# No workspace root found: nothing to guard.
if [[ -z "$root" ]]; then
    exit 0
fi

# Walk from the target's directory up to the workspace root looking for
# AGENTS.md (case-sensitive first, then lowercase).
found=""
dir="$(cd "$(dirname "$target")" 2>/dev/null && pwd || echo "")"
while [[ -n "$dir" && "$dir" != "$root" ]]; do
    for name in AGENTS.md agents.md; do
        if [[ -f "$dir/$name" ]]; then
            found="$dir/$name"
            break 2
        fi
    done
    parent="$(dirname "$dir")"
    if [[ "$parent" == "$dir" ]]; then
        break
    fi
    dir="$parent"
done
# Check the root itself too.
if [[ -z "$found" ]]; then
    for name in AGENTS.md agents.md; do
        if [[ -f "$root/$name" ]]; then
            found="$root/$name"
            break
        fi
    done
fi

# No AGENTS.md in the chain: proceed.
if [[ -z "$found" ]]; then
    exit 0
fi

# An AGENTS.md exists and we are not reading it: cancel with a pointer.
# The note tells the model what to do and where to read.
relpath="${found#$root/}"
echo "AGENTS.md guard: read $relpath before $op-ing $(basename "$target"). It contains project-specific rules, conventions, or constraints that apply to this path. Read it, then retry the operation."
exit 2
