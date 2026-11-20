#!/usr/bin/env bash
# Install the tools release/deps.json pins (mcpshell, poly-lsp-mcp, raglit) for
# THIS os/arch, so the tests run against the versions a release ships.
# The one place the jq / sha256 logic lives: `make test-pinned`, ci.yml and
# release.yml all call it.
#
#   tools/pinned-tools.sh [--github] [CACHE_DIR]
#
# CACHE_DIR defaults to ${XDG_CACHE_HOME:-~/.cache}/dun/pinned. Each tool goes
# to CACHE_DIR/<tool>-<version>/ (tarball + binary). A tarball already there
# whose sha256 matches deps.json is not downloaded again; a download whose
# sha256 differs fails. CACHE_DIR/bin/ holds a symlink per tool.
#
# Output (stdout): shell lines to eval, which export DUN_MCPSHELL,
# DUN_POLY_LSP_MCP, DUN_RAGLIT (dun does not look on PATH for its own tools)
# and PATH. With --github: append them to $GITHUB_ENV and $GITHUB_PATH instead.
# Needs jq, curl, sha256sum (or shasum -a 256), tar.
set -euo pipefail

github=0
if [ "${1:-}" = "--github" ]; then github=1; shift; fi
cache=${1:-${XDG_CACHE_HOME:-$HOME/.cache}/dun/pinned}
root=$(cd "$(dirname "$0")/.." && pwd)
deps=$root/release/deps.json
tools="mcpshell poly-lsp-mcp raglit"

for c in jq curl tar; do
	command -v "$c" >/dev/null || { echo "pinned-tools: $c is required and not on PATH" >&2; exit 1; }
done
if command -v sha256sum >/dev/null; then sha() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null; then sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
else echo "pinned-tools: sha256sum (or shasum) is required and not on PATH" >&2; exit 1; fi

# deps.json names the platforms GOOS_GOARCH; uname is the fallback without go.
if command -v go >/dev/null; then plat=$(go env GOOS)_$(go env GOARCH)
else
	os=$(uname -s | tr A-Z a-z); arch=$(uname -m)
	case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
	plat=${os}_$arch
fi

mkdir -p "$cache/bin"
envlines=""
for t in $tools; do
	ver=$(jq -r --arg t "$t" '.artifacts[$t].version // empty' "$deps")
	url=$(jq -r --arg t "$t" --arg p "$plat" '.artifacts[$t].files[$p].url // empty' "$deps")
	want=$(jq -r --arg t "$t" --arg p "$plat" '.artifacts[$t].files[$p].sha256 // empty' "$deps")
	if [ -z "$ver" ] || [ -z "$url" ] || [ -z "$want" ]; then
		echo "pinned-tools: $deps has no $t for $plat" >&2; exit 1
	fi
	dir=$cache/$t-$ver
	tgz=$dir/$t.tar.gz
	mkdir -p "$dir"
	if [ -f "$tgz" ] && [ "$(sha "$tgz")" = "$want" ]; then
		echo "pinned-tools: $t $ver cached" >&2
	else
		echo "pinned-tools: $t $ver downloading $url" >&2
		rm -f "$dir/$t" # a binary from a tarball that is not this one
		curl -fsSL -o "$tgz.part" "$url"
		got=$(sha "$tgz.part")
		if [ "$got" != "$want" ]; then
			rm -f "$tgz.part"
			echo "pinned-tools: $t sha256 mismatch: deps.json $want, download $got" >&2; exit 1
		fi
		mv "$tgz.part" "$tgz"
	fi
	if [ ! -x "$dir/$t" ]; then
		tar -xzf "$tgz" -C "$dir" "$t" 2>/dev/null || tar -xzf "$tgz" -C "$dir"
		chmod 0755 "$dir/$t"
	fi
	ln -sf "$dir/$t" "$cache/bin/$t"
	name=DUN_$(echo "$t" | tr a-z- A-Z_)
	if [ "$github" = 1 ]; then echo "$name=$cache/bin/$t" >> "$GITHUB_ENV"
	else envlines="$envlines$(printf 'export %s=%q' "$name" "$cache/bin/$t")"$'\n'; fi
done
if [ "$github" = 1 ]; then echo "$cache/bin" >> "$GITHUB_PATH"
else printf '%s' "$envlines"; printf 'export PATH=%q:$PATH\n' "$cache/bin"; fi
