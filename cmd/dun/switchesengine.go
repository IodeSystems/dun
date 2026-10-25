package main

import (
	"strings"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// switchesEvent answers the control command "switches": "<kind>" is the
// kind's view; "<kind> on|off|unset <name or glob> [--scope]" and "<kind>
// mode <mode|unset> [--scope]" change it first (session scope when none is
// given, as /mcp's switches) and say what they did.
func switchesEvent(h *dun.Harness, action string) *proto.Switches {
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
		} else {
			h.Aside("[control/" + kind + "] " + msg)
		}
	}
	v, ok := h.SwitchView(kind)
	if !ok {
		return &proto.Switches{Kind: kind, Message: "no such kind of switches: " + kind}
	}
	ev := &proto.Switches{Kind: v.Kind, Items: []proto.SwitchItem{}, Mode: v.Mode, ModeFrom: v.ModeFrom,
		Modes: v.Modes, About: v.About, Order: v.Order, Note: v.Note, Message: msg}
	for _, it := range v.Items {
		ev.Items = append(ev.Items, proto.SwitchItem{Name: it.Name, Description: it.Description, Source: it.Source,
			Detail: it.Detail, Tokens: it.Tokens, On: it.On, From: it.From,
			Global: it.Global, Project: it.Project, Session: it.Session})
	}
	return ev
}

// switchOp runs one operation on a kind.
func switchOp(h *dun.Harness, kind string, f []string) (string, error) {
	verb, rest := f[0], f[1:]
	if verb == "mode" {
		if len(rest) == 0 {
			return "", errUsage(kind, "mode <mode|unset> [--global|--project|--session]")
		}
		scope, err := scopeArg(rest[1:], dun.ScopeSession)
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
	scope, err := scopeArg(rest[1:], dun.ScopeSession)
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
	return usageErr("usage: /" + strings.TrimSuffix(kind, "s") + " " + usage)
}
