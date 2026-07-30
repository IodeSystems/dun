package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/iodesystems/dun"
)

// Tool servers the user can turn on and off: /rag (raglit, the docs index) and
// /lsp (poly-lsp-mcp, semantic code). Neither starts on its own — see
// dun.DefaultServers for why — so this is how a session gets them, and how a
// machine that wants them every time says so once.
//
// The command names are what the tools are called; the server ids are the tool
// families they provide.
var serverAliases = map[string]string{
	"rag": dun.ServerDocs,
	"lsp": dun.ServerCode,
}

// serverLabel renders "rag (docs)" for messages: the command the user types,
// then the id they will see in logs and config files.
func serverLabel(alias string) string {
	return alias + " (" + serverAliases[alias] + ")"
}

// aliasOf is serverAliases reversed: the command name for a server id, or the
// id itself for a server that has no command (shell, or a project's own).
func aliasOf(id string) string {
	for alias, sid := range serverAliases {
		if sid == id {
			return alias
		}
	}
	return id
}

// tristate is a bool flag that remembers whether it was given at all, so
// "--rag=false" (force off for this run) is distinguishable from not passing
// --rag (use the saved setting).
type tristate struct {
	set bool
	val bool
}

func (t *tristate) String() string {
	if t == nil || !t.set {
		return ""
	}
	return strconv.FormatBool(t.val)
}

func (t *tristate) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return fmt.Errorf("want true or false, got %q", s)
	}
	t.set, t.val = true, v
	return nil
}

// IsBoolFlag lets the flag package accept a bare --rag (as --rag=true).
func (t *tristate) IsBoolFlag() bool { return true }

// autostartOverrides turns the --rag/--lsp flags into a dun.Config override.
// Empty when neither was passed, which is what leaves the saved setting alone.
func autostartOverrides(rag, lsp tristate) map[string]bool {
	out := map[string]bool{}
	if rag.set {
		out[dun.ServerDocs] = rag.val
	}
	if lsp.set {
		out[dun.ServerCode] = lsp.val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// runServerCmd applies one /rag or /lsp action and returns the line to show the
// user. A failure to start is a RETURNED MESSAGE, never a fatal error: a
// misconfigured raglit should cost you search, not your session.
//
// Actions: "" or "status" (report), "on", "off", "auto" (start + remember),
// "manual" (stop remembering, leave running).
func runServerCmd(ctx context.Context, h *dun.Harness, alias, action string) string {
	id, ok := serverAliases[alias]
	if !ok {
		id = alias // a project-defined server, addressed by its id
	}
	label := alias
	if _, known := serverAliases[alias]; known {
		label = serverLabel(alias)
	}
	switch action {
	case "", "status":
		return serverStatusLine(h, id, label)
	case "on":
		if err := h.StartServer(ctx, id); err != nil {
			return label + ": did not start — " + oneLine(err.Error()) + "\ncarrying on without it; fix and try /" + alias + " on again"
		}
		return label + ": " + runningSummary(h, id)
	case "off":
		if err := h.StopServer(id); err != nil {
			return label + ": " + err.Error()
		}
		return label + ": stopped"
	case "auto":
		path, err := h.SetAutostart(id, true)
		if err != nil {
			return label + ": " + err.Error()
		}
		msg := label + ": autostart on (saved to " + path + ")"
		if err := h.StartServer(ctx, id); err != nil {
			return msg + "\nbut it did not start now — " + oneLine(err.Error())
		}
		return msg + "\n" + label + ": " + runningSummary(h, id)
	case "manual":
		path, err := h.SetAutostart(id, false)
		if err != nil {
			return label + ": " + err.Error()
		}
		return label + ": autostart off (saved to " + path + ") — still running this session; /" + alias + " off to stop it"
	default:
		return "unknown action " + strconv.Quote(action) + " — try /" + alias + " [on|off|auto|manual]"
	}
}

// serverStatusLine is the bare "/rag" report: what it is doing now, what it
// will do next session, and the one command that changes each.
func serverStatusLine(h *dun.Harness, id, label string) string {
	st, ok := stateOf(h, id)
	if !ok {
		return label + ": not configured"
	}
	alias := aliasOf(id)
	var b strings.Builder
	b.WriteString(label + ": ")
	if st.Running {
		b.WriteString(fmt.Sprintf("running · %d tools", st.Tools))
	} else {
		b.WriteString("stopped")
	}
	if st.Auto {
		b.WriteString(" · autostart on")
	} else {
		b.WriteString(" · autostart off")
	}
	if st.Err != "" {
		b.WriteString("\nlast start failed: " + oneLine(st.Err))
	}
	switch {
	case !st.Running && !st.Auto:
		b.WriteString("\n/" + alias + " on to start it now · /" + alias + " auto to start it every session")
	case !st.Running && st.Auto:
		b.WriteString("\n/" + alias + " on to start it now")
	case st.Running && !st.Auto:
		b.WriteString("\n/" + alias + " auto to start it every session · /" + alias + " off to stop it")
	default:
		b.WriteString("\n/" + alias + " manual to stop starting it automatically")
	}
	return b.String()
}

func runningSummary(h *dun.Harness, id string) string {
	st, ok := stateOf(h, id)
	if !ok || !st.Running {
		return "not running"
	}
	return fmt.Sprintf("running · %d tools", st.Tools)
}

func stateOf(h *dun.Harness, id string) (dun.ServerState, bool) {
	for _, st := range h.Servers() {
		if st.ID == id {
			return st, true
		}
	}
	return dun.ServerState{}, false
}

// serverHint is the startup line naming what is NOT running and how to get it.
// Shown once, at ready: an agent that silently lacks code navigation is the
// kind of thing a user only discovers by watching it flail with grep.
func serverHint(states []dun.ServerState) string {
	var lines []string
	for _, st := range states {
		if st.Running {
			continue
		}
		alias := aliasOf(st.ID)
		_, hasCmd := serverAliases[alias]
		switch {
		case st.Err != "":
			lines = append(lines, fmt.Sprintf("%s did not start: %s", st.ID, oneLine(st.Err)))
		case hasCmd:
			lines = append(lines, fmt.Sprintf("%s off — /%s on to start it, /%s auto to start it every session", st.ID, alias, alias))
		default:
			lines = append(lines, st.ID+" off")
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// serversToAny renders server states for a JSON event.
func serversToAny(states []dun.ServerState) []map[string]any {
	out := make([]map[string]any, 0, len(states))
	for _, st := range states {
		m := map[string]any{
			"id": st.ID, "running": st.Running, "auto": st.Auto, "tools": st.Tools,
		}
		if st.Err != "" {
			m["err"] = st.Err
		}
		out = append(out, m)
	}
	return out
}
