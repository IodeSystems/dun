package tui

import (
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/iodesystems/dun/internal/buildinfo"
)

// dundSeen is dund's build from the last hello reply ("" before one). The
// TUI, dund and each session's engine are three processes that can be three
// builds; /upgrade names all of them.
var dundSeen atomic.Value // string

func dundVersion() string {
	s, _ := dundSeen.Load().(string)
	return s
}

// describeRe reads a release tag or a `git describe` build: v0.8.1,
// v0.8.1-9-g67f1169, with an optional -dirty.
var describeRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-(\d+)-g([0-9a-f]+))?(-dirty)?$`)

// compareBuilds orders two builds: -1 a is older, 0 the same, 1 a is newer.
// ok is false when the versions do not say (dev, another format, or two
// builds with the same distance from the tag on different commits).
func compareBuilds(a, b string) (cmp int, ok bool) {
	if a == b {
		return 0, a != ""
	}
	pa, pb := describeRe.FindStringSubmatch(a), describeRe.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return 0, false
	}
	for i := 1; i <= 4; i++ { // major, minor, patch, commits past the tag
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, false // same tag and distance, different commit or dirty
}

// engineUnreported is the engine's build when its ready said none: every
// engine since Ready.Version (merged in 0681425) says it, and dund fills it
// in from an engine link's hello, so one that does not is older than that.
const engineUnreported = "not reported (built before 0681425, so older than this TUI)"

// engineBuild is this session's engine build for versionsReport: "" before
// its ready, engineUnreported when the ready said none.
func (m *tuiModel) engineBuild() string {
	if m.engineVersion == "" && m.engineReady {
		return engineUnreported
	}
	return m.engineVersion
}

// versionsReport is /upgrade's first lines: this TUI, dund and this
// session's engine; one warning line when they differ. "(same)" only when
// all three are known and equal: an unknown one is not the same.
func versionsReport(tui, dund, engine string) []string {
	show := func(label, v string) string {
		if v == "" {
			v = "unknown"
		}
		return label + " " + v
	}
	var differ []string
	if dund != "" && dund != tui {
		differ = append(differ, "dund")
	}
	if engine != "" && engine != tui {
		differ = append(differ, "engine")
	}
	line := "versions: " + show("TUI", tui) + " · " + show("dund", dund) + " · " + show("engine", engine)
	if len(differ) > 0 {
		return []string{
			StDim.Render(line),
			warnLine("this TUI is " + tui + " but " + strings.Join(differ, "/") + " is not: /upgrade now restarts them into this build"),
		}
	}
	if dund == tui && engine == tui {
		line += " (same)"
	}
	return []string{StDim.Render(line)}
}

// newSubcommands are the engine commands' subcommands added after a first
// release, by command. An engine whose command spec does not offer one is
// older than this TUI: engineLacks says so before the TUI sends it, instead
// of the engine answering with its usage string. Add a row to cover a new
// subcommand; nothing else changes.
var newSubcommands = map[string][]string{
	"jobs": {"clear"},
}

// engineLacks reports whether the engine of this session is known not to
// have the subcommand args[0] of /name. Unknown (no commands event yet, or
// not in the table) is false: it is sent.
func (m *tuiModel) engineLacks(name string, args []string) bool {
	if len(args) == 0 {
		return false
	}
	var listed bool
	for _, s := range newSubcommands[name] {
		listed = listed || strings.EqualFold(s, args[0])
	}
	if !listed {
		return false
	}
	for _, c := range m.engineCmds {
		if c.name != name {
			continue
		}
		for _, ch := range c.choices {
			if strings.EqualFold(ch.value, args[0]) {
				return false
			}
		}
		return true
	}
	return false
}

// engineOldLine is the answer to a command this engine lacks: its build
// when known, else only what its commands show (it lacks one, so it is
// older than this TUI).
func (m *tuiModel) engineOldLine(name string, args []string) string {
	cmd := "/" + name + " " + args[0]
	if m.engineVersion != "" {
		return "this session's engine is older (" + m.engineVersion + ") and has no " + cmd + ": /upgrade now (this TUI is " + buildinfo.Version + ")"
	}
	return "this session's engine has no " + cmd + ", so it is older than this TUI (" + buildinfo.Version + "); it does not report its build: /upgrade now"
}
