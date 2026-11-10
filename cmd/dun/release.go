package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/mod/semver"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/dunconfig"
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

// pinnedTool is name's download for this platform in the tools manifest
// this release build was made with; false on any other build. A variable
// for the tests.
var pinnedTool = func(name string) (update.Artifact, update.File, bool) {
	if !buildinfo.IsRelease() {
		return update.Artifact{}, update.File{}, false
	}
	plat := update.Platform(runtime.GOOS, runtime.GOARCH)
	m, err := update.ParseManifest(release.Deps)
	if err != nil {
		return update.Artifact{}, update.File{}, false
	}
	a, f, ok := m.File(name, plat)
	// A newer pin in the registry (signed by the same key) wins, when it has
	// this platform's file: a tool's point release without a dun release.
	if t, rok := dun.RegistryTool(name); rok {
		if rf, fok := t.Files[plat]; fok && (!ok || semver.Compare(t.Recommended, a.Version) > 0) {
			return update.Artifact{Version: t.Recommended, Files: t.Files}, rf, true
		}
	}
	return a, f, ok
}

// installPinned installs name from the pinned manifest beside dun, where
// the engine looks after PATH (dun.FindCommand). A variable for the tests.
var installPinned = func(name string) (string, error) {
	a, f, ok := pinnedTool(name)
	if !ok {
		return "", errNotPinned
	}
	exe, err := buildinfo.SelfExe()
	if err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "installing %s %s from its release (the checksum is pinned in this build): %s\n", name, a.Version, f.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return (&update.Fetcher{}).Install(ctx, f, name, filepath.Join(filepath.Dir(exe), name))
}

var errNotPinned = errors.New("not in this release's tools manifest")

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
