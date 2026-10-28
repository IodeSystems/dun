package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
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
	for _, c := range ownCommands("") {
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
	errs := append(h.ExtensionErrors(), dun.RegistryErrors()...)
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

// extensionsEvent is every extension for a UI's /ext list.
func extensionsEvent(h *dun.Harness) *proto.Extensions {
	exts, refused := engineExtensions(h.Extensions())
	ev := &proto.Extensions{Items: []proto.ExtItem{}, Errors: refused,
		AddPath: dun.UserExtensionsDir() + "/<id>/" + dun.ExtensionFile}
	for _, e := range h.ExtensionErrors() {
		ev.Errors = append(ev.Errors, e.Error())
	}
	for _, e := range dun.RegistryErrors() { // an entry that fit and did not load: the built-in one is in use
		ev.Errors = append(ev.Errors, e.Error())
	}
	for _, e := range exts {
		it := proto.ExtItem{ID: e.ID, Source: e.Source, Path: e.Path, Description: e.Description, Slash: e.SlashName(), Hooks: e.Hooks}
		if e.Slash != nil {
			for _, n := range slices.Sorted(maps.Keys(e.Slash.Commands)) {
				it.Commands = append(it.Commands, proto.ExtVerb{Name: n, Help: e.Slash.Commands[n].Help})
			}
		}
		if r := e.Roles.Search; r != nil {
			it.Roles = append(it.Roles, "search:"+r.Tool)
		}
		if r := e.Roles.Index; r != nil {
			idx := "index"
			if r.Tool != "" {
				idx += ":" + r.Tool + "/" + r.Forget
			}
			it.Roles = append(it.Roles, idx)
		}
		for _, k := range slices.Sorted(maps.Keys(e.HostHooks)) {
			it.HostHooks = append(it.HostHooks, k+"="+e.HostHooks[k])
		}
		ev.Items = append(ev.Items, it)
	}
	return ev
}

// extIndexVerb reports whether /<slash> <verb> is one of the index commands
// e's roles give it (dun/attach.go): watch with an index CLI, attach and
// detach with a search role. An extension's own command of the same name
// wins (routeSlash asks it first).
func extIndexVerb(e dun.Extension, verb string) bool {
	switch verb {
	case "watch":
		return e.Roles.Index != nil && e.Roles.Index.CLI != ""
	case "attach", "detach":
		return e.Roles.Search != nil
	case "workspace": // whether the workspace is read into an index (dun/ragworkspace.go)
		return e.Roles.Search != nil && e.Subscribes(dun.HookWorkspaceOpen)
	}
	return false
}

// parseWatch reads "<dir> [--index N]"; the dir may hold spaces.
func parseWatch(args []string) (dir, index string, err error) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--index" || a == "-index":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--index needs a name")
			}
			i++
			index = args[i]
		case strings.HasPrefix(a, "--index="):
			index = strings.TrimPrefix(a, "--index=")
		case strings.HasPrefix(a, "-"):
			return "", "", fmt.Errorf("unknown flag %s", a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) == 0 {
		return "", "", fmt.Errorf("which directory?")
	}
	return strings.Join(pos, " "), index, nil
}

// parseAttach reads "<index> [--session|--project|--global]" (bare: the
// session); no index, or --list, lists.
func parseAttach(args []string) (index, scope string, list bool, err error) {
	scope = dun.ScopeSession
	for _, a := range args {
		if a == "--list" || a == "-l" {
			list = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			s, ok := dun.ParseScope(a)
			if !ok {
				return "", "", false, fmt.Errorf("unknown flag %s", a)
			}
			scope = s
			continue
		}
		if index != "" {
			return "", "", false, fmt.Errorf("one index at a time")
		}
		index = a
	}
	return index, scope, list || index == "", nil
}

// extIndexCmd carries out /<slash> watch|attach|detach for extension id;
// the answer is the message shown.
func extIndexCmd(ctx context.Context, h *dun.Harness, verb, id, rest string) string {
	slash := id
	for _, e := range h.Extensions() {
		if e.ID == id && e.SlashName() != "" {
			slash = e.SlashName()
		}
	}
	args := strings.Fields(rest)
	switch verb {
	case "watch":
		dir, index, err := parseWatch(args)
		if err != nil {
			return "usage: /" + slash + " watch <dir> [--index NAME] — " + err.Error()
		}
		index, out, err := h.WatchDir(ctx, id, dir, index)
		if err != nil {
			return "error: " + err.Error()
		}
		return out + "\nwatched into index " + index + " — /" + slash + " attach " + index +
			" to search it in this session (" + h.AttachName(id, index) + " from another project's)"
	case "workspace":
		return workspaceCmd(h, slash, args)
	case "attach", "detach":
		index, scope, list, err := parseAttach(args)
		if err != nil {
			return "usage: /" + slash + " " + verb + " <index>|<project>:<index> [--project|--global] — " + err.Error()
		}
		if list {
			if verb == "detach" {
				return "usage: /" + slash + " detach <index> [--project|--global]"
			}
			return attachListing(h, id, slash)
		}
		name, path, changed, err := h.AttachIndex(ctx, id, index, scope, verb == "attach")
		switch {
		case err != nil:
			return "error: " + err.Error()
		case !changed && verb == "attach":
			return name + " is already attached at " + scope
		case !changed:
			return name + " is not attached at " + scope + "\n" + attachListing(h, id, slash)
		}
		msg := name + " " + verb + "ed (" + scope + ")"
		if path != "" {
			msg += " in " + path
		}
		return msg + "\n" + attachListing(h, id, slash)
	}
	return "unknown command " + verb
}

// attachListing is bare /<slash> attach: each attached index and its scopes.
func attachListing(h *dun.Harness, id, slash string) string {
	at, err := h.Attachments(id)
	if err != nil {
		return "error: " + err.Error()
	}
	if len(at) == 0 {
		return slash + ": no indexes attached — /" + slash + " attach <index>|<project>:<index> [--project|--global]"
	}
	var b strings.Builder
	b.WriteString(slash + " attached (searched with this project's indexes when a search names none):")
	for _, a := range at {
		fmt.Fprintf(&b, "\n  %-28s %s", a.Index, strings.Join(a.Scopes, ", "))
	}
	return b.String()
}

// workspaceCmd is /<slash> workspace [on|off] [--session|--project|--global]:
// whether the workspace is read into an index; bare says. The scope is the
// project's unless named (USER 2026-10-27).
func workspaceCmd(h *dun.Harness, slash string, args []string) string {
	usage := "usage: /" + slash + " workspace [on|off] [--session|--project|--global]"
	scope, set := dun.ScopeProject, ""
	for _, a := range args {
		switch {
		case a == "on" || a == "off":
			set = a
		case strings.HasPrefix(a, "-"):
			s, ok := dun.ParseScope(a)
			if !ok {
				return usage + " — unknown flag " + a
			}
			scope = s
		default:
			return usage
		}
	}
	if set == "" {
		on, from := h.WorkspaceIndexed()
		if from == "" {
			from = "the default"
		}
		state := "read into this project's index"
		if !on {
			state = "not read; search reads the project's own indexes and attached ones"
		}
		return fmt.Sprintf("workspace: %s (%s) — /%s workspace on|off [--session|--project|--global]", state, from, slash)
	}
	msg, err := h.SetWorkspaceIndexed(set == "on", scope)
	if err != nil {
		return "error: " + err.Error()
	}
	if set == "on" {
		msg += "; read when " + slash + " next starts"
	}
	return msg
}
