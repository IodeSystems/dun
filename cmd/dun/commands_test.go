package main

import (
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// The TUI has no local copy of an engine command — so none can drift from
// what the other clients run — and it shows and completes the engine's list
// once the commands event arrives.
func TestTUIDoesNotShadowEngineCommands(t *testing.T) {
	local := map[string]bool{}
	for _, c := range slashCommands {
		local[c.name] = true
	}
	for _, c := range commandList(nil, "").Items {
		if local[c.Name] {
			t.Errorf("/%s is both a local TUI command and an engine command", c.Name)
		}
	}
	m := tuiModel{}
	m.engineCmds = engineCmdsFromEvent(evMsg{"items": []any{map[string]any{"name": "raglit", "usage": "[on|off]", "description": "docs"}}})
	found := false
	for _, c := range m.allSlash() {
		if c.name == "raglit" && c.args == "[on|off]" {
			found = true
		}
	}
	if !found {
		t.Error("the engine's commands are not in the TUI's palette")
	}

	// A user's extension cannot shadow a command either — the engine's or
	// the TUI's own: it loads without its slash command, and says so.
	ext := func(id, slash string) dun.Extension {
		return dun.Extension{ID: id, Source: dun.ExtUser, Slash: &dun.ExtSlash{Name: slash,
			Commands: map[string]dun.ExtSlashCmd{"greet": {ExtStep: dun.ExtStep{Tool: "greet", Args: map[string]any{"name": "{{arg}}"}}}}}}
	}
	exts, refused := engineExtensions(append(dun.BundledExtensions(), ext("a", "mcp"), ext("b", "help"), ext("c", "ext"), ext("hello", "hello")))
	if len(refused) != 3 {
		t.Errorf("refused: %v", refused)
	}
	got := map[string]bool{}
	for _, c := range commandList(nil, "", exts...).Items {
		if got[c.Name] {
			t.Errorf("/%s listed twice", c.Name)
		}
		got[c.Name] = true
	}
	if !got["hello"] || !got["rag"] || !got["lsp"] {
		t.Errorf("extension commands: %v", got)
	}
	for _, c := range commandList(nil, "", exts...).Items {
		if c.Name == "hello" && !strings.Contains(c.Usage, "greet") {
			t.Errorf("hello's usage lacks its command: %q", c.Usage)
		}
	}
	type call struct{ kind, a, b string }
	var calls []call
	r := slashRoutes{exts: exts,
		server:  func(a, b string) { calls = append(calls, call{"server", a, b}) },
		control: func(a, b string) { calls = append(calls, call{"control", a, b}) },
		reply:   func(m string) { calls = append(calls, call{"reply", m, ""}) },
	}
	for line, want := range map[string]call{
		"hello greet World": {"control", "ext", "call hello greet World"},
		"hello on":          {"server", "hello", "on"},
		"mcp on a":          {"server", "a", "on"},
		"ext":               {"control", "ext", ""},
	} {
		calls = nil
		routeSlash(line, r)
		if len(calls) != 1 || calls[0] != want {
			t.Errorf("%q: %v, want %v", line, calls, want)
		}
	}
}

func TestCommandListShowsState(t *testing.T) {
	l := commandList([]dun.ServerState{{ID: dun.ServerDocs, Running: true, Auto: true}}, dun.IsoDocker)
	got := map[string]string{}
	for _, c := range l.Items {
		got[c.Name] = c.State + "/" + c.Detail
	}
	if got["rag"] != "on/autostart" || got["lsp"] != "off/not configured" || got["isolate"] != "docker/" {
		t.Fatalf("states: %v", got)
	}
}

// raglit's commands are in its completion with their help, and in the usage.
func TestCommandList_ExtensionCommandsWithHelp(t *testing.T) {
	var raglit proto.CommandSpec
	for _, c := range commandList(nil, "").Items {
		if c.Name == "rag" {
			raglit = c
		}
	}
	help := map[string]string{}
	for _, c := range raglit.Choices {
		help[c.Value] = c.Description
	}
	for _, n := range []string{"include", "add-document", "exclude", "unexclude", "excludes", "watch", "attach", "detach"} {
		if help[n] == "" {
			t.Errorf("%s: no help in %v", n, help)
		}
		if n != "detach" && !strings.Contains(raglit.Usage, "|"+n) {
			t.Errorf("usage %q lacks %s", raglit.Usage, n)
		}
	}
	if !strings.Contains(help["include"], "WATCHED") || !strings.Contains(help["add-document"], "ONCE") ||
		!strings.Contains(help["exclude"], "no / matches the file name") {
		t.Errorf("include and add-document say how they differ; exclude how a GLOB matches: %v", help)
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
		"rag on":                               {"server", "raglit", "on"},
		"/poly-lsp":                            {"server", "poly-lsp", ""},
		"mcp refresh docs":                     {"control", "mcp", "refresh docs"},
		"mcp login snow":                       {"control", "mcp", "login snow"},
		"mcp LOGOUT snow":                      {"control", "mcp", "logout snow"},
		"mcp move corp SHELL":                  {"control", "mcp", "mode corp shell"},
		"mcp move corp direct":                 {"control", "mcp", "mode corp direct"},
		"mcp via corp shell --project":         {"control", "mcp", "mode corp shell --project"},
		"mcp mode corp scripted --global":      {"control", "mcp", "mode corp scripted --global"},
		"mcp on corp --global":                 {"server", "corp", "on --global"},
		"mcp off corp":                         {"server", "corp", "off"},
		"mcp unset corp --session":             {"server", "corp", "unset --session"},
		"mcp toggle corp Tool_X off --project": {"control", "mcptools", "toggle corp Tool_X off --project"},
		"rag on --project":                     {"server", "raglit", "on --project"},
		"mcp tools el":                         {"control", "mcptools", "el"},
		"mcp tools":                            {"control", "mcptools", ""},
		"mcp restart":                          {"server", allServers, "restart"},
		"mcp off github":                       {"server", "github", "off"},
		"docker OFF":                           {"control", "docker", "off"},
		"worktree new":                         {"control", "worktree", "new"},
		"remember Tests Are Slow":              {"control", "remember", "Tests Are Slow"},
		"memories":                             {"control", "memories", ""},
		"clear":                                {"reset", "", ""},
		"discard":                              {"control", "close", ""},
		"rephrase prompt Be Terse, Please":     {"control", "rephrase", "prompt Be Terse, Please"},
		"rephrase prompt":                      {"control", "rephrase", "prompt"},
		"interrupt":                            {"control", "interrupt", ""},
		"worklog":                              {"control", "worklog", ""},
		"worklog START Fix The Bug":            {"control", "worklog", "start Fix The Bug"},
		"worklog end blocked No Log":           {"control", "worklog", "end blocked No Log"},
		"worklog view 2":                       {"control", "worklog", "view 2"},
		"worklog write 2":                      {"control", "worklog", "write 2"},
		"worklog frob":                         {"reply", "usage: /worklog [start <title> | end [done|dropped|blocked] <why> | view [n] | write [n]]", ""},
		// raglit's index commands keep a path's and a name's case.
		"rag watch ~/My Docs --index Hb": {"control", "ext", "watch raglit ~/My Docs --index Hb"},
		"rag ATTACH Other:Idx --project": {"control", "ext", "attach raglit Other:Idx --project"},
		"rag detach x --global":          {"control", "ext", "detach raglit x --global"},
		"rag attach":                     {"control", "ext", "attach raglit"},
		"rag workspace off --global":     {"control", "ext", "workspace raglit off --global"},
		"poly-lsp attach x":              {"server", "poly-lsp", "attach x"}, // no search role
		// the code index's excludes keep a directory's case.
		"poly-lsp exclude Vendor/X":  {"server", "poly-lsp", "exclude Vendor/X"},
		"poly-lsp INCLUDE .claude/W": {"server", "poly-lsp", "include .claude/W"},
		"poly-lsp budget 48000ms":    {"server", "poly-lsp", "budget 48000ms"},
		// /lsp is the code server's command now; /poly-lsp still routes to it.
		"lsp exclude Vendor/X":                  {"server", "poly-lsp", "exclude Vendor/X"},
		"lsp OFF Go --project":                  {"server", "poly-lsp", "off go --project"},
		"lsp add Zig --ext .zig --cmd /Opt/zls": {"server", "poly-lsp", "add Zig --ext .zig --cmd /Opt/zls"},
		"lsp remove Zig":                        {"server", "poly-lsp", "remove Zig"},
		// raglit's manifest commands keep the text as typed.
		"rag INCLUDE ~/My Docs Notes.MD": {"control", "ext", "call raglit include ~/My Docs Notes.MD"},
		"rag exclude '**/*.LOG' --keep":  {"control", "ext", "call raglit exclude '**/*.LOG' --keep"},
		"rag excludes":                   {"control", "ext", "call raglit excludes "},
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

func TestParseWatchAndAttach(t *testing.T) {
	for in, want := range map[string][2]string{
		"docs":                 {"docs", ""},
		"~/My Docs --index Hb": {"~/My Docs", "Hb"},
		"--index=h /abs/dir":   {"/abs/dir", "h"},
	} {
		dir, idx, err := parseWatch(strings.Fields(in))
		if err != nil || dir != want[0] || idx != want[1] {
			t.Errorf("watch %q: %q %q %v", in, dir, idx, err)
		}
	}
	for _, bad := range []string{"", "--index", "d --bogus"} {
		if _, _, err := parseWatch(strings.Fields(bad)); err == nil {
			t.Errorf("watch %q accepted", bad)
		}
	}
	type att struct {
		index, scope string
		list         bool
	}
	for in, want := range map[string]att{
		"":              {"", dun.ScopeSession, true},
		"--list":        {"", dun.ScopeSession, true},
		"x":             {"x", dun.ScopeSession, false},
		"p:x --project": {"p:x", dun.ScopeProject, false},
		"--global p:x":  {"p:x", dun.ScopeGlobal, false},
		"x --session":   {"x", dun.ScopeSession, false},
	} {
		i, s, l, err := parseAttach(strings.Fields(in))
		if err != nil || (att{i, s, l}) != want {
			t.Errorf("attach %q: %q %q %v %v", in, i, s, l, err)
		}
	}
	for _, bad := range []string{"a b", "x --bogus"} {
		if _, _, _, err := parseAttach(strings.Fields(bad)); err == nil {
			t.Errorf("attach %q accepted", bad)
		}
	}
}
