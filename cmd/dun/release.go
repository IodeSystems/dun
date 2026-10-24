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
	"runtime/debug"
	"strings"
	"time"
	"unicode"

	"github.com/iodesystems/dun/internal/dundcli"
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

// isRelease: stamped release AND built with the tag that carries the
// manifest's location and the key.
func isRelease() bool { return channel == "release" && release.Built }

// selfOrigin is how this binary was built.
func selfOrigin() update.Origin {
	o := update.Origin{Kind: update.GoBuild}
	if bi, ok := debug.ReadBuildInfo(); ok {
		o = update.FromBuildInfo(bi)
	}
	switch {
	case channel == "release":
		o.Kind, o.Version = update.Release, version
	case srcDir != "":
		o.Kind = update.Checkout
		if o.Revision == "" {
			o.Revision = version
		}
	}
	return o
}

// pinnedTool is name's download for this platform in the tools manifest
// this release build was made with; false on any other build. A variable
// for the tests.
var pinnedTool = func(name string) (update.Artifact, update.File, bool) {
	if !isRelease() {
		return update.Artifact{}, update.File{}, false
	}
	m, err := update.ParseManifest(release.Deps)
	if err != nil {
		return update.Artifact{}, update.File{}, false
	}
	return m.File(name, update.Platform(runtime.GOOS, runtime.GOARCH))
}

// installPinned installs name from the pinned manifest beside dun, where
// the engine looks after PATH (dun.FindCommand). A variable for the tests.
var installPinned = func(name string) (string, error) {
	a, f, ok := pinnedTool(name)
	if !ok {
		return "", errNotPinned
	}
	exe, err := selfExe()
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
	if !isRelease() {
		return nil
	}
	keys, err := update.ParseKeys(release.PublicKey)
	if err != nil {
		return nil // the placeholder key: dun doctor says so
	}
	exe, err := selfExe()
	if err != nil {
		return nil
	}
	return &update.Updater{ManifestURL: release.ManifestURL, Keys: keys, Current: version, Exe: exe,
		Home: dunHome(), Platform: update.Platform(runtime.GOOS, runtime.GOARCH), Fetch: &update.Fetcher{}}
}

// releaseUpdate runs the daily check, in the background of the TUI, and
// returns the one line to show ("" for none). Unreachable networks and
// checks not due are silent; a refused download is said.
func releaseUpdate() string {
	u := newUpdater()
	if u == nil || os.Getenv("DUN_CHILD") != "" || loadConfig().NoUpdates {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	res, err := u.Run(ctx)
	switch {
	case err != nil && update.IsVerify(err):
		return "update refused: " + plainLine(err.Error()) + " — nothing was installed · /upgrade off to stop checking"
	case err != nil || !res.Staged:
		return ""
	}
	return stagedLine(res, askDundUpgrade(u.Exe, res.To))
}

// stagedLine says a new build is in place: what changed, when it takes
// over, and how to opt out.
func stagedLine(res update.Result, asked bool) string {
	what := "was " + res.From
	if s := plainLine(res.Summary); s != "" {
		what += ": " + fit(s, 80)
	}
	when := "it takes over when dun next starts"
	if asked {
		when = "it takes over at the next quiet moment"
	}
	return "dun " + res.To + " is ready (" + what + "); " + when + " · /upgrade off to stop updates"
}

// plainLine is s on one line, without control characters: a manifest's
// words reach the screen.
func plainLine(s string) string {
	return oneLine(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

// askDundUpgrade asks a running dund that coordinates upgrades to move to
// exe (build ver) once nothing is busy. It never starts dund: one that is
// not running starts on the new file anyway. A variable for the tests.
var askDundUpgrade = func(exe, ver string) bool {
	c, err := net.DialTimeout("unix", dundcli.Socket(dunHome()), 2*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	ac := &attachConn{c: c, wait: 10 * time.Second}
	ev, err := ac.call(sc, "", &proto.Hello{Version: version})
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
	c := loadConfig()
	c.NoUpdates = !on
	if err := saveConfig(c); err != nil {
		return "", err
	}
	line := "updates off — dun no longer checks for new releases · /upgrade on to check again"
	if on {
		line = "updates on — dun checks for a new release once a day · /upgrade off to stop"
	}
	if !isRelease() {
		line += " (this dun is " + selfOrigin().String() + ": it does not update from releases)"
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
	if loadConfig().NoUpdates {
		d.add("ok", "build", "release updates off", "/upgrade on in the TUI to check daily")
		return
	}
	detail := "checks for a new release daily"
	st := update.LoadState(update.StateFile(dunHome()))
	if !st.Checked.IsZero() {
		detail += ", last " + relTime(st.Checked)
	}
	if st.Error != "" {
		d.add("warn", "build", detail+": "+oneLine(st.Error), "it tries again a day after the last check")
		return
	}
	d.add("ok", "build", detail, "")
}
