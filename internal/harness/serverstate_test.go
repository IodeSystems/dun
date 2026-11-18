package harness

import "testing"

func TestServerStateWord(t *testing.T) {
	for _, c := range []struct {
		st       ServerState
		run, all string
	}{
		{ServerState{Auto: true, Running: true}, "running", "running"},
		{ServerState{Auto: true, Running: true, Starting: true}, "starting…", "starting…"},
		{ServerState{Starting: true}, "starting…", "starting…"},
		{ServerState{Auto: true}, "stopped", "stopped"},
		{ServerState{Auto: true, Err: "exit 1:\n  boom"}, "stopped · last error: exit 1: boom", "stopped · last error: exit 1: boom"},
		{ServerState{Err: "boom"}, "stopped · last error: boom", "stopped · last error: boom"},
		{ServerState{}, "stopped", "off"},
		{ServerState{Running: true}, "running", "running"},
	} {
		if got := ServerRunWord(c.st); got != c.run {
			t.Errorf("ServerRunWord(%+v) = %q, want %q", c.st, got, c.run)
		}
		if got := ServerStateWord(c.st); got != c.all {
			t.Errorf("ServerStateWord(%+v) = %q, want %q", c.st, got, c.all)
		}
	}
}
