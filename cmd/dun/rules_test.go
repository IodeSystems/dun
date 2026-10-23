package main

import "testing"

func TestRouteSlash_Rules(t *testing.T) {
	type call struct{ kind, a, b string }
	var calls []call
	r := slashRoutes{
		server:  func(a, b string) { calls = append(calls, call{"server", a, b}) },
		control: func(a, b string) { calls = append(calls, call{"control", a, b}) },
		reset:   func() { calls = append(calls, call{"reset", "", ""}) },
		reply:   func(m string) { calls = append(calls, call{"reply", m, ""}) },
	}
	for line, want := range map[string]call{
		"rules":          {"control", "rules", ""},
		"/rules SUMMARY": {"control", "rules", "summary"},
		"rules index":    {"control", "rules", "index"},
		"rules full":     {"control", "rules", "full"},
	} {
		calls = nil
		routeSlash(line, r)
		if len(calls) != 1 || calls[0] != want {
			t.Errorf("%q: %v, want %v", line, calls, want)
		}
	}
	for _, line := range []string{"rules half", "rules full now"} {
		calls = nil
		routeSlash(line, r)
		if len(calls) != 1 || calls[0].kind != "reply" {
			t.Errorf("%q should only reply with the usage, got %v", line, calls)
		}
	}
}
