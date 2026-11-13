package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/sys/unix"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// A tool's version, against what this dun supports (release.Supported):
// older than Min is a warn (too old for this dun), older than Recommended an
// ok with a note (an update is available). The version is read from the
// binary's build info, without running it. How it updates follows how the
// TOOL was built, not dun (owner, item 49):
//
//	tool from a release   → the release this dun pins (a release dun's --fix downloads it)
//	tool from go install  → go install <module>@<recommended> (--fix runs it)
//	tool from a checkout  → rebuild it in that checkout (said, never run)
//
// --fix never downgrades and never touches a checkout. A modified checkout
// is compared by the version its build info gives (v0.7.2-0.<ts>-<sha>+dirty
// is v0.7.2's pseudo-version, valid semver): a Mac ran such a build, far
// below Min, for weeks while doctor said ok. Only a build whose version says
// nothing (a plain go build, "(devel)") is not compared: one ok line says
// so, with no update to nag about.

// Fix actions planTool can name.
const (
	actDownload  = "download"
	actGoInstall = "go install"
)

// toolEnv is what planTool needs from this dun and machine.
type toolEnv struct {
	pinned   string // the version this release build pins for the tool; "" on other builds
	beside   bool   // the tool is in dun's directory, where a download goes
	haveGo   bool   // go is on PATH
	writable bool   // the tool's directory is writable (go install replaces it there)
}

// toolPlan is doctor's line on an installed tool's version, and the update
// --fix may run for it (act "" when there is none or it needs the person).
type toolPlan struct {
	level, detail, fix string
	act, target        string // target: the version act installs
}

// planTool decides it for name at p, built as o.
func planTool(name, p string, o update.Origin, s release.Tool, env toolEnv) toolPlan {
	pl := toolPlan{level: "ok", detail: name + ": " + o.String()}
	v := o.Version
	if !semver.IsValid(v) || o.Kind == update.GoBuild || isUnstamped(v) {
		if o.Kind != update.Release && o.Kind != update.GoInstall {
			pl.detail += "; unknown version, can't compare it with " + s.Recommended + " (this dun's)"
		}
		return pl
	}
	target := s.Recommended
	if o.Kind == update.Release && env.pinned != "" {
		target = env.pinned
	}
	old, behind := semver.Compare(v, s.Min) < 0, semver.Compare(v, target) < 0
	switch {
	case old:
		pl.level = "warn"
		pl.detail += "; too old for this dun (it needs " + s.Min + " or newer)"
	case behind:
		pl.detail += "; an update is available: " + target + " (tested with this dun)"
		if o.Kind == update.Checkout && o.Modified {
			pl.level = "warn" // unreleased code, and behind what this dun was tested with
		}
	default:
		return pl
	}
	if semver.Compare(target, v) <= 0 {
		return pl // never a downgrade (the minimum is past what this dun installs)
	}
	pl.target = target
	switch o.Kind {
	case update.Release:
		switch {
		case env.pinned == "":
			pl.fix = "download " + target + " or newer from " + repoURL(s.Module) + "/releases"
		case env.beside:
			pl.act = actDownload
			pl.fix = "dun doctor --fix (downloads " + name + " " + target + " from its release; the sha256 is pinned in this build)"
		default:
			pl.fix = "remove " + p + ", then dun doctor --fix downloads " + name + " " + target + " beside dun"
		}
	case update.GoInstall:
		mod := s.Module + "@" + target
		switch {
		case !env.haveGo:
			pl.fix = "go install " + mod + " (needs Go: go.dev/dl)"
		case !env.writable:
			pl.fix = "go install " + mod + ", as a user who can write " + filepath.Dir(p)
		default:
			pl.act = actGoInstall
			pl.fix = "dun doctor --fix (runs go install " + mod + ")"
		}
	case update.Checkout:
		pl.fix = "rebuild it from its checkout (make install)"
		if o.SrcDir != "" {
			pl.fix = "rebuild it in " + o.SrcDir + " (make install)"
		}
		if o.Modified {
			pl.detail = name + ": a modified checkout of " + v + " (at " + o.Revision + ")" + strings.TrimPrefix(pl.detail, name+": "+o.String())
			where := "its checkout"
			if o.SrcDir != "" {
				where = o.SrcDir
			}
			pl.fix = "rebuild it in " + where + " from a current tag (make install), or remove " + p +
				" and run dun doctor --fix to get the pinned release"
		}
	}
	return pl
}

// toolOrigin is doctor's line on how a tool at p was built, and on a
// supported tool its version against this dun's; with --fix, a release or
// go install update is applied.
func (d *doctor) toolOrigin(name, p string) {
	o, err := update.OfFile(p)
	if err != nil {
		return // a script, or a binary without build info
	}
	s, ok := supportedTool(name)
	if !ok {
		fix := ""
		if o.Kind == update.GoInstall {
			fix = "to update: go install " + o.Module + "@latest"
			if inst, ok := dun.ToolInstall[name]; ok {
				fix = "to update: go install " + inst.Module
			}
		}
		d.add("ok", "origin", name+": "+o.String()+originNote(name, o), fix)
		return
	}
	pl := planTool(name, p, o, s, d.toolEnv(name, p))
	if d.fix && pl.act != "" {
		if err := d.update(name, p, s, pl); err != nil {
			pl.fix = "--fix failed: " + textfmt.OneLine(err.Error())
		} else if o, err = update.OfFile(p); err == nil {
			pl = planTool(name, p, o, s, d.toolEnv(name, p))
		}
	}
	d.add(pl.level, "origin", pl.detail+originNote(name, o), pl.fix)
}

// toolEnv is planTool's view of this dun and machine for the tool at p.
func (d *doctor) toolEnv(name, p string) toolEnv {
	env := toolEnv{beside: besideDun(p), writable: unix.Access(filepath.Dir(p), unix.W_OK) == nil}
	if a, _, ok := pinnedTool(name); ok {
		env.pinned = a.Version
	}
	_, err := exec.LookPath("go")
	env.haveGo = err == nil
	return env
}

// update runs pl's action, saying it first.
func (d *doctor) update(name, p string, s release.Tool, pl toolPlan) error {
	d.applied++
	switch pl.act {
	case actDownload:
		_, err := installPinned(name)
		return err
	case actGoInstall:
		mod := s.Module + "@" + pl.target
		fmt.Fprintf(os.Stderr, "updating %s: go install %s (into %s)\n", name, mod, filepath.Dir(p))
		_, err := goInstall(mod, filepath.Dir(p))
		return err
	}
	return errors.New("no update to run")
}

// supportedTool is name's entry in release.Supported, its Min and
// Recommended raised by the registry's pin for this dun (never lowered).
func supportedTool(name string) (release.Tool, bool) {
	s, ok := release.SupportedTool(name)
	if !ok {
		return s, false
	}
	if t, rok := dun.RegistryTool(name); rok {
		if semver.Compare(t.Min, s.Min) > 0 {
			s.Min = t.Min
		}
		if semver.Compare(t.Recommended, s.Recommended) > 0 {
			s.Recommended = t.Recommended
		}
	}
	return s, true
}

// installModule is what go install builds for a missing name: the version
// this dun supports, else the module's latest.
func installModule(name string) string {
	if s, ok := supportedTool(name); ok {
		return s.Module + "@" + s.Recommended
	}
	return dun.ToolInstall[name].Module
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
// no word from dun until it crashed. Build info only: no tool is run.
func toolWarnings() []string {
	d := &doctor{}
	var out []string
	for _, t := range release.Supported {
		p, err := dun.FindCommand(t.Name)
		if err != nil {
			continue // missing: the server's own start says so
		}
		o, err := update.OfFile(p)
		if err != nil {
			continue
		}
		s, _ := supportedTool(t.Name)
		if w := toolWarning(t.Name, p, o, s, d.toolEnv(t.Name, p)); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// toolWarning is toolWarnings' line for name at p, or "". A build with no
// version ("(devel)", a plain go build) is not compared, as in doctor.
func toolWarning(name, p string, o update.Origin, s release.Tool, env toolEnv) string {
	v := o.Version
	if !semver.IsValid(v) || o.Kind == update.GoBuild || isUnstamped(v) {
		return ""
	}
	pin := s.Recommended
	if env.pinned != "" {
		pin = env.pinned
	}
	// Semver orders a pseudo-version (v0.12.2-0.<time>-<hash>) after the tag
	// it follows, and ignores +dirty: at or past the pin is fine.
	if semver.Compare(v, pin) >= 0 {
		return ""
	}
	pl := planTool(name, p, o, s, env)
	fix := pl.fix
	if fix == "" {
		fix = "remove " + p + " and run dun doctor --fix to get the pinned release"
	}
	return fmt.Sprintf("%s: %s is %s, older than the %s this dun pins. Fix: %s", name, p, v, pin, fix)
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
