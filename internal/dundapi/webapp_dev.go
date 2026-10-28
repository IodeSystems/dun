//go:build !release

package dundapi

import "io/fs"

// embeddedApp is the web app a release build carries (webapp_release.go); a
// source build has none, and serves --web-root or a page saying so.
func embeddedApp() (fs.FS, bool) { return nil, false }
