package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/mod/semver"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/proto"
	"github.com/iodesystems/dun/release"
)

// Release builds: how dun was built decides how it updates (owner, item
// 47). A release build (make release: -tags release, -X main.channel=
// release) checks the signed manifest of the newest release once a day in
// the background, installs a newer dun over its own file, and asks dund to
// move to it at its next quiet moment (the coordinated upgrade). A build
// from a checkout rebuilds itself (selfupdate.go); a go install build is
// told the go install command; neither ever takes a build from releases.

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

// newUpdater is this release build's daily check; nil when it has none.
var newUpdater = func() *update.Updater {
	if !buildinfo.IsRelease() {
		return nil
	}
	keys, err := update.ParseKeys(release.PublicKey)
	if err != nil {
		return nil // the placeholder key: dun doctor says so
	}
	exe, err := buildinfo.SelfExe()
	if err != nil {
		return nil
	}
	return &update.Updater{ManifestURL: release.ManifestURL, Keys: keys, Current: buildinfo.Version, Exe: exe,
		Home: dunconfig.DunHome(), Platform: update.Platform(runtime.GOOS, runtime.GOARCH), Fetch: &update.Fetcher{}}
}

// releaseUpdate runs the daily check, in the background of the TUI, and
// returns the one line to show ("" for none). Unreachable networks and
// checks not due are silent; a refused download is said.
func releaseUpdate() string {
	u := newUpdater()
	if u == nil || os.Getenv("DUN_CHILD") != "" || dunconfig.LoadConfig().NoUpdates {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	res, err := u.Run(ctx)
	switch {
	case err != nil && update.IsVerify(err):
		return "update refused: " + textfmt.PlainLine(err.Error()) + " — nothing was installed · /upgrade off to stop checking"
	case err != nil || !res.Staged:
		return ""
	}
	return stagedLine(res, askDundUpgrade(u.Exe, res.To))
}

// stagedLine says a new build is in place: what changed, when it takes
// over, and how to opt out.
func stagedLine(res update.Result, asked bool) string {
	what := "was " + res.From
	if s := textfmt.PlainLine(res.Summary); s != "" {
		what += ": " + fit(s, 80)
	}
	when := "it takes over when dun next starts"
	if asked {
		when = "it takes over at the next quiet moment"
	}
	return "dun " + res.To + " is ready (" + what + "); " + when + " · /upgrade off to stop updates"
}

// askDundUpgrade asks a running dund that coordinates upgrades to move to
// exe (build ver) once nothing is busy. It never starts dund: one that is
// not running starts on the new file anyway. A variable for the tests.
var askDundUpgrade = func(exe, ver string) bool {
	c, err := net.DialTimeout("unix", dundcli.Socket(dunconfig.DunHome()), 2*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	ac := &attachConn{c: c, wait: 10 * time.Second}
	ev, err := ac.call(sc, "", &proto.Hello{Version: buildinfo.Version})
	if err != nil {
		return false
	}
	r, ok := ev.Payload.(*proto.HelloReply)
	if !ok || !r.Coordinated || !r.Upgradable {
		return false
	}
	_, err = ac.call(sc, "", &proto.Upgrade{Exe: exe, Version: ver})
	return err == nil
}

// upgradeSwitch is /upgrade off|on: release updates for every dun of this
// user (the global config).
func upgradeSwitch(on bool) (string, error) {
	c := dunconfig.LoadConfig()
	c.NoUpdates = !on
	if err := dunconfig.SaveConfig(c); err != nil {
		return "", err
	}
	line := "updates off — dun no longer checks for new releases · /upgrade on to check again"
	if on {
		line = "updates on — dun checks for a new release once a day · /upgrade off to stop"
	}
	if !buildinfo.IsRelease() {
		line += " (this dun is " + buildinfo.SelfOrigin().String() + ": it does not update from releases)"
	}
	return line, nil
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
