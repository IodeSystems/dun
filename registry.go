package dun

import (
	"fmt"
	"sync"
	"sync/atomic"

	dunreg "github.com/iodesystems/dun/internal/registry"
	"github.com/iodesystems/dun/internal/update"
)

// The registry (docs/registry.md, internal/registry) in this process: what
// it changes of what dun was built with. The CLI installs the cached index
// once, before anything reads a manifest (UseRegistry); a fresher index
// fetched later is for the next process, so a session's manifests never
// change under it.
//
//	a bundled extension's manifest  the newest entry for its id that fits
//	                                this dun and the installed tool's version
//	the MCP presets                 the entries for this dun, when there are any
//	a tool's Min and Recommended    raised (never lowered) by its entry

// ExtRegistry is an extension source: a bundled extension's manifest from a
// registry entry (bundled < registry < user < project).
const ExtRegistry = "registry"

type registryUse struct {
	ix  *dunreg.Index
	dun string // this dun's version: entries are matched against it
}

var (
	regUse  atomic.Pointer[registryUse]
	overlay sync.Mutex
	// overlaid is bundled() with the registry applied, made once per index.
	overlaid struct {
		for_ *registryUse
		exts []Extension
		errs []ExtError
	}
)

// UseRegistry installs ix (nil: none) for this process; dunVersion is this
// dun's ("dev" for a dev build, which takes entries for any dun).
func UseRegistry(ix *dunreg.Index, dunVersion string) {
	if ix == nil {
		regUse.Store(nil)
		return
	}
	regUse.Store(&registryUse{ix: ix, dun: dunVersion})
}

// toolVersion is the version of the command installed for a bundled
// extension, from its build info ("" when it is not installed or says
// nothing).
var toolVersion = func(command string) string {
	p, err := FindCommand(command)
	if err != nil {
		return ""
	}
	o, err := update.OfFile(p)
	if err != nil {
		return ""
	}
	return o.Version
}

// withRegistry is the embedded manifests with the registry's entries in
// place of those that fit, and the entries that did not load.
func withRegistry(embedded []Extension) ([]Extension, []ExtError) {
	use := regUse.Load()
	if use == nil {
		return embedded, nil
	}
	overlay.Lock()
	defer overlay.Unlock()
	if overlaid.for_ == use {
		return overlaid.exts, overlaid.errs
	}
	out := make([]Extension, len(embedded))
	copy(out, embedded)
	var errs []ExtError
	for i, e := range out {
		tv := toolVersion(e.Server.Command)
		ent, ok := use.ix.Extension(e.ID, use.dun, tv)
		if !ok {
			continue
		}
		where := fmt.Sprintf("registry: %s %s (for %s %s)", ent.ID, ent.Version, e.Server.Command, ent.Tools)
		re, err := ParseExtension(ent.Manifest)
		if err == nil && re.ID != e.ID {
			err = fmt.Errorf("id %q is not %q", re.ID, e.ID)
		}
		if err != nil {
			errs = append(errs, ExtError{where, err}) // the built-in one stays
			continue
		}
		re.Source, re.Path = ExtRegistry, where
		out[i] = re
	}
	overlaid.for_, overlaid.exts, overlaid.errs = use, out, errs
	return out, errs
}

// RegistryErrors are the registry entries that fit but did not load.
func RegistryErrors() []ExtError {
	_, errs := withRegistry(embeddedExtensions())
	return errs
}

// Presets are the servers /mcp add suggests: the registry's for this dun
// when it has any, else ServerPresets.
func Presets() []ServerPreset {
	use := regUse.Load()
	if use == nil {
		return ServerPresets
	}
	ps := use.ix.PresetsFor(use.dun)
	if len(ps) == 0 {
		return ServerPresets
	}
	out := make([]ServerPreset, 0, len(ps))
	for _, p := range ps {
		out = append(out, ServerPreset{ID: p.ID, Description: p.Description, Command: p.Command, Args: p.Args, URL: p.URL})
	}
	return out
}

// RegistryTool is the registry's pin for tool name, for this dun.
func RegistryTool(name string) (dunreg.ToolEntry, bool) {
	use := regUse.Load()
	if use == nil {
		return dunreg.ToolEntry{}, false
	}
	return use.ix.Tool(name, use.dun)
}
