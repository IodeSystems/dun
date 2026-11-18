package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// A tool's version, against what this dun supports (release.Supported). dun
// runs its own pinned copy of each tool, under $DUN_HOME/tools (USER
// 2026-11-18, mcpserver/tools.go), or the person's override:
//
//	pinned copy   the pinned version by construction; a kept older copy
//	              (the pin's is not there yet) is updated by --fix: the
//	              release download on a release build, go install
//	              <module>@<pin> into its directory on any other
//	your build    an override ($DUN_<TOOL>, tools.<name>.path): run
//	              whatever its version; older than Min is a warn, and dun
//	              never updates it
//
// The version is read from the binary's build info, without running it. A
// modified checkout is compared by the version its build info gives
// (v0.7.2-0.<ts>-<sha>+dirty is v0.7.2's pseudo-version, valid semver); a
// build whose version says nothing (a plain go build, "(devel)", v0.0.0-…)
// is not compared.

// Fix actions planTool can name.
const (
	actDownload  = "download"
	actGoInstall = "go install"
)

// toolEnv is what planTool needs from this dun and machine.
type toolEnv struct {
	release bool // this release build has the pin's download
	haveGo  bool // go is on PATH
}

// toolPlan is doctor's line on an installed tool's version, and the update
// --fix may run for it (act "" when there is none or it needs the person).
type toolPlan struct {
	level, detail, fix string
	act, target        string // target: the version act installs
}

// planTool decides it for name resolved as r, built as o.
func planTool(name string, r mcpserver.Resolved, o update.Origin, s release.Tool, env toolEnv) toolPlan {
	if r.Source == mcpserver.SourceOverride {
		pl := toolPlan{level: "ok", detail: name + ": your build, " + o.String()}
		switch {
		case !comparable(o):
			pl.detail += "; unknown version, can't compare it with " + s.Min + " (the oldest this dun runs)"
		case semver.Compare(o.Version, s.Min) < 0:
			pl.level = "warn"
			pl.detail += "; too old for this dun (it needs " + s.Min + " or newer)"
			pl.fix = "update it, or " + undoOverride(name, r) + " to run the pinned " + r.Pin
		}
		return pl
	}
	pl := toolPlan{level: "ok", detail: name + ": pinned " + r.Version + ", " + o.String()}
	if !r.Stale() {
		return pl
	}
	pl.detail += "; a kept copy: this dun pins " + r.Pin
	pl.target = r.Pin
	switch {
	case env.release:
		pl.act, pl.fix = actDownload, "dun doctor --fix (downloads "+name+" "+r.Pin+" from its release; the sha256 is pinned in this build)"
	case env.haveGo:
		pl.act, pl.fix = actGoInstall, "dun doctor --fix (runs go install "+s.Module+"@"+r.Pin+")"
	default:
		pl.fix = "install Go (go.dev/dl), then dun doctor --fix"
	}
	return pl
}

// comparable is a build whose version says how old it is.
func comparable(o update.Origin) bool {
	return semver.IsValid(o.Version) && o.Kind != update.GoBuild && !isUnstamped(o.Version)
}

// undoOverride is how to stop using r's override.
func undoOverride(name string, r mcpserver.Resolved) string {
	if env, ok := strings.CutPrefix(r.Via, "$"); ok {
		return "unset " + env
	}
	return "remove tools." + name + ".path from " + mcpserver.UserServersFile()
}

// toolOrigin is doctor's line on how the tool r names was built, and its
// version against this dun's; with --fix, a kept copy is updated to the pin.
func (d *doctor) toolOrigin(name string, r mcpserver.Resolved) {
	o, err := update.OfFile(r.Path)
	if err != nil {
		return // a script, or a binary without build info
	}
	s, ok := mcpserver.SupportedTool(name)
	if !ok {
		d.add("ok", "origin", name+": "+o.String()+originNote(name, o), "")
		return
	}
	pl := planTool(name, r, o, s, d.toolEnv(name))
	if d.fix && pl.act != "" {
		if err := d.update(name, s, pl); err != nil {
			pl.fix = "--fix failed: " + textfmt.OneLine(err.Error())
		} else if r = mcpserver.ResolveTool(name); r.Path != "" {
			if o, err = update.OfFile(r.Path); err == nil {
				pl = planTool(name, r, o, s, d.toolEnv(name))
			}
		}
	}
	d.add(pl.level, "origin", pl.detail+originNote(name, o), pl.fix)
}

// toolEnv is planTool's view of this dun and machine for name.
func (d *doctor) toolEnv(name string) toolEnv {
	_, _, rel := pinnedTool(name)
	_, err := exec.LookPath("go")
	return toolEnv{release: rel, haveGo: err == nil}
}

// update runs pl's action, saying it first.
func (d *doctor) update(name string, s release.Tool, pl toolPlan) error {
	d.applied++
	switch pl.act {
	case actDownload:
		_, err := installPinned(name)
		return err
	case actGoInstall:
		_, err := goInstallPinned(name, s.Module, pl.target)
		return err
	}
	return errors.New("no update to run")
}

// goInstallPinned builds module@version with go install into name's managed
// directory, $DUN_HOME/tools/<name>/<version>/: into a temp GOBIN beside
// it, then renamed into place, so a session starting meanwhile never runs
// half a binary.
func goInstallPinned(name, module, version string) (string, error) {
	dst := mcpserver.PinnedPath(name, version)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(filepath.Dir(dst)), ".go-install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	mod := module + "@" + version
	fmt.Fprintf(os.Stderr, "installing %s: go install %s (into %s)\n", name, mod, filepath.Dir(dst))
	if _, err := goInstall(mod, tmp); err != nil {
		return "", fmt.Errorf("go install %s: %w", mod, err)
	}
	if err := os.Rename(filepath.Join(tmp, path.Base(module)), dst); err != nil {
		return "", fmt.Errorf("go install %s: %w", mod, err)
	}
	return dst, nil
}

// installModule is what go install builds for a missing name: the version
// this dun supports, else the module's latest.
func installModule(name string) string {
	if s, ok := mcpserver.SupportedTool(name); ok {
		return s.Module + "@" + s.Recommended
	}
	return mcpserver.ToolInstall[name].Module
}

// repoURL is a module's repository page: its path's first three elements.
func repoURL(module string) string {
	repo, _, _ := strings.Cut(module, "@")
	return "https://" + strings.Join(strings.Split(repo, "/")[:3], "/")
}

// polyUpdate is the optional update block of `poly-lsp-mcp doctor --json`:
// how poly-lsp itself sees its own updates (by its origin, against its
// latest release or module version). Absent when it knows nothing.
type polyUpdate struct {
	Origin    string `json:"origin"`
	Current   string `json:"current"`
	Latest    string `json:"latest"` // "" when it cannot compare
	Available bool   `json:"available"`
	How       string `json:"how"`
	Note      string `json:"note"`
}

// parsePolyUpdate reads it from doctor --json's output; nil when absent.
func parsePolyUpdate(out []byte) *polyUpdate {
	var r struct {
		Update *polyUpdate `json:"update"`
	}
	if json.Unmarshal(out, &r) != nil {
		return nil
	}
	return r.Update
}

// addToolUpdate relays a tool's update block (poly-lsp-mcp's shape, which
// raglit's doctor --json copies) as one line: an update the tool knows of,
// or why it cannot tell. Up to date says nothing (the origin line has it).
func (d *doctor) addToolUpdate(name string, u *polyUpdate) {
	area := name
	if name == "poly-lsp-mcp" {
		area = "poly-lsp"
	}
	switch {
	case u == nil:
	case u.Available && u.Latest != "":
		d.add("ok", area, name+" says an update is available: "+textfmt.PlainLine(u.Current)+" → "+textfmt.PlainLine(u.Latest), textfmt.PlainLine(u.How))
	case u.Latest == "" && u.Note != "":
		d.add("ok", area, name+" "+textfmt.PlainLine(u.Current)+": "+textfmt.PlainLine(u.Note), "")
	}
}

// originNote is what the origin line adds about how name was built: a go
// install raglit embeds a placeholder web UI, because the real one needs
// node to build; its release tarballs carry it.
func originNote(name string, o update.Origin) string {
	if name == "raglit" && o.Kind == update.GoInstall {
		return "; a go install build serves a placeholder web UI (the real one needs node to build; raglit's release tarballs include it)"
	}
	return ""
}

// toolWarnings are the session's start lines on the tools the engine runs
// (Ready's hint): each one older than the version this dun pins, by
// path and version, with the version dun pins and the fix. A Mac ran a
// modified mcpshell checkout from ~/.local/bin, found on PATH first, with

// toolWarnings are the session's start lines on the tools the engine runs
// (Ready's hint): an override older than this dun's Min, or a kept copy
// older than the pin on a build that does not fetch, by path and version,
// with the fix. A Mac ran a modified mcpshell checkout from ~/.local/bin,
// found on PATH first, with no word from dun until it crashed. Build info
// only: no tool is run.
func toolWarnings() []string {
	d := &doctor{}
	var out []string
	for _, t := range release.Supported {
		r := mcpserver.ResolveTool(t.Name)
		if r.Path == "" {
			continue // missing: the server's own start says so
		}
		o, err := update.OfFile(r.Path)
		if err != nil {
			continue
		}
		s, _ := mcpserver.SupportedTool(t.Name)
		if w := toolWarning(t.Name, r, o, s, d.toolEnv(t.Name)); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// toolWarning is toolWarnings' line for name resolved as r, or "". A build
// with no version ("(devel)", a plain go build) is not compared, as in
// doctor. A release build fetches the pin itself, so its kept copy is not
// warned about.
func toolWarning(name string, r mcpserver.Resolved, o update.Origin, s release.Tool, env toolEnv) string {
	if !comparable(o) {
		return ""
	}
	pl := planTool(name, r, o, s, env)
	switch {
	case r.Source == mcpserver.SourceOverride && pl.level == "warn":
		return fmt.Sprintf("%s: your build at %s (%s) is %s, older than the %s this dun needs. Fix: %s",
			name, r.Path, r.Via, o.Version, s.Min, pl.fix)
	case r.Stale() && !env.release:
		return fmt.Sprintf("%s: running the kept %s at %s; this dun pins %s. Fix: %s", name, r.Version, r.Path, r.Pin, pl.fix)
	}
	return ""
}

// isUnstamped is a version Go gives a build with no VCS stamp: v0.0.0-<time>-<hash>.
// It says nothing about age.
func isUnstamped(v string) bool {
	return semver.MajorMinor(v) == "v0.0" && module.IsPseudoVersion(v) && strings.HasPrefix(v, "v0.0.0-")
}

// withToolWarnings is Ready's hint with toolWarnings after it: the TUI says
// the hint once per session.
func withToolWarnings(hint string) string {
	lines := toolWarnings()
	if hint != "" {
		lines = append([]string{hint}, lines...)
	}
	return strings.Join(lines, "\n")
}
