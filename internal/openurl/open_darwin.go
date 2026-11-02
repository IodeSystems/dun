package openurl

import "os/exec"

// Open opens url in the default browser, without waiting for it.
func Open(url string) error {
	return startDetached(exec.Command("open", url))
}
