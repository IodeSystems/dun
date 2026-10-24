//go:build release

package release

import _ "embed"

// Built is true in a release build (`go build -tags release`).
const Built = true

// ManifestURL is where a release build looks for the newest release's
// manifest; its signature is beside it, ManifestURL + ".minisig". Only
// release builds carry a download location at all (owner, item 47).
const ManifestURL = "https://github.com/IodeSystems/dun/releases/latest/download/dun-manifest.json"

// PublicKey is the minisign public key file (one key, or several during a
// rotation) that a release manifest must be signed with. The committed file
// is a placeholder that parses as no key: a build carrying it never updates.
//
//go:embed minisign.pub
var PublicKey string

// Deps is the manifest of the tools this release was tested with
// (mcpshell, poly-lsp-mcp), pinned when the build was made: `dun doctor
// --fix` installs them from it, checked against these checksums. dun's own
// entry cannot be in it (a binary cannot carry its own checksum); updates
// use the remote manifest.
//
//go:embed deps.json
var Deps []byte
