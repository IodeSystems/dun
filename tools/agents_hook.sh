#!/usr/bin/env bash
# tools/agents_hook.sh — HUMAN-MAINTAINED COPY of the AGENTS.md guard.
#
# This file is the reference for the script that agents_hook.go embeds in the
# binary and arms on the code server at startup. The two MUST stay in sync:
# agentsHookScript (the Go const) is what actually runs. If you change one,
# change the other. To regenerate this file from the Go const:
#   python3 -c "import re; s=open('agents_hook.go').read(); print(s[s.index('const agentsHookScript = `')+27:s.rindex('`', s.index('const agentsHookScript = `')+27]))" > /tmp/x
# (or just edit both together.)
#
# Invoked by poly-lsp-mcp as:  agents_hook.sh <op> <absPath>
#   op        read | edit | create
#   absPath   the file about to be read/edited/created
# CWD is the file's directory (set by poly-lsp-mcp).
#
# Policy: cancel (exit 2, stdout = note) when an AGENTS.md sits in the target's
# directory chain (up to the nearest .git) AND has not already been read this
# session. Reading the AGENTS.md marks it read (state file at $root/.dun/
# agents_read, keyed by path+mtime). A changed AGENTS.md re-engages the guard.

#!/usr/bin/env bash
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
