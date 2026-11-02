package openurl

import "os/exec"

// Open opens url in the desktop's browser, without waiting for it.
// It fails on a box with no desktop, which is why a login URL is always
// shown to the person as well (see mcpoauth.go).
func Open(url string) error {
	return startDetached(exec.Command("xdg-open", url))
}
