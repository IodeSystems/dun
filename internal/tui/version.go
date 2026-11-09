package tui

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/depver"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// /version (USER 2026-10-28): which dun this is, the newest release, and the
// update policy — whether this build updates itself, and from where. Then
// the libraries compiled in and the tools dun runs, each against the
// version this dun pins (USER 2026-11-09).

// versionLatestMsg is the newest release, looked up for /version.
type versionLatestMsg struct{ line string }

func versionSlash(m *tuiModel, _ []string) tea.Cmd {
	m.append(StDim.Render(strings.Join(versionLines(time.Now()), "\n")))
	m.refresh()
	// The tools are `dun tools --json`'s rows (cmd/dun/toolscmd.go), as
	// /upgrade reads them: their build info, never running them.
	tools := func() tea.Msg { return versionLatestMsg{line: versionToolLines(runTools(false))} }
	if !buildinfo.IsRelease() {
		return tools // only release builds carry a download location
	}
	return tea.Batch(tools, func() tea.Msg { return versionLatestMsg{line: latestLine()} })
}

// versionToolLines is /version's tools block, a line per tool.
func versionToolLines(rows []toolState, err error) string {
	if err != nil {
		return "tools: not checked — " + textfmt.PlainLine(err.Error())
	}
	lines := []string{"tools:"}
	for _, r := range rows {
		lines = append(lines, "  "+r.dep().Line())
	}
	return strings.Join(lines, "\n")
}

// versionLines is what /version says at once: the build and the policy.
func versionLines(now time.Time) []string {
	plat := runtime.GOOS + "/" + runtime.GOARCH
	var out []string
	if buildinfo.IsRelease() {
		out = append(out, fmt.Sprintf("dun %s · release build · %s", buildinfo.Version, plat))
	} else {
		cur := "dev"
		if rev := buildRev(); rev != "" {
			cur += " " + rev
		}
		out = append(out, fmt.Sprintf("dun %s · source build (%s) · %s", cur, buildinfo.Version, plat))
	}
	out = append(out, "updates: "+updatePolicy(now), "modules:")
	for _, l := range depver.ModuleLines() {
		out = append(out, "  "+l)
	}
	return out
}

// updatePolicy says whether this build updates itself, and from where.
func updatePolicy(now time.Time) string {
	if !buildinfo.IsRelease() {
		switch {
		case buildinfo.SrcDir == "":
			return "none — not a release build, and not built from a checkout (make install stamps one)"
		case os.Getenv("DUN_NO_AUTOBUILD") == "1":
			return "none — a source build from " + buildinfo.SrcDir + "; rebuilding on start is off (DUN_NO_AUTOBUILD=1)"
		}
		return "a source build: rebuilds itself from " + buildinfo.SrcDir + " when its source changed, at start; releases are not looked up"
	}
	if newUpdater() == nil {
		return "none — this build carries no release key (dun doctor says why)"
	}
	if dunconfig.LoadConfig().NoUpdates {
		return "off (/upgrade on to check daily again)"
	}
	st := update.LoadState(update.StateFile(dunconfig.DunHome()))
	line := "daily, from " + release.ManifestURL + " (signed; verified with the key built into this build)"
	switch {
	case st.Checked.IsZero():
		line += " · not checked yet"
	default:
		line += " · last checked " + until(st.Checked, now)
	}
	if st.Error != "" {
		line += " · it ran into: " + textfmt.PlainLine(st.Error)
	}
	if st.Staged != "" && st.Staged != buildinfo.Version {
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
	man, err := update.Latest(ctx, &update.Fetcher{}, release.ManifestURL, keys, dunconfig.DunHome())
	if err != nil {
		return "latest: could not look it up — " + textfmt.PlainLine(err.Error())
	}
	cur, cerr := update.ParseVersion(buildinfo.Version)
	lat, lerr := update.ParseVersion(man.Version)
	switch {
	case cerr == nil && lerr == nil && lat.Compare(cur) <= 0:
		return "latest: " + man.Version + " — this is it"
	}
	line := "latest: " + man.Version
	if t := man.PublishedAt(); !t.IsZero() {
		line += ", published " + until(t, time.Now())
	}
	if s := textfmt.PlainLine(man.Summary); s != "" {
		line += " — " + fit(s, 100)
	}
	return line
}
