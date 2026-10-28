#!/bin/sh
# Install dun: curl -fsSL https://dun.iodesystems.com/install.sh | sh
#
# Downloads the latest release for this OS and CPU from GitHub, checks its
# sha256 against the release manifest, and puts dun in ~/.local/bin
# (DUN_INSTALL_DIR overrides). Runs nothing else.
set -eu

dir=${DUN_INSTALL_DIR:-$HOME/.local/bin}
base=https://github.com/IodeSystems/dun/releases/latest/download

fail() { echo "install.sh: $*" >&2; exit 1; }

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "no release for $(uname -s); build from source: https://github.com/IodeSystems/dun" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "no release for $(uname -m); build from source: https://github.com/IodeSystems/dun" ;;
esac
plat=${os}_${arch}

if command -v curl >/dev/null 2>&1; then
  get() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  get() { wget -qO "$2" "$1"; }
else
  fail "needs curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sum() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sum() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "needs sha256sum or shasum"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

get "$base/dun-manifest.json" "$tmp/manifest.json" || fail "could not download the release manifest"
# The manifest is written by tools/mkmanifest: one platform per block, its
# sha256 a few lines below the platform's name.
want=$(grep -A3 "\"$plat\": {" "$tmp/manifest.json" | sed -n 's/.*"sha256": *"\([0-9a-f]*\)".*/\1/p' | head -1)
version=$(sed -n 's/^  "version": *"\([^"]*\)".*/\1/p' "$tmp/manifest.json" | head -1)
[ -n "$want" ] || fail "the manifest lists no $plat build"

echo "dun $version for $plat"
get "$base/dun_$plat.tar.gz" "$tmp/dun.tar.gz" || fail "could not download dun_$plat.tar.gz"
got=$(sum "$tmp/dun.tar.gz")
[ "$got" = "$want" ] || fail "sha256 mismatch for dun_$plat.tar.gz: got $got, the manifest says $want"

tar -xzf "$tmp/dun.tar.gz" -C "$tmp" dun
mkdir -p "$dir"
install -m 755 "$tmp/dun" "$dir/dun"
echo "installed $dir/dun"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "note: $dir is not on PATH; add it, e.g. export PATH=\"$dir:\$PATH\"" ;;
esac
cat <<EOF

next:
  dun doctor --fix    # fetches mcpshell; poly-lsp-mcp and raglit when you want them
  dun --setup         # log in to a model: an address, and a key if it needs one
  cd your-project && dun
EOF
