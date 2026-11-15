package tui

import (
	"strings"
	"testing"
)

// mcp_login says how a login went: started (the URL), done, failed.
func TestMcpLogin_Phases(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = resized(m, 120, 24)
	m = m.feed(evMsg{"type": "mcp_login", "phase": "started", "server": "gh", "url": "https://x.dev/login"})
	m = m.feed(evMsg{"type": "mcp_login", "phase": "done", "server": "gh"})
	m = m.feed(evMsg{"type": "mcp_login", "phase": "failed", "server": "db", "error": "access_denied"})
	m = m.feed(evMsg{"type": "mcp_login", "server": "old", "url": "https://y.dev/login"}) // no phase: a start
	txt := stripANSI(convoText(m))
	for _, want := range []string{"open to log in to gh: https://x.dev/login", "gh: logged in",
		"db: login failed — access_denied", "open to log in to old: https://y.dev/login"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
}
