package dun

import (
	"strings"
	"testing"

	dunreg "github.com/iodesystems/dun/internal/registry"
)

// With a registry, a bundled extension whose entry fits this dun and the
// installed tool is the registry's manifest (its server too), says where it
// came from, and one that fits nothing stays the built-in; an entry that
// fits and does not load is reported, and the built-in stays. Presets come
// from the registry when it has any for this dun.
func TestRegistry_Overlay(t *testing.T) {
	ix, err := dunreg.Parse([]byte(`{"schema": 1, "published": "2026-10-26T10:00:00Z",
	 "extensions": [
	  {"id": "raglit", "version": "1.2.0", "dun": ">=0.1.3", "tools": "~0.2.1",
	   "manifest": {"id": "raglit", "description": "from the registry", "server": {"command": "raglit", "args": ["serve", "--new-flag"]}}},
	  {"id": "poly-lsp", "version": "1.0.0", "dun": ">=0.1.3", "tools": "^9.0.0",
	   "manifest": {"id": "poly-lsp", "server": {"command": "poly-lsp-mcp"}}}],
	 "presets": [{"id": "only", "version": "1.0.0", "dun": ">=0.1.3", "description": "the one", "command": "npx", "args": ["x"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	old := toolVersion
	toolVersion = func(cmd string) string { return map[string]string{"raglit": "v0.2.4", "poly-lsp-mcp": "v0.2.0"}[cmd] }
	t.Cleanup(func() { toolVersion = old; UseRegistry(nil, "") })

	UseRegistry(ix, "v0.1.3")
	byID := map[string]Extension{}
	for _, e := range BundledExtensions() {
		byID[e.ID] = e
	}
	if r := byID["raglit"]; r.Source != ExtRegistry || r.Description != "from the registry" || !strings.Contains(r.Path, "raglit 1.2.0") {
		t.Errorf("raglit: %+v", r)
	}
	if p := byID["poly-lsp"]; p.Source != ExtBundled {
		t.Errorf("poly-lsp v0.2.0 is not ^9.0.0, yet: %+v", p)
	}
	if s := DefaultServers("/ws", "")[2]; s.ID != ServerDocs || strings.Join(s.Args, " ") != "serve --new-flag" {
		t.Errorf("raglit's server is not the registry's: %+v", s)
	}
	if ps := Presets(); len(ps) != 1 || ps[0].ID != "only" {
		t.Errorf("presets: %+v", ps)
	}

	// An older dun reads none of it.
	UseRegistry(ix, "v0.1.2")
	for _, e := range BundledExtensions() {
		if e.Source != ExtBundled {
			t.Errorf("v0.1.2 took %s from the registry", e.ID)
		}
	}
	if len(Presets()) != len(ServerPresets) {
		t.Error("v0.1.2 took the registry's presets")
	}

	// An entry that fits but does not load: reported, the built-in stays.
	bad, _ := dunreg.Parse([]byte(`{"schema": 1, "published": "2026-10-26T10:00:00Z", "extensions": [
	  {"id": "raglit", "version": "1.3.0", "dun": "*", "tools": "*", "manifest": {"id": "not-raglit", "server": {"command": "raglit"}}}]}`))
	UseRegistry(bad, "v0.1.3")
	if errs := RegistryErrors(); len(errs) != 1 || !strings.Contains(errs[0].Error(), "raglit 1.3.0") {
		t.Errorf("errors: %v", errs)
	}
	for _, e := range BundledExtensions() {
		if e.Source != ExtBundled {
			t.Errorf("%s: a bad entry replaced the built-in", e.ID)
		}
	}
}
