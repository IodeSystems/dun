package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/depver"
	"github.com/iodesystems/dun/internal/update"
)

// `dun tools [--upgrade] [--json]`: the tools the engines run against the
// versions this dun pins, and with --upgrade the updates doctor --fix would
// run for them (toolver.go's planTool). The TUI's /upgrade runs it (USER
// 2026-11-04: "I was assuming it would be in the /upgrade command"); the
// version is always the pin (release/deps.json, raised by the signed
// registry), never a tool's latest release.

const toolsHelp = `dun tools — the tools dun runs, against the versions it pins

  dun tools              each tool's version, and the update for it if any
  dun tools --upgrade    run those updates: a release tool is downloaded at
                         its pinned version (the sha256 is checked), a go
                         install one is go-installed; a checkout is said
  --json                 one JSON array of the rows

The pin is this build's release/deps.json, raised by the registry (fetched
first). A running engine keeps the tool it started; /upgrade now restarts
them.
`

// toolRow is one tool's line.
type toolRow struct {
	Name    string `json:"name"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"` // as installed, after any update
	Origin  string `json:"origin,omitempty"`  // doctor's words for how it was built
	Target  string `json:"target,omitempty"`  // the version an update goes to
	Note    string `json:"note,omitempty"`    // planTool's detail past the origin
	Fix     string `json:"fix,omitempty"`     // what the person runs, when dun cannot
	Can     bool   `json:"can,omitempty"`     // --upgrade can run the update
	Updated bool   `json:"updated,omitempty"` // --upgrade ran it and it worked
	Error   string `json:"error,omitempty"`
	Missing bool   `json:"missing,omitempty"`
	Pin     string `json:"pin,omitempty"`     // the version this dun pins (the download, else Recommended)
	Release bool   `json:"release,omitempty"` // a release build of the tool
}

// dep is the row as /version and dun --version show it.
func (r toolRow) dep() depver.Tool {
	return depver.Tool{Name: r.Name, Path: r.Path, Version: r.Version, Origin: r.Origin,
		Pin: r.Pin, Release: r.Release, Missing: r.Missing}
}

// pinFor is the version this dun pins for name: the release download, else
// the version it supports (release.Supported, raised by the registry).
func pinFor(name string) string {
	if a, _, ok := pinnedTool(name); ok {
		return a.Version
	}
	if s, ok := supportedTool(name); ok {
		return s.Recommended
	}
	return ""
}

func toolsCmd(args []string, stdout, stderr io.Writer) int {
	var upgrade, asJSON bool
	for _, a := range args {
		switch a {
		case "--upgrade":
			upgrade = true
		case "--json":
			asJSON = true
		case "-h", "--help":
			fmt.Fprint(stdout, toolsHelp)
			return 0
		default:
			fmt.Fprintf(stderr, "dun tools: no flag %s\n\n%s", a, toolsHelp)
			return 2
		}
	}
	// The newest pins first: a tool release reaches users through the
	// registry without a dun release.
	if s := registryStore(); s != nil {
		s.MaxAge = 0
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if ix, _, err := s.Refresh(ctx); err == nil && ix != nil {
			dun.UseRegistry(ix, buildinfo.Version)
		}
		cancel()
	}
	rows := toolRows(upgrade)
	if asJSON {
		_ = json.NewEncoder(stdout).Encode(rows)
	} else {
		for _, r := range rows {
			fmt.Fprintln(stdout, r.line())
		}
	}
	for _, r := range rows {
		if r.Error != "" {
			return 1
		}
	}
	return 0
}

// toolRows checks each tool the engines run (doctorTools but git); with
// upgrade, runs the updates dun can run.
func toolRows(upgrade bool) []toolRow {
	var rows []toolRow
	d := &doctor{fix: true}
	for _, t := range doctorTools {
		if t.name == "git" {
			continue
		}
		r := toolRow{Name: t.name, Pin: pinFor(t.name)}
		p, err := dun.FindCommand(t.name)
		if err != nil {
			r.Missing = true
			if fixTools[t.name] {
				r.Fix = "dun doctor --fix installs it"
			}
			rows = append(rows, r)
			continue
		}
		r.Path = p
		o, err := update.OfFile(p)
		if err != nil {
			r.Note = "no build info: its version is not known"
			rows = append(rows, r)
			continue
		}
		r.Version, r.Origin, r.Release = o.Version, o.String(), o.Kind == update.Release
		s, ok := supportedTool(t.name)
		if !ok {
			rows = append(rows, r)
			continue
		}
		pl := planTool(t.name, p, o, s, d.toolEnv(t.name, p))
		r.Note = trimPrefix(pl.detail, t.name+": "+o.String())
		r.Target, r.Fix, r.Can = pl.target, pl.fix, pl.act != ""
		if upgrade && r.Can {
			if err := d.update(t.name, p, s, pl); err != nil {
				r.Error = err.Error()
			} else {
				r.Updated = true
				if o, err := update.OfFile(p); err == nil {
					r.Version, r.Origin, r.Release = o.Version, o.String(), o.Kind == update.Release
				}
			}
		}
		rows = append(rows, r)
	}
	return rows
}

func trimPrefix(s, p string) string {
	if len(s) >= len(p) && s[:len(p)] == p {
		s = s[len(p):]
	}
	if len(s) >= 2 && s[:2] == "; " {
		s = s[2:]
	}
	return s
}

// line is a row as `dun tools` prints it.
func (r toolRow) line() string {
	switch {
	case r.Missing:
		s := r.Name + ": not installed"
		if r.Fix != "" {
			s += " (" + r.Fix + ")"
		}
		return s
	case r.Error != "":
		return r.Name + ": " + r.Origin + "; the update to " + r.Target + " failed: " + r.Error
	case r.Updated:
		return r.Name + ": updated to " + r.Version
	}
	s := r.Name + ": " + r.Origin
	if r.Note != "" {
		s += "; " + r.Note
	}
	if r.Target != "" && r.Fix != "" {
		if r.Can {
			s += " — dun tools --upgrade (or /upgrade now) installs it"
		} else {
			s += " — " + r.Fix
		}
	}
	return s
}

// versionBlock is what dun --version prints after its first line: the
// libraries compiled in and the tools dun runs, a line each. The registry is
// the cached one (main installed it); no tool is run.
func versionBlock() []string {
	out := []string{"modules:"}
	for _, l := range depver.ModuleLines() {
		out = append(out, "  "+l)
	}
	out = append(out, "tools:")
	for _, r := range toolRows(false) {
		out = append(out, "  "+r.dep().Line())
	}
	return out
}
