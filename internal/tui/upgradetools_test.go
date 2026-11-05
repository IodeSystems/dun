package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// /upgrade lists each tool against its pin; /upgrade now installs what is
// behind and, with a tool updated, restarts dund into this build so the
// engines take the new tools.
func TestUpgradeSlash_Tools(t *testing.T) {
	old, oldAsk := runTools, askDundUpgrade
	defer func() { runTools, askDundUpgrade = old, oldAsk }()
	var asked []bool
	runTools = func(upgrade bool) ([]toolState, error) {
		asked = append(asked, upgrade)
		ms := toolState{Name: "mcpshell", Version: "v0.7.1", Origin: "release v0.7.1", Target: "v0.8.1",
			Note: "an update is available: v0.8.1 (tested with this dun)", Fix: "dun doctor --fix", Can: true}
		if upgrade {
			ms.Updated, ms.Version = true, "v0.8.1"
		}
		return []toolState{ms, {Name: "raglit", Missing: true}}, nil
	}
	restarted := false
	askDundUpgrade = func(string, string) bool { restarted = true; return true }

	rec := &recWC{}
	m := newTUIModel(&dunProc{stdin: rec}, "/ws")
	last := func() string { return ansi.Strip(m.convo[len(m.convo)-1].view()) }

	cmd := upgradeSlash(&m, nil)
	m.toolsDone(cmd().(toolsMsg))
	if got := last(); !strings.Contains(got, "mcpshell: release v0.7.1; an update is available: v0.8.1") ||
		!strings.Contains(got, "/upgrade now installs it") || !strings.Contains(got, "raglit: not installed") {
		t.Errorf("bare /upgrade:\n%s", got)
	}
	if restarted {
		t.Error("bare /upgrade restarted dund")
	}

	cmd = upgradeSlash(&m, []string{"now"})
	follow := m.toolsDone(cmd().(toolsMsg))
	if got := last(); !strings.Contains(ansi.Strip(m.convo[len(m.convo)-2].view()), "mcpshell: updated to v0.8.1") || !strings.Contains(got, "restart") {
		t.Errorf("/upgrade now:\n%s", got)
	}
	if follow == nil {
		t.Fatal("/upgrade now with a tool updated did not restart dund")
	}
	if msg, _ := follow().(upgradeSelfMsg); msg.err != nil || !restarted {
		t.Errorf("restart: %v (asked %v)", msg.err, restarted)
	}
	if len(asked) != 2 || asked[0] || !asked[1] {
		t.Errorf("runTools calls: %v", asked)
	}
}
