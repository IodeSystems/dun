package tui

import (
	"bufio"
	"context"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/proto"
	"github.com/iodesystems/dun/release"
)

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
		line += " (this dun is " + buildinfo.SelfOrigin().String() + ": it does not update from releases; /upgrade --channel=release switches it to them)"
	}
	return line, nil
}
