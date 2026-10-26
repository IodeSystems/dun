package release

import _ "embed"

// RegistryKey is the release key (minisign.pub) in every build: the
// registry (internal/registry, docs/registry.md) is signed with it, and a
// dev build reads the registry too. A public key and a config address are
// not a download location; the tool files a registry entry names are
// downloaded by release builds only, as deps.json's are.
//
//go:embed minisign.pub
var RegistryKey string

// RegistryURL is the registry's index; its signature is beside it,
// RegistryURL + ".minisig".
const RegistryURL = "https://dun.iodesystems.com/registry/v1/index.json"
