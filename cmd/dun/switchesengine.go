package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/proto"
)

// switchesEvent answers the control command "switches": "<kind>" is the
// kind's view; "<kind> on|off|unset <name or glob> [--scope]" and "<kind>
// mode <mode|unset> [--scope]" change it first (session scope when none is
// given, as /mcp's switches) and say what they did.
func switchesEvent(h *harness.Harness, action string) *proto.Switches {
	f := strings.Fields(action)
	if len(f) == 0 {
		return &proto.Switches{Message: "switches: which kind? (skills)"}
	}
	kind, msg := f[0], ""
	if len(f) > 1 {
		var err error
		msg, err = switchOp(h, kind, f[1:])
		if err != nil {
			msg = kind + ": " + err.Error()
		}
	}
	v, ok := h.SwitchView(kind)
	if !ok {
		return &proto.Switches{Kind: kind, Message: "no such kind of switches: " + kind}
	}
	ev := &proto.Switches{Kind: v.Kind, Items: []proto.SwitchItem{}, Mode: v.Mode, ModeFrom: v.ModeFrom,
		Modes: v.Modes, About: v.About, Order: v.Order, Note: v.Note, Message: msg, Slash: v.Slash, Status: v.Status, Search: v.Search}
	for _, it := range v.Items {
		ev.Items = append(ev.Items, proto.SwitchItem{Name: it.Name, Description: it.Description, Source: it.Source,
			Detail: it.Detail, Tokens: it.Tokens, On: it.On, From: it.From,
			Global: it.Global, Project: it.Project, Session: it.Session, Parent: it.Parent})
	}
	return ev
}

// switchOp runs one operation on a kind.
func switchOp(h *harness.Harness, kind string, f []string) (string, error) {
	verb, rest := f[0], f[1:]
	if kind == "lsp" && (verb == "add" || verb == "remove") {
		return h.LSPEdit(context.Background(), verb, rest)
	}
	if verb == "mode" {
		if len(rest) == 0 {
			return "", errUsage(kind, "mode <mode|unset> [--global|--project|--session]")
		}
		scope, err := scopeArg(rest[1:], mcpserver.ScopeSession)
		if err != nil {
			return "", err
		}
		mode := rest[0]
		if mode == "unset" {
			mode = ""
		}
		return h.SetSwitchMode(kind, mode, scope)
	}
	if verb != "on" && verb != "off" && verb != "unset" || len(rest) == 0 {
		return "", errUsage(kind, "on|off|unset <name or glob> [--global|--project|--session] · mode <mode|unset> [--scope]")
	}
	scope, err := scopeArg(rest[1:], mcpserver.ScopeSession)
	if err != nil {
		return "", err
	}
	var on *bool
	if verb != "unset" {
		v := verb == "on"
		on = &v
	}
	return h.SetSwitch(kind, rest[0], scope, on)
}

type usageErr string

func (u usageErr) Error() string { return string(u) }

func errUsage(kind, usage string) error {
	slash := strings.TrimSuffix(kind, "s")
	if kind == "rules" {
		slash = "rules"
	}
	return usageErr("usage: /" + slash + " " + usage)
}

// rulesCmd is /rules' engine side, for every client: "" the files, their
// cost and the mode; a bare mode word sets the project's mode (as /rules
// always did) or, with a scope, that scope's; add|remove <path> [--scope]
// makes a file a root file or takes it back; the rest are the switches'.
func rulesCmd(ctx context.Context, h *harness.Harness, action string) (string, error) {
	f := strings.Fields(action)
	switch {
	case len(f) == 0:
		return h.RulesStatus(), nil
	case f[0] == harness.RulesFull || f[0] == harness.RulesSummary || f[0] == harness.RulesIndex:
		if len(f) == 1 {
			return h.SetRulesMode(ctx, f[0])
		}
		return switchOp(h, "rules", append([]string{"mode"}, f...))
	case f[0] == "add" || f[0] == "remove":
		if len(f) < 2 {
			return "", errUsage("rules", f[0]+" <path> [--global|--project|--session]")
		}
		scope, err := scopeArg(f[2:], mcpserver.ScopeSession)
		if err != nil {
			return "", err
		}
		if f[0] == "add" {
			return h.AddRule(f[1], scope)
		}
		return h.RemoveRule(f[1], scope)
	}
	return switchOp(h, "rules", f)
}

// skillFileEvent answers the control command "skillfile": action is JSON
// {"skill", "file"}; the file is read by the engine, confined to the
// skill's folder (harness.Harness.SkillFile), so a client attached through dund reads
// the engine's disk, not its own.
func skillFileEvent(ctx context.Context, h *harness.Harness, action string) *proto.SkillFile {
	var req struct{ Skill, File string }
	if err := json.Unmarshal([]byte(action), &req); err != nil {
		return &proto.SkillFile{Error: "skillfile: the action is JSON {skill, file}: " + err.Error()}
	}
	v, err := h.SkillFile(ctx, req.Skill, req.File)
	ev := &proto.SkillFile{Skill: req.Skill, File: v.File, Name: v.Name, Description: v.Description, Text: v.Text,
		Clipped: v.Clipped, Files: v.Files, Includes: v.Includes}
	if err != nil {
		ev.File, ev.Error = req.File, err.Error()
		if ev.File == "" {
			ev.File = "SKILL.md"
		}
	}
	return ev
}
