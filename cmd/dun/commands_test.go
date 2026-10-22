package main

import (
	"testing"

	"github.com/iodesystems/dun"
)

// The TUI has no local copy of an engine command — so none can drift from
// what the other clients run — and it shows and completes the engine's list
// once the commands event arrives.
func TestTUIDoesNotShadowEngineCommands(t *testing.T) {
	local := map[string]bool{}
	for _, c := range slashCommands {
		local[c.name] = true
	}
	for _, c := range commandList(nil, false).Items {
		if local[c.Name] {
			t.Errorf("/%s is both a local TUI command and an engine command", c.Name)
		}
	}
	m := tuiModel{}
	m.engineCmds = engineCmdsFromEvent(evMsg{"items": []any{map[string]any{"name": "rag", "usage": "[on|off]", "description": "docs"}}})
	found := false
	for _, c := range m.allSlash() {
		if c.name == "rag" && c.args == "[on|off]" {
			found = true
		}
	}
	if !found {
		t.Error("the engine's commands are not in the TUI's palette")
	}
}

func TestCommandListShowsState(t *testing.T) {
	l := commandList([]dun.ServerState{{ID: dun.ServerDocs, Running: true, Auto: true}}, true)
	got := map[string]string{}
	for _, c := range l.Items {
		got[c.Name] = c.State + "/" + c.Detail
	}
	if got["rag"] != "on/autostart" || got["lsp"] != "off/not configured" || got["docker"] != "on/" {
		t.Fatalf("states: %v", got)
	}
}

func TestRouteSlash(t *testing.T) {
	type call struct{ kind, a, b string }
	var calls []call
	r := slashRoutes{
		server:  func(a, b string) { calls = append(calls, call{"server", a, b}) },
		control: func(a, b string) { calls = append(calls, call{"control", a, b}) },
		reset:   func() { calls = append(calls, call{"reset", "", ""}) },
		reply:   func(m string) { calls = append(calls, call{"reply", m, ""}) },
	}
	for line, want := range map[string]call{
		"rag on":                  {"server", "rag", "on"},
		"/lsp":                    {"server", "lsp", ""},
		"mcp refresh docs":        {"control", "mcp", "refresh docs"},
		"mcp move corp SHELL":     {"control", "mcp", "move corp shell"},
		"mcp move corp direct":    {"control", "mcp", "move corp direct"},
		"mcp restart":             {"server", allServers, "restart"},
		"mcp off github":          {"server", "github", "off"},
		"docker OFF":              {"control", "docker", "off"},
		"worktree new":            {"control", "worktree", "new"},
		"remember Tests Are Slow": {"control", "remember", "Tests Are Slow"},
		"memories":                {"control", "memories", ""},
		"clear":                   {"reset", "", ""},
		"close":                   {"control", "close", ""},
	} {
		calls = nil
		routeSlash(line, r)
		if len(calls) != 1 || calls[0] != want {
			t.Errorf("%q: %v, want %v", line, calls, want)
		}
	}
	// A toggle the engine answers silently is answered here.
	calls = nil
	routeSlash("rephrase on", r)
	if len(calls) != 2 || calls[0] != (call{"control", "rephrase", "on"}) || calls[1] != (call{"reply", "rephrase: on", ""}) {
		t.Errorf("rephrase on: %v", calls)
	}
	// Bare /rephrase reports; it must not turn rephrasing off.
	for _, line := range []string{"rephrase", "rephrase status", "nope", "mcp add", "mcp remove", "mcp move corp", "mcp move corp sideways"} {
		calls = nil
		routeSlash(line, r)
		if len(calls) != 1 || calls[0].kind != "reply" {
			t.Errorf("%q should only reply, got %v", line, calls)
		}
	}
}
