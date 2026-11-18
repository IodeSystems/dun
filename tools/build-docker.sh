#!/bin/bash
# Build the dun Docker image with all required binaries.
# Usage: tools/build-docker.sh [image-tag]
#
# Copies dun (from PATH or ~/go/bin) and dun's own copies of mcpshell,
# poly-lsp-mcp and raglit into a build context, and runs docker build.
# Each tool resolves as dun resolves it (internal/mcpserver/tools.go):
#   1. $DUN_MCPSHELL, $DUN_POLY_LSP_MCP, $DUN_RAGLIT
#   2. the pinned copy for the image's linux arch:
#      $DUN_HOME/tools/<tool>/<pin>/linux-<arch>/<tool>
#      (<pin> is the version `dun tools --json` reports)
# The image arch is the docker daemon's. Install the copies with
# `dun doctor --fix` (another arch's copy: a DUN_HOME on that machine).

set -euo pipefail

TAG="${1:-dun:local}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

DUN="$(command -v dun 2>/dev/null || true)"
if [ -z "$DUN" ] && [ -x "$HOME/go/bin/dun" ]; then
    DUN="$HOME/go/bin/dun"
fi
if [ -z "$DUN" ]; then
    echo "error: dun not found on PATH or in ~/go/bin" >&2
    exit 1
fi
cp "$DUN" "$TMPDIR/dun"
echo "  dun: $DUN"

# The image arch, as a GOARCH.
case "$(docker info --format '{{.Architecture}}' 2>/dev/null || uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *)
        echo "error: unsupported image arch: $(docker info --format '{{.Architecture}}' 2>/dev/null || uname -m)" >&2
        exit 1
        ;;
esac

TOOLS_JSON="$("$DUN" tools --json 2>/dev/null || true)"
HOME_DIR="${DUN_HOME:-$HOME/.dun}"

for bin in poly-lsp-mcp mcpshell raglit; do
    var="DUN_$(echo "$bin" | tr 'a-z-' 'A-Z_')"
    src="${!var:-}"
    if [ -n "$src" ]; then
        if [ ! -x "$src" ]; then
            echo "error: \$$var is $src, which is not an executable file" >&2
            exit 1
        fi
    else
        pin="$(printf '%s' "$TOOLS_JSON" | jq -r --arg n "$bin" '.[] | select(.name == $n) | .pin // empty' 2>/dev/null || true)"
        src="$HOME_DIR/tools/$bin/$pin/linux-$ARCH/$bin"
        if [ -z "$pin" ] || [ ! -x "$src" ]; then
            echo "error: $bin ${pin:-(pin unknown)} for linux-$ARCH not found at $src" >&2
            echo "       run: dun doctor --fix (or set \$$var)" >&2
            exit 1
        fi
    fi
    cp "$src" "$TMPDIR/$bin"
    echo "  $bin: $src"
done

cp "$ROOT/Dockerfile" "$TMPDIR/Dockerfile"

echo "Building $TAG ..."
docker build -t "$TAG" "$TMPDIR"
echo "Done: $TAG ($(docker images -q "$TAG" | head -1))"
