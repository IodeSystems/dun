package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/dun"
)

// /mcp on|off on a group of dun's own tools works like a server's switch.
func TestMCPSwitch_BuiltinGroup(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h, err := dun.Start(context.Background(), dun.Config{Workspace: t.TempDir(), SessionFile: filepath.Join(t.TempDir(), "s.jsonl"), Servers: []dun.Server{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, action := range []string{"off --session", "off"} {
		msg := runServerCmd(context.Background(), h, "dun:recap", action)
		if !strings.Contains(msg, "dun:recap") || strings.Contains(msg, "no server") || slices.Contains(h.ToolNames(), "recap") {
			t.Errorf("%q: %s · tools %v", action, msg, h.ToolNames())
		}
		runServerCmd(context.Background(), h, "dun:recap", "unset --session")
	}
	if msg := runServerCmd(context.Background(), h, "dun:core", "off --session"); !strings.Contains(msg, "always on") {
		t.Errorf("core off: %s", msg)
	}
}
