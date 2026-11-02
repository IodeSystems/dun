//go:build !release

package tui

import "testing"

// A build without -tags release has no updater.
func TestDevBuildHasNoUpdater(t *testing.T) {
	if newUpdater() != nil {
		t.Fatal("a dev build would check for updates")
	}
}
