//go:build !linux && !darwin

package dun

import (
	"fmt"
	"runtime"
)

// OpenBrowser refuses rather than guesses on an OS dun does not know how to
// open a browser on; the login URL is shown to the person either way.
func OpenBrowser(url string) error {
	return fmt.Errorf("dun: no browser opener on %s — open the URL by hand", runtime.GOOS)
}
