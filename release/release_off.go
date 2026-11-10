//go:build !release

package release

// A source, go-install or plain build: no key for the daily update and no
// pinned tools (owner: only release builds update and install from
// releases). ManifestURL is in every build (key.go), read by a source build
// only on an explicit /upgrade --channel=release. See release_on.go.

const Built = false

var (
	PublicKey string
	Deps      []byte
)
