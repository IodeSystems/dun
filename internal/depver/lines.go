package depver

import (
	"runtime/debug"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// ModuleLines are the libraries compiled in, one line each: "raglit v0.9.0
// (ragcore)", and what is off — a version that is not go.mod's, a directory
// replace, a version read from go.mod because the build info has none.
func ModuleLines() []string { return moduleLines(deps()) }

func moduleLines(deps map[string]*debug.Module) []string {
	var out []string
	for _, m := range Modules {
		v, from := version(m.Path, deps)
		s := m.Name + " " + v
		if m.Part != "" {
			s += " (" + m.Part + ")"
		}
		var notes []string
		switch {
		case from == "go.mod":
			notes = append(notes, "from go.mod: this binary's build info lists no modules")
		case strings.HasPrefix(from, "replaced by "):
			notes = append(notes, from)
		}
		if v != m.Pin {
			notes = append(notes, "go.mod pins "+m.Pin)
		}
		if len(notes) > 0 {
			s += " — " + strings.Join(notes, "; ")
		}
		out = append(out, s)
	}
	return out
}

// Tool is a tool dun runs, as `dun tools --json` reports it (cmd/dun
// toolscmd.go's toolRow has these fields under the same names).
type Tool struct {
	Name    string `json:"name"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"` // from its build info; "" when it has none
	Origin  string `json:"origin,omitempty"`  // how it was built, in doctor's words
	Pin     string `json:"pin,omitempty"`     // the version this dun pins for it
	Release bool   `json:"release,omitempty"` // it is a release build
	Missing bool   `json:"missing,omitempty"`
}

// Line is the tool's version line: "mcpshell v0.12.0 at /p (release); pinned
// v0.12.0", with "older than the pin", "differs from the pin" or
// "unknown version" after it. A build past the pin (a pseudo-version after the
// tag, +dirty) is not flagged for not being a release.
func (t Tool) Line() string {
	pin := ""
	if t.Pin != "" {
		pin = "; pinned " + t.Pin
	}
	switch {
	case t.Missing:
		return t.Name + ": not installed" + pin
	case t.Version == "":
		return t.Name + " at " + t.Path + ": version not known (no build info)" + pin
	}
	s := t.Name + " " + t.Version + " at " + t.Path
	switch {
	case t.Release:
		s += " (release)"
	case t.Origin != "":
		s += " (" + t.Origin + ")"
	}
	s += pin
	var marks []string
	switch {
	case !semver.IsValid(t.Version) || (module.IsPseudoVersion(t.Version) && strings.HasPrefix(t.Version, "v0.0.0-")):
		marks = append(marks, "unknown version (no release tag in its build info)")
	case t.Pin != "" && semver.Compare(t.Version, t.Pin) < 0:
		marks = append(marks, "older than the pin")
	case t.Pin != "" && t.Version != t.Pin:
		marks = append(marks, "differs from the pin")
	}
	if len(marks) > 0 {
		s += " — " + strings.Join(marks, ", ")
	}
	return s
}
