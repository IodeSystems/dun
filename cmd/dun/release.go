package main

import (
	"context"
	"fmt"
	"os"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// Release builds: how dun was built decides how it updates (owner, item
// 47). A release build (make release: -tags release, -X main.channel=
// release) checks the signed manifest of the newest release once a day in
// the background, installs a newer dun over its own file, and asks dund to
// move to it at its next quiet moment (the coordinated upgrade). A build
// from a checkout rebuilds itself (selfupdate.go); a go install build is
// told the go install command; neither takes a build from releases except
// on an explicit /upgrade --channel=release (Carl 2026-11-10,
// internal/tui/channel.go).

// channel is "release" in a release build, stamped by make release.
var channel = ""

// pinnedTool is name's download for this platform: the tools manifest this
// release build was made with, or the registry's newer pin
// (mcpserver.PinnedDownload); false on any other build. A variable for the
// tests.
var pinnedTool = mcpserver.PinnedDownload

// installPinned downloads name's pinned release into its managed directory,
// $DUN_HOME/tools/<name>/<version>/ (mcpserver.FetchPinned: the same code a
// session's first need runs). A variable for the tests.
var installPinned = func(name string) (string, error) {
	return mcpserver.FetchPinned(context.Background(), name, func(s string) { fmt.Fprintln(os.Stderr, s) })
}

// releaseBuild is doctor's line on a release build's updates.
func (d *doctor) releaseBuild() {
	if !release.Built {
		d.add("warn", "build", "stamped a release, but built without -tags release: it cannot update", "make release builds both")
		return
	}
	if _, err := update.ParseKeys(release.PublicKey); err != nil {
		d.add("warn", "build", "a release build with no signing key (release/minisign.pub is the placeholder): it cannot update",
			"install a published release")
		return
	}
	if dunconfig.LoadConfig().NoUpdates {
		d.add("ok", "build", "release updates off", "/upgrade on in the TUI to check daily")
		return
	}
	detail := "checks for a new release daily"
	st := update.LoadState(update.StateFile(dunconfig.DunHome()))
	if !st.Checked.IsZero() {
		detail += ", last " + textfmt.RelTime(st.Checked)
	}
	if st.Error != "" {
		d.add("warn", "build", detail+": "+textfmt.OneLine(st.Error), "it tries again a day after the last check")
		return
	}
	d.add("ok", "build", detail, "")
}
