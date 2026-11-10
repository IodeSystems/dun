package release

import _ "embed"

// RegistryKey is the release key (minisign.pub) in every build: the
// registry (internal/registry, docs/registry.md) is signed with it, and a
// dev build reads the registry too. A public key and a config address are
// not a download location; the tool files a registry entry names are
// downloaded by release builds only, as deps.json's are. A source build
// also verifies the release manifest with it, on an explicit /upgrade
// --channel=release only (ManifestURL).
//
//go:embed minisign.pub
var RegistryKey string

// RegistryURL is the registry's index; its signature is beside it,
// RegistryURL + ".minisig".
const RegistryURL = "https://dun.iodesystems.com/registry/v1/index.json"

// ManifestURL is where the newest release's manifest is; its signature is
// beside it, ManifestURL + ".minisig". A release build checks it daily. It
// is in every build (Carl 2026-11-10, reversing item 47's "source builds
// have no download paths", docs/decisions.md), but a source build reads it
// only on an explicit /upgrade --channel=release; it never updates from
// releases otherwise.
const ManifestURL = "https://github.com/IodeSystems/dun/releases/latest/download/dun-manifest.json"
