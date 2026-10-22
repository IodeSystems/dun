package main

import (
	"slices"
	"testing"

	"github.com/iodesystems/dun"
)

// Every TUI slash command the engine carries out is in the list other
// clients get — so a command added to the TUI cannot silently be missing on
// the web.
func TestCommandListCoversTheTUIsEngineCommands(t *testing.T) {
	engineRouted := []string{"rag", "lsp", "mcp", "docker", "worktree", "ship", "memories", "remember", "suggest", "rephrase", "clear", "close"}
	var tui []string
	for _, c := range (tuiModel{}).allSlash() {
		tui = append(tui, c.name)
	}
	var listed []string
	for _, c := range commandList(nil, false).Items {
		listed = append(listed, c.Name)
		if !slices.Contains(tui, c.Name) {
			t.Errorf("/%s is listed for clients but the TUI has no such command", c.Name)
		}
	}
	for _, n := range engineRouted {
		if !slices.Contains(listed, n) {
			t.Errorf("/%s is engine-routed in the TUI but missing from the list", n)
		}
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
	for _, line := range []string{"rephrase", "rephrase status", "nope", "mcp add", "mcp remove"} {
		calls = nil
		routeSlash(line, r)
		if len(calls) != 1 || calls[0].kind != "reply" {
			t.Errorf("%q should only reply, got %v", line, calls)
		}
	}
}
