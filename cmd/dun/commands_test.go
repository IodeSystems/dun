package main

import (
	"strings"
	"testing"
	"time"

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
		"rag on":                               {"server", "rag", "on"},
		"/lsp":                                 {"server", "lsp", ""},
		"mcp refresh docs":                     {"control", "mcp", "refresh docs"},
		"mcp login snow":                       {"control", "mcp", "login snow"},
		"mcp LOGOUT snow":                      {"control", "mcp", "logout snow"},
		"mcp move corp SHELL":                  {"control", "mcp", "via corp shell"},
		"mcp move corp direct":                 {"control", "mcp", "via corp direct"},
		"mcp via corp shell --project":         {"control", "mcp", "via corp shell --project"},
		"mcp on corp --global":                 {"server", "corp", "on --global"},
		"mcp off corp":                         {"server", "corp", "off"},
		"mcp unset corp --session":             {"server", "corp", "unset --session"},
		"mcp toggle corp Tool_X off --project": {"control", "mcptools", "toggle corp Tool_X off --project"},
		"rag on --project":                     {"server", "rag", "on --project"},
		"mcp tools el":                         {"control", "mcptools", "el"},
		"mcp tools":                            {"control", "mcptools", ""},
		"mcp restart":                          {"server", allServers, "restart"},
		"mcp off github":                       {"server", "github", "off"},
		"docker OFF":                           {"control", "docker", "off"},
		"worktree new":                         {"control", "worktree", "new"},
		"remember Tests Are Slow":              {"control", "remember", "Tests Are Slow"},
		"memories":                             {"control", "memories", ""},
		"clear":                                {"reset", "", ""},
		"close":                                {"control", "close", ""},
		"interrupt":                            {"control", "interrupt", ""},
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
	for _, line := range []string{"rephrase", "rephrase status", "nope", "mcp add", "mcp remove", "mcp login", "mcp move corp", "mcp move corp sideways", "mcp unset corp", "mcp toggle corp x", "mcp on --global"} {
		calls = nil
		routeSlash(line, r)
		if len(calls) != 1 || calls[0].kind != "reply" {
			t.Errorf("%q should only reply, got %v", line, calls)
		}
	}
}

func TestRouteSlash_Jobs(t *testing.T) {
	type call struct{ kind, a, b string }
	var calls []call
	r := slashRoutes{
		server:  func(a, b string) { calls = append(calls, call{"server", a, b}) },
		control: func(a, b string) { calls = append(calls, call{"control", a, b}) },
		reset:   func() { calls = append(calls, call{"reset", "", ""}) },
		reply:   func(m string) { calls = append(calls, call{"reply", m, ""}) },
	}
	for line, want := range map[string]call{
		"jobs":          {"control", "jobs", ""},
		"/jobs list":    {"control", "jobs", ""},
		"jobs kill 3":   {"control", "jobs", "kill 3"},
		"jobs kill #3":  {"control", "jobs", "kill 3"},
		"jobs KILL ALL": {"control", "jobs", "kill all"},
	} {
		calls = nil
		routeSlash(line, r)
		if len(calls) != 1 || calls[0] != want {
			t.Errorf("%q: %v, want %v", line, calls, want)
		}
	}
	for _, line := range []string{"jobs kill", "jobs kill x", "jobs kill 0", "jobs stop 3", "jobs kill 3 4"} {
		calls = nil
		routeSlash(line, r)
		if len(calls) != 1 || calls[0].kind != "reply" {
			t.Errorf("%q should only reply with the usage, got %v", line, calls)
		}
	}
}

func TestJobsListing(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	if got := jobsListing(nil, now); got != "no background jobs are running" {
		t.Errorf("empty: %q", got)
	}
	got := jobsListing([]dun.JobInfo{
		{ID: 3, Command: "go test ./...", Started: now.Unix() - 125, Last: "ok  pkg/a 1.2s"},
		{ID: 5, Command: "sleep 30", Started: now.Unix() - 7},
	}, now)
	for _, want := range []string{"2 running", "#3 go test ./... (2m) ⟩ ok  pkg/a 1.2s", "#5 sleep 30 (7s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing lacks %q:\n%s", want, got)
		}
	}
}
