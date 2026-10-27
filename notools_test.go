package dun

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// --no-tools: the model is offered no tool, the system prompt says so, no
// server is resolved, and a stray call is refused.
func TestNoTools_PlainChat(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if n := len(withoutDirServers(DefaultServers("/ws", ""), Config{NoTools: true})); n != 0 {
		t.Errorf("servers resolved under NoTools: %d", n)
	}
	dir := t.TempDir()
	r := &visionRunner{vision: true}
	h, err := Start(context.Background(), Config{Workspace: dir, Client: r, NoTools: true,
		SessionFile: filepath.Join(dir, "s.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	if got := h.ToolNames(); len(got) != 0 {
		t.Errorf("tool names: %v", got)
	}
	if len(h.Session.Tools) != 0 {
		t.Errorf("tool defs: %d", len(h.Session.Tools))
	}
	if len(h.Servers()) != 0 {
		t.Errorf("servers: %v", h.Servers())
	}
	if !strings.Contains(h.Session.System, "no tools") {
		t.Errorf("system: %q", h.Session.System)
	}
	var tc llm.ToolCall
	tc.Function.Name = "exec"
	if _, err := h.Session.Dispatch(context.Background(), tc); err == nil {
		t.Error("a call was dispatched")
	}
	if _, err := h.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
}
