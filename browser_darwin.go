package dun

import "os/exec"

// OpenBrowser opens url in the default browser, without waiting for it.
func OpenBrowser(url string) error {
	return startDetached(exec.Command("open", url))
}
