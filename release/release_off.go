//go:build !release

package release

// A source, go-install or plain build: no download location, no key, no
// pinned tools (owner: only release builds have the download paths). See
// release_on.go.

const (
	Built       = false
	ManifestURL = ""
)

var (
	PublicKey string
	Deps      []byte
)
