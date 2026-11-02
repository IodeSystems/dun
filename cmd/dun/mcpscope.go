package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/mcptable"
	"github.com/iodesystems/dun/internal/textfmt"
)

// The server switches as commands and as a table (dun mcpscope.go has the
// model): on/off at global, project or session scope, unset at the last two,
// and the mode at each — standalone (dun connects to it; "direct" in the
// files) or scripted (hosted in mcpshell, called through eval; "shell").
//
//	/mcp on|off <id> [--global|--project|--session]   bare: this session
//	/mcp unset <id> --project|--session
//	/mcp mode <id> standalone|scripted|unset [--scope]
//	                                   via, move and shell|direct are synonyms
//	/mcp toggle <id> <tool> on|off|unset [--scope]

// scopeArg reads the scope flags after a command; def when there are none.
func scopeArg(flags []string, def string) (string, error) {
	scope := ""
	for _, f := range flags {
		s, ok := dun.ParseScope(f)
		if !ok || !strings.HasPrefix(f, "-") {
			return "", fmt.Errorf("%q: want --global, --project or --session", f)
		}
		if scope != "" && scope != s {
			return "", fmt.Errorf("one scope at a time")
		}
		scope = s
	}
	if scope == "" {
		scope = def
	}
	return scope, nil
}

// runSwitchCmd sets one server's on/off switch: verb on, off or unset.
func runSwitchCmd(ctx context.Context, h *dun.Harness, alias, verb string, flags []string) string {
	id, label := alias, alias
	def := dun.ScopeSession
	if verb == "unset" {
		def = ""
	}
	scope, err := scopeArg(flags, def)
	if err != nil {
		return label + ": " + err.Error()
	}
	var on *bool
	switch verb {
	case "on", "off":
		b := verb == "on"
		on = &b
	case "unset":
		if scope == "" || scope == dun.ScopeGlobal {
			return "usage: /mcp unset <id> --project|--session (the global switch is on or off)"
		}
	default:
		return "unknown switch " + verb + " — on, off or unset"
	}
	t0 := time.Now()
	path, err := h.SetServerOn(ctx, id, scope, on)
	st, _ := stateOf(h, id)
	if path == "" && err != nil {
		return label + ": " + textfmt.OneLine(err.Error())
	}
	msg := label + ": " + scope + " " + verb
	if path != "" {
		msg += " (saved to " + path + ")"
	}
	msg += " → " + mcptable.StateCell(st) + " · " + mcptable.ServerDetail(st)
	if elapsed := time.Since(t0); elapsed > time.Second {
		msg += fmt.Sprintf(" · %s", elapsed.Round(time.Second))
	}
	if err != nil {
		msg += "\ndid not start — " + textfmt.OneLine(err.Error()) + "\ncarrying on without it; fix and /mcp on " + id + " again"
	}
	// Said, because the switch the user just set is not the one that decides.
	if st.From != scope && st.From != "" {
		msg += "\nthe " + fromWord(st.From) + " switch decides here"
	}
	if verb == "unset" && scope == dun.ScopeProject && st.Project != "" {
		msg += "\nthe repo's dun.json sets it for this project — that is the project value while the project's state is unset"
	}
	return msg + ingestLine(h, id)
}

// runViaCmd sets a server's mode: mode <id> standalone|scripted|unset [--scope].
func runViaCmd(ctx context.Context, h *dun.Harness, f []string) string {
	const usage = "usage: /mcp mode <id> standalone|scripted|unset [--global|--project|--session] (bare: this session)"
	if len(f) < 2 {
		return usage
	}
	id, via := f[0], strings.ToLower(f[1])
	scope, err := scopeArg(f[2:], dun.ScopeSession)
	if err != nil {
		return "mcp mode: " + err.Error()
	}
	if via == "unset" {
		via = ""
	} else if v, err := dun.NormalizeVia(via); err != nil {
		return usage
	} else {
		via = map[string]string{"": dun.ViaDirect, dun.ViaShell: dun.ViaShell}[v]
	}
	restarted, err := h.MoveServer(ctx, id, scope, via)
	if err != nil {
		return "mcp mode: " + err.Error()
	}
	st, _ := stateOf(h, id)
	word := "unset"
	if via != "" {
		word = dun.ModeWord(via)
	}
	msg := id + ": mode " + scope + " " + word + " → " + mcptable.ViaCell(st) + " · " + mcptable.ServerDetail(st)
	if restarted {
		msg += " · shell restarted"
	}
	if st.Via == dun.ViaShell {
		if sh, _ := stateOf(h, dun.ServerShell); !sh.Running && st.Auto {
			msg += "\nthe shell is not running — it hosts " + id + " when it starts (/mcp on shell)"
		}
		msg += "\n" + shellPlacementNote
	}
	return msg
}

// toolSwitchLine sets one tool's switch and says what happened.
func toolSwitchLine(h *dun.Harness, id, tool, verb string, flags []string) string {
	scope, err := scopeArg(flags, dun.ScopeSession)
	if err != nil {
		return id + ": " + err.Error()
	}
	var on *bool
	switch verb {
	case "on", "off":
		b := verb == "on"
		on = &b
	case "unset":
	default:
		return "usage: /mcp toggle <id> <tool> on|off|unset [--global|--project|--session]"
	}
	path, changed, err := h.SetToolOn(id, tool, scope, on)
	switch {
	case err != nil:
		return id + ": " + err.Error()
	case !changed:
		return id + ": " + tool + " is already " + verb + " at " + scope + " scope"
	}
	msg := id + ": " + tool + " " + verb + " at " + scope + " scope"
	if path != "" {
		msg += " (saved to " + path + ")"
	}
	return msg + "; the model sees it from the next turn"
}

// fromWord names a deciding scope.
func fromWord(from string) string {
	if from == "flag" {
		return "--rag/--lsp flag (this run)"
	}
	return from
}
