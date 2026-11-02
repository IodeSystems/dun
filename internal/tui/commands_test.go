package tui

import (
	"testing"

	"github.com/iodesystems/dun/internal/enginecmd"
)

// The TUI has no local copy of an engine command — so none can drift from
// what the other clients run — and it shows and completes the engine's list
// once the commands event arrives.
func TestTUIDoesNotShadowEngineCommands(t *testing.T) {
	local := map[string]bool{}
	for _, c := range SlashCommands {
		local[c.Name] = true
	}
	for _, c := range enginecmd.CommandList(nil, "").Items {
		if local[c.Name] {
			t.Errorf("/%s is both a local TUI command and an engine command", c.Name)
		}
	}
	m := tuiModel{}
	m.engineCmds = engineCmdsFromEvent(evMsg{"items": []any{map[string]any{"name": "raglit", "usage": "[on|off]", "description": "docs"}}})
	found := false
	for _, c := range m.allSlash() {
		if c.Name == "raglit" && c.args == "[on|off]" {
			found = true
		}
	}
	if !found {
		t.Error("the engine's commands are not in the TUI's palette")
	}

}
