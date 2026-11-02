package main

import (
	"strings"
	"testing"
)

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
