package dun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// What dun is, three ways: the session tool, the file dun session info
// reads, and the system prompt's line (USER 2026-10-27).
func TestHarnessInfo(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	sf := filepath.Join(t.TempDir(), "20261027-101500.jsonl")
	h := &Harness{cfg: Config{Workspace: ws, SessionFile: sf, Version: "v0.1.3",
		Provider: "openrouter", Endpoint: Endpoint{Model: "deepseek/x"}}}
	h.effort.Store(EffortHigh)
	in := h.HarnessInfo()
	if in.Name != "dun" || in.Icon != "👷" || in.SessionID != "20261027-101500" || in.Version != "v0.1.3" ||
		in.Model != "deepseek/x" || in.Provider != "openrouter" || in.Effort != "high" || in.Workspace != ws || in.Context == nil {
		t.Errorf("info: %+v", in)
	}
	line := h.identityLine()
	// The session tool is not listed; the line says to call it, and carries no
	// session id or version (the same text in every session, for the cache).
	for _, want := range []string{"You are dun (👷)", "Call session() for harness info"} {
		if !strings.Contains(line, want) {
			t.Errorf("identity lacks %q: %s", want, line)
		}
	}
	d := withSession(func(context.Context, llm.ToolCall) (string, error) { return "other", nil }, h, nil)
	var call llm.ToolCall
	call.Function.Name = "session"
	out, _ := d(context.Background(), call)
	var got sessionReply
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Harness.SessionID != "20261027-101500" {
		t.Errorf("tool: %v %s", err, out)
	}
	h.writeHarnessInfo()
	b, err := os.ReadFile(HarnessInfoFile(sf))
	if err != nil || !strings.Contains(string(b), `"session_id": "20261027-101500"`) {
		t.Errorf("file: %v %s", err, b)
	}
}
