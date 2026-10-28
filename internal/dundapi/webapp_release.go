//go:build release

package dundapi

import (
	"embed"
	"io/fs"
)

// The web app, embedded in a release build: make release runs make webapp
// first, which exports mobile/ for the web into webdist (USER 2026-10-28:
// "release should have the embed expo app"). A source build has none
// (webapp_dev.go).
//
//go:embed all:webdist
var webdist embed.FS

func embeddedApp() (fs.FS, bool) {
	app, err := fs.Sub(webdist, "webdist")
	return app, err == nil
}
