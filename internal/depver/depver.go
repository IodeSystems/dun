// Package depver says which versions of dun's own libraries are compiled
// into this binary, and formats the version lines /version and
// `dun --version` show for them and for the tools dun runs.
//
// A module's version comes from the binary's build info, and from go.mod's
// require line (Pins, checked against go.mod by a test) when the build info
// has no entry for it. A test binary built by Go 1.26 carries no
// dependencies in its build info (Go 1.27's does), so a test run by CI's Go
// read poly-lsp-mcp as "devel" where a local run read v0.5.0 (the v0.4.0
// release run, 2026-11-09).
package depver

import (
	"runtime/debug"
	"sync"
)

// The module paths of dun's own libraries.
const (
	AgentKit    = "github.com/iodesystems/agentkit"
	Shim        = "github.com/iodesystems/openai-claude-api-shim"
	Raglit      = "github.com/iodesystems/raglit"
	PolyLSP     = "github.com/iodesystems/poly-lsp-mcp"
	Gwag        = "github.com/iodesystems/gwag/v2"
	BubbleTeaMD = "github.com/iodesystems/bubbleteandmd"
)

// Module is one of dun's libraries: its name, the part of it dun uses, and
// its go.mod pin.
type Module struct {
	Name, Part, Path, Pin string
}

// Modules are the libraries /version lists, in that order. Pin is go.mod's
// require version: TestPinsMatchGoMod fails when go.mod moves and these do
// not.
var Modules = []Module{
	{Name: "agentkit", Path: AgentKit, Pin: "v0.29.0"},
	{Name: "openai-claude-api-shim", Path: Shim, Pin: "v0.6.0"},
	{Name: "raglit", Part: "ragcore", Path: Raglit, Pin: "v0.9.0"},
	{Name: "poly-lsp-mcp", Part: "segment", Path: PolyLSP, Pin: "v0.5.1"},
	{Name: "gwag", Part: "gat", Path: Gwag, Pin: "v2.11.0"},
	{Name: "bubbleteandmd", Part: "render", Path: BubbleTeaMD, Pin: "v0.4.0"},
}

// Devel is the version of a module replaced by a directory (a go.work or a
// replace line): its code is whatever that directory held.
const Devel = "devel"

var (
	buildOnce sync.Once
	buildDeps map[string]*debug.Module
)

func deps() map[string]*debug.Module {
	buildOnce.Do(func() {
		buildDeps = map[string]*debug.Module{}
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, d := range bi.Deps {
				buildDeps[d.Path] = d
			}
		}
	})
	return buildDeps
}

// Version is path's version in this binary: the build info's, Devel for a
// directory replace, else the go.mod pin (a binary whose build info lists no
// dependencies). "" for a module that is not one of Modules and not in the
// build info.
func Version(path string) string {
	v, _ := version(path, deps())
	return v
}

// version is Version, and how it was found: "build" (the build info),
// "replaced" (a directory replace, with the directory) or "go.mod".
func version(path string, deps map[string]*debug.Module) (v, from string) {
	if d, ok := deps[path]; ok {
		if d.Replace != nil {
			if d.Replace.Version != "" {
				return d.Replace.Version, "build"
			}
			return Devel, "replaced by " + d.Replace.Path
		}
		if d.Version != "" && d.Version != "(devel)" {
			return d.Version, "build"
		}
		return Devel, "build"
	}
	for _, m := range Modules {
		if m.Path == path {
			return m.Pin, "go.mod"
		}
	}
	return "", ""
}
