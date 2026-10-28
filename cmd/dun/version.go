package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// /version (USER 2026-10-28): which dun this is, the newest release, and the
// update policy — whether this build updates itself, and from where.

// versionLatestMsg is the newest release, looked up for /version.
type versionLatestMsg struct{ line string }

func versionSlash(m *tuiModel, _ []string) tea.Cmd {
	m.append(stDim.Render(strings.Join(versionLines(time.Now()), "\n")))
	m.refresh()
	if !isRelease() {
		return nil // only release builds carry a download location
	}
	return func() tea.Msg { return versionLatestMsg{line: latestLine()} }
}

// versionLines is what /version says at once: the build and the policy.
func versionLines(now time.Time) []string {
	plat := runtime.GOOS + "/" + runtime.GOARCH
	var out []string
	if isRelease() {
		out = append(out, fmt.Sprintf("dun %s · release build · %s", version, plat))
	} else {
		cur := "dev"
		if rev := buildRev(); rev != "" {
			cur += " " + rev
		}
		out = append(out, fmt.Sprintf("dun %s · source build (%s) · %s", cur, version, plat))
	}
	return append(out, "updates: "+updatePolicy(now))
}

// updatePolicy says whether this build updates itself, and from where.
func updatePolicy(now time.Time) string {
	if !isRelease() {
		switch {
		case srcDir == "":
			return "none — not a release build, and not built from a checkout (make install stamps one)"
		case os.Getenv("DUN_NO_AUTOBUILD") == "1":
			return "none — a source build from " + srcDir + "; rebuilding on start is off (DUN_NO_AUTOBUILD=1)"
		}
		return "a source build: rebuilds itself from " + srcDir + " when its source changed, at start; releases are not looked up"
	}
	if newUpdater() == nil {
		return "none — this build carries no release key (dun doctor says why)"
	}
	if loadConfig().NoUpdates {
		return "off (/upgrade on to check daily again)"
	}
	st := update.LoadState(update.StateFile(dunHome()))
	line := "daily, from " + release.ManifestURL + " (signed; verified with the key built into this build)"
	switch {
	case st.Checked.IsZero():
		line += " · not checked yet"
	default:
		line += " · last checked " + until(st.Checked, now)
	}
	if st.Error != "" {
		line += " · it ran into: " + plainLine(st.Error)
	}
	if st.Staged != "" && st.Staged != version {
		line += " · " + st.Staged + " is in place and takes over when dun next starts"
	}
	return line
}

// latestLine looks the newest release up now, verified, installing nothing.
func latestLine() string {
	keys, err := update.ParseKeys(release.PublicKey)
	if err != nil {
		return "latest: not looked up — this build carries no release key"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	man, err := update.Latest(ctx, &update.Fetcher{}, release.ManifestURL, keys, dunHome())
	if err != nil {
		return "latest: could not look it up — " + plainLine(err.Error())
	}
	cur, cerr := update.ParseVersion(version)
	lat, lerr := update.ParseVersion(man.Version)
	switch {
	case cerr == nil && lerr == nil && lat.Compare(cur) <= 0:
		return "latest: " + man.Version + " — this is it"
	}
	line := "latest: " + man.Version
	if t := man.PublishedAt(); !t.IsZero() {
		line += ", published " + until(t, time.Now())
	}
	if s := plainLine(man.Summary); s != "" {
		line += " — " + fit(s, 100)
	}
	return line
}
