package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/iodesystems/dun"
)

// Extensions as the engine's commands see them (dun/extension.go): each
// one's slash command joins the command list and routeSlash. /ext lists
// them — a command of its own rather than a column in /mcp, because /mcp is
// about servers and their switches, and an extension is a manifest: where it
// was loaded from, what dun asks of it and sends it, and why one did not load.
// Its server is in /mcp like any other.

// engineExtensions are the extensions whose slash commands the engine
// offers. A user's or project's that would shadow one of dun's commands, the
// engine's or the TUI's own, keeps its server and hooks and loses the slash
// command; refused says which.
func engineExtensions(all []dun.Extension) (ok []dun.Extension, refused []string) {
	taken := map[string]bool{}
	for _, c := range ownCommands(false) {
		taken[c.Name] = true
	}
	for _, c := range slashCommands {
		taken[c.name] = true
	}
	for _, e := range all {
		if n := e.SlashName(); n != "" && e.Source != dun.ExtBundled && taken[n] {
			refused = append(refused, fmt.Sprintf("%s: slash.name: /%s is dun's own command; loaded without a slash command", e.ID, n))
			e.Slash = nil
		}
		ok = append(ok, e)
	}
	return ok, refused
}

// extListing is /ext: every extension, then the manifests that did not load.
func extListing(h *dun.Harness) string {
	exts, refused := engineExtensions(h.Extensions())
	running := map[string]bool{}
	for _, st := range h.Servers() {
		running[st.ID] = st.Running
	}
	var b strings.Builder
	b.WriteString("extensions (bundled < user < project; same id replaces):")
	for _, e := range exts {
		state := "off"
		if running[e.ID] {
			state = "running"
		}
		fmt.Fprintf(&b, "\n  %-12s %-8s server %s (%s)", e.ID, e.Source, e.ID, state)
		if n := e.SlashName(); n != "" {
			b.WriteString(" · /" + n)
		}
		var roles []string
		if r := e.Roles.Search; r != nil {
			roles = append(roles, "search:"+r.Tool)
		}
		if r := e.Roles.Index; r != nil {
			idx := "index:"
			if r.Tool != "" {
				idx += r.Tool + "/" + r.Forget
			}
			if r.CLI != "" {
				idx += " (cli fallback " + r.CLI + ")"
			}
			roles = append(roles, idx)
		}
		if len(roles) > 0 {
			b.WriteString(" · roles " + strings.Join(roles, ", "))
		}
		if len(e.Hooks) > 0 {
			b.WriteString(" · hooks " + strings.Join(e.Hooks, ", "))
		}
		keys := make([]string, 0, len(e.HostHooks))
		for k := range e.HostHooks {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(" · host hook " + k + "=" + e.HostHooks[k])
		}
		if e.Path != "" {
			b.WriteString("\n    " + e.Path)
		}
		if e.Description != "" {
			b.WriteString("\n    " + e.Description)
		}
	}
	errs := h.ExtensionErrors()
	if len(errs) > 0 || len(refused) > 0 {
		b.WriteString("\nnot loaded:")
		for _, e := range errs {
			b.WriteString("\n  " + e.Error())
		}
		for _, r := range refused {
			b.WriteString("\n  " + r)
		}
	}
	fmt.Fprintf(&b, "\nadd one: %s/<id>/%s, or .dun/extensions/<id>/ in a project (examples/extensions in dun's repo)", dun.UserExtensionsDir(), dun.ExtensionFile)
	return b.String()
}
