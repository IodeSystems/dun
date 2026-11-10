package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// /upgrade --channel=release|source moves this dun to another channel
// (Carl 2026-11-10, docs/decisions.md). The channel is not a setting: it
// is how the installed binary was built, so after a switch plain /upgrade
// follows whatever is installed.
//
//   - source → release: the newest signed release goes over this binary's
//     file (the launcher's binary: to install.sh's ~/.local/bin/dun), even
//     when it is older than this build; both versions are said. A dun that
//     comes first on PATH is named, never removed.
//   - release → source: nothing is downloaded or built; it says to run
//     make install in the checkout.
//   - the channel this dun is already on: plain /upgrade.

// channelArg is the value of --channel=X or --channel X in args; ok false
// when there is no --channel.
func channelArg(args []string) (string, bool) {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--channel="); ok {
			return v, true
		}
		if a == "--channel" {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

// Variables for the tests.
var (
	channelIsRelease = buildinfo.IsRelease
	channelExe       = buildinfo.SelfExe
	// newSwitcher is the release check that installs at target. A source
	// build has no release.PublicKey; the manifest is signed with the same
	// key as the registry (release/key.go).
	newSwitcher = func(target string) (*update.Updater, error) {
		keys, err := update.ParseKeys(release.RegistryKey)
		if err != nil {
			return nil, errors.New("this build carries no release key")
		}
		return &update.Updater{ManifestURL: release.ManifestURL, Keys: keys, Current: buildinfo.Version, Exe: target,
			Home: dunconfig.DunHome(), Platform: update.Platform(runtime.GOOS, runtime.GOARCH), Fetch: &update.Fetcher{}}, nil
	}
)

// channelMsg is a switch's outcome.
type channelMsg struct {
	lines []string
	err   error
}

// channelSlash is /upgrade --channel=ch.
func channelSlash(m *tuiModel, ch string) tea.Cmd {
	switch ch {
	case "release":
		if channelIsRelease() {
			m.append(StDim.Render("already on the release channel (" + buildinfo.Version + ")"))
			return upgradeSlash(m, nil)
		}
		m.append(StDim.Render("channel: installing the newest release over this source build (" + buildinfo.Version + ")…"))
		return func() tea.Msg {
			lines, err := switchToRelease()
			return channelMsg{lines: lines, err: err}
		}
	case "source":
		if channelIsRelease() {
			m.append(stNote.Render("this dun is release " + buildinfo.Version + ": to switch to source, run `make install` in your dun checkout (nothing was downloaded or built)"))
			return nil
		}
		m.append(StDim.Render("already on the source channel (" + buildinfo.SelfOrigin().String() + ", " + buildinfo.Version + ")"))
		return upgradeSlash(m, nil)
	}
	m.append(StErr.Render("unknown channel " + quoteArg(ch) + ": --channel=release or --channel=source"))
	return nil
}

func quoteArg(s string) string { return `"` + textfmt.PlainLine(s) + `"` }

// channelDone shows a switch's outcome.
func (m *tuiModel) channelDone(msg channelMsg) {
	if msg.err != nil {
		m.append(StErr.Render("channel: not switched: " + textfmt.PlainLine(msg.err.Error()) + " — nothing was installed"))
		return
	}
	m.append(stNote.Render(strings.Join(msg.lines, "\n")))
}

// switchToRelease installs the newest release where SwitchTarget says and
// returns what to show.
func switchToRelease() ([]string, error) {
	exe, err := channelExe()
	if err != nil {
		return nil, err
	}
	target, launcher := update.SwitchTarget(exe)
	u, err := newSwitcher(target)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	res, err := u.Switch(ctx)
	if err != nil {
		return nil, err
	}
	lines := []string{"dun release " + res.To + " is installed at " + target + " (was the source build " + res.From + ")"}
	if launcher {
		lines = append(lines, "this dun ran from the launcher's build "+exe+", which the launcher rebuilds from the checkout: it was left as it is")
	}
	switch first, onPath := update.Shadow(target, os.Getenv("PATH")); {
	case first != "":
		what := "a dun"
		if update.IsLauncher(first) {
			what = "the launcher"
		}
		lines = append(lines, "warning: "+what+" at "+first+" comes first on PATH, so `dun` runs it, not "+target+
			" — it was not removed; remove it or put "+filepath.Dir(target)+" before "+filepath.Dir(first)+" on PATH")
	case !onPath:
		lines = append(lines, "warning: "+filepath.Dir(target)+" is not on PATH; add it, e.g. export PATH=\""+filepath.Dir(target)+":$PATH\"")
	}
	when := "it takes over when dun next starts"
	if askDundUpgrade(target, res.To) {
		when = "dund moves to it at the next quiet moment"
	}
	lines = append(lines, when+" · plain /upgrade follows releases from now on · /upgrade --channel=source says how to go back")
	return lines, nil
}
