package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/iodesystems/dun"
)

// infoServers: the shell, a heavy standalone server, a light one, a
// scripted one, one that did not start, and an extension that is off.
func infoServers() []dun.ServerState {
	return []dun.ServerState{
		{ID: "shell", Running: true, Auto: true, Tools: 1},
		{ID: "notes", Running: true, Auto: true, Tools: 12, Via: dun.ViaDirect},
		{ID: "tiny", Running: true, Auto: true, Tools: 2, Via: dun.ViaDirect},
		{ID: "corp", Auto: true, Tools: 30, Via: dun.ViaShell, Hosted: true},
		{ID: "figma", Auto: true, Via: dun.ViaDirect, Err: "connect: connection refused"},
		{ID: "raglit", Slash: "raglit"},
	}
}

func infoParts() []dun.ContextPart {
	return []dun.ContextPart{{Name: "built-in tools", Tokens: 1600},
		{Name: "mcp: shell", Tokens: 1100}, {Name: "mcp: notes", Tokens: 5200}, {Name: "mcp: tiny", Tokens: 300}}
}

// plainInfo is /mcp info with its no-break spaces as spaces.
func plainInfo(in mcpInfoIn) string { return strings.ReplaceAll(mcpInfo(in), " ", " ") }

// /mcp info: a row per server, the total of the window, the shell, the
// hint for the heavy server only, and /mcp help last.
func TestMCPInfo_Rows(t *testing.T) {
	s := plainInfo(mcpInfoIn{servers: infoServers(), parts: infoParts(), counted: true, window: 32000})
	// Moving notes adds ", notes" to the hosted sentence: what it saves is
	// its part less that.
	hosted := []string{"corp"}
	line := estimateTok(dun.HostedLine(append(hosted, "notes"))) - estimateTok(dun.HostedLine(hosted))
	if line <= 0 || line > 3 {
		t.Fatalf("naming one more server costs %d tokens", line)
	}
	save := "  /mcp mode notes scripted — saves ~" + tokN(5200-line) + " tokens\n"
	for _, want := range []string{
		"mcp servers\n",
		"  shell   running · built-in · 1 tool · ~1.1k tokens\n",
		"  notes   running · standalone · 12 tools · ~5.2k tokens\n",
		"  tiny    running · standalone · 2 tools · ~300 tokens\n",
		"  corp    running · scripted · 30 tools · 0 tokens\n",
		"  figma   did not start: connect: connection refused · standalone\n",
		"  raglit  off · built-in\n",
		"  total ~6.6k tokens in every request, 21% of the 32k window\n",
		"the shell's eval: ~1.1k tokens · the line naming its servers: ~" + tokN(estimateTok(dun.HostedLine(hosted))) + " tokens · scripted: corp\n",
		"to save tokens:\n" + save,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "mode tiny") || strings.Contains(s, "mode figma") || strings.Contains(s, "mode shell") {
		t.Errorf("a hint for a light, failed or built-in server:\n%s", s)
	}
	if !strings.HasSuffix(s, "\n/mcp help for more") {
		t.Errorf("last row:\n%s", s)
	}
}

// Nothing heavy, nothing said about saving; no window, no share; before the
// count, no tokens at all.
func TestMCPInfo_Quiet(t *testing.T) {
	in := mcpInfoIn{servers: infoServers(), parts: infoParts()[:2], counted: true, exact: true}
	s := plainInfo(in)
	if strings.Contains(s, "save") || strings.Contains(s, "window") {
		t.Errorf("nothing heavy, no window:\n%s", s)
	}
	if !strings.Contains(s, "  total 1.1k tokens in every request\n") {
		t.Errorf("exact total:\n%s", s)
	}
	in.counted = false
	s = plainInfo(in)
	if !strings.Contains(s, "  tokens: counting…\n") || strings.Contains(s, " tokens\n") {
		t.Errorf("before the count:\n%s", s)
	}
}

// A heavy server with the shell off still gets its hint, and the hint says
// the shell must be on; an oauth server gets none (it cannot be scripted).
func TestMCPInfo_ShellOff(t *testing.T) {
	srv := infoServers()
	srv[0].Running = false
	srv = append(srv, dun.ServerState{ID: "snow", Running: true, Auto: true, Tools: 40, Via: dun.ViaDirect, Auth: "logged in"})
	parts := append(infoParts(), dun.ContextPart{Name: "mcp: snow", Tokens: 9000})
	s := plainInfo(mcpInfoIn{servers: srv, parts: parts, counted: true})
	line := estimateTok(dun.HostedLine([]string{"notes"}))
	for _, want := range []string{
		"  corp    waits for the shell · scripted\n",
		"the shell is off — /mcp on shell · scripted: corp\n",
		"  /mcp mode notes scripted — saves ~" + tokN(5200-line) + " tokens, with the shell on (/mcp on shell)\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "mode snow") {
		t.Errorf("an oauth server hinted:\n%s", s)
	}
}

// The rows in the conversation at a phone's width and a wide one: wrapped
// between words, never cut — every fact is there whole and no row is wider
// than the screen.
func TestMCPInfo_Wraps(t *testing.T) {
	msg := mcpInfo(mcpInfoIn{servers: infoServers(), parts: infoParts(), counted: true, window: 32000})
	for _, w := range []int{49, 100} {
		v := newVtui(w, 60)
		v.event(map[string]any{"type": "control", "id": "mcpinfo", "message": msg})
		view := v.view()
		for _, row := range strings.Split(view, "\n") {
			if ansi.StringWidth(row) > w {
				t.Errorf("%d cols: row %d wide: %q", w, ansi.StringWidth(row), row)
			}
		}
		flat := strings.Join(strings.Fields(view), " ")
		for _, f := range []string{"did not start: connect: connection refused", "21% of the 32k window",
			"/mcp mode notes scripted — saves ~5.2k tokens", "help(\"<ns>\") lists them when the model asks", "/mcp help for more"} {
			if !strings.Contains(flat, f) {
				t.Errorf("%d cols: lacks %q:\n%s", w, f, view)
			}
		}
		// A fact is never split over two rows.
		plain := strings.ReplaceAll(view, "\u00a0", " ")
		for _, f := range []string{"~1.1k tokens", "~5.2k tokens", "12 tools", "~6.6k tokens", "21% of the 32k window",
			"/mcp mode notes scripted", "did not start:", "~29 tokens"} {
			if !strings.Contains(plain, f) {
				t.Errorf("%d cols: %q is split:\n%s", w, f, view)
			}
		}
		t.Logf("%d cols:\n%s", w, plain)
	}
}

// /mcp info goes to the engine; /mcp help is answered with every action.
func TestRouteSlash_MCPInfoHelp(t *testing.T) {
	var got []string
	r := slashRoutes{
		server:  func(a, b string) { got = append(got, "server "+a+" "+b) },
		control: func(a, b string) { got = append(got, "control "+a+" "+b) },
		reset:   func() {},
		reply:   func(m string) { got = append(got, "reply "+m) },
	}
	routeSlash("mcp INFO", r)
	if len(got) != 1 || got[0] != "control mcpinfo " {
		t.Errorf("info: %q", got)
	}
	got = nil
	routeSlash("mcp help", r)
	if len(got) != 1 || !strings.HasPrefix(got[0], "reply /mcp alone") {
		t.Fatalf("help: %q", got)
	}
	for _, a := range []string{"info", "help", "add", "tools", "mode", "toggle", "login", "logout", "remove", "refresh", "restart"} {
		if !strings.Contains(got[0], "\n  "+a+" ") {
			t.Errorf("help lacks %s:\n%s", a, got[0])
		}
	}
}

// The palette offers info and help after /mcp.
func TestPalette_MCPInfoCompletion(t *testing.T) {
	m, _ := skillModel(t, 0)
	m.engineCmds = engineCmdsFromEvent(evMsg{"items": toAny(commandList(nil, false).Items)})
	for q, want := range map[string]string{"/mcp inf": "info", "/mcp hel": "help"} {
		mm := typeStr(m, q)
		if _, l, ok := mm.paletteArgs(); !ok || len(l.choices) == 0 || l.choices[0].value != want {
			t.Errorf("%s: %+v", q, l)
		}
	}
}

// An action no server knows is answered once, with the nearest action, and
// is not sent to every server (each answered with the same usage line).
func TestRouteSlash_MCPUnknownActionOnce(t *testing.T) {
	var got []string
	r := slashRoutes{
		server:  func(a, b string) { got = append(got, "server "+a+" "+b) },
		control: func(a, b string) { got = append(got, "control "+a+" "+b) },
		reset:   func() {},
		reply:   func(m string) { got = append(got, "reply "+m) },
	}
	routeSlash("mcp inf9", r)
	if len(got) != 1 || got[0] != `reply unknown /mcp action "inf9" — did you mean /mcp info? /mcp help lists them` {
		t.Errorf("inf9: %q", got)
	}
	got = nil
	routeSlash("mcp zzzzzz", r)
	if len(got) != 1 || got[0] != `reply unknown /mcp action "zzzzzz" /mcp help lists them` {
		t.Errorf("zzzzzz: %q", got)
	}
	got = nil
	routeSlash("mcp restart", r)
	if len(got) != 1 || !strings.HasPrefix(got[0], "server ") {
		t.Errorf("restart is still a server action: %q", got)
	}
}

// With recorded rounds and a measured speed, /mcp info says what each
// server's schemas cost over the requests that processed them, what a
// scripted server saved (and its help, once), and the uncached worst case.
func TestMCPInfo_Savings(t *testing.T) {
	servers := infoServers()
	servers[3].Standalone, servers[3].Help = 38000, 1600
	usage := dun.StatsSummary{Rounds: 412, Processed: 31, Cost: 0.5, Currency: "USD", CostTokens: 1_000_000,
		Servers: map[string]*dun.ServerUsage{"notes": {Rounds: 412, Processed: 31, Tokens: 161200}, "corp": {SavedRounds: 31, Saved: 1_178_000}}}
	s := plainInfo(mcpInfoIn{servers: servers, parts: infoParts(), counted: true, window: 32000,
		usage: usage, usageDays: 7, speed: 6700, speedRounds: 20})
	for _, want := range []string{
		"running · scripted · 30 tools · 0 tokens (standalone ~38.0k)",
		"what the schemas cost, last 7 days (412 requests, 31 processed the prefix; the rest reused it from the cache):",
		"  notes: its ~5.2k of schemas, processed in 31 requests: ~161.2k tokens · ~24.1s · ~$0.08",
		`  corp: scripted, saves ~38.0k tokens in every request; 31 requests would have processed them: ~1.18M tokens · ~175.8s · ~$0.59 · help("corp") ~1.6k once, when the model asks`,
		"an uncached request processes all ~6.6k tokens of tool schemas: ~985ms at 6.7k/s (measured over 20 requests)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	// No timings: the time is not guessed.
	s = plainInfo(mcpInfoIn{servers: servers, parts: infoParts(), counted: true, usage: usage, usageDays: 7})
	if !strings.Contains(s, "prefill speed: not measured") || strings.Contains(s, "~24.1s") {
		t.Errorf("no speed:\n%s", s)
	}
}
