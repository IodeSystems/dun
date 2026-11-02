//go:build !linux && !darwin

package openurl

import (
	"fmt"
	"runtime"
)

// Open refuses rather than guesses on an OS dun does not know how to
// open a browser on; the login URL is shown to the person either way.
func Open(url string) error {
	return fmt.Errorf("dun: no browser opener on %s — open the URL by hand", runtime.GOOS)
}
