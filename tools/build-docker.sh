#!/bin/bash
# Build the dun Docker image with all required binaries.
# Usage: tools/build-docker.sh [image-tag]
#
# Finds dun, poly-lsp-mcp, mcpshell, and raglit on PATH (or ~/go/bin),
# copies them into a build context, and runs docker build.

set -euo pipefail

TAG="${1:-dun:local}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

# Find binaries
for bin in dun poly-lsp-mcp mcpshell raglit; do
    src="$(command -v "$bin" 2>/dev/null || true)"
    if [ -z "$src" ]; then
        echo "error: $bin not found on PATH" >&2
        exit 1
    fi
    cp "$src" "$TMPDIR/$bin"
    echo "  $bin: $src"
done

cp "$ROOT/Dockerfile" "$TMPDIR/Dockerfile"

echo "Building $TAG ..."
docker build -t "$TAG" "$TMPDIR"
echo "Done: $TAG ($(docker images -q "$TAG" | head -1))"
