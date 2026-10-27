package dun

import (
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// A notice that is JSON (mcpshell's hosted sizes) goes to the server's
// activity log, not to the conversation; prose still reaches the model.
func TestServerNotice_JSONToTheLog(t *testing.T) {
	h := &Harness{}
	var told []string
	h.cfg.OnNotify = func(s string) { told = append(told, s) }
	h.noteServerNotification(mcpmgr.Notification{ServerID: "shell", Method: "notifications/message",
		Params: map[string]any{"level": "info", "logger": "mcpshell/hosted", "data": `{"servers":[{"ns":"vay","tools":112}]}`}})
	if len(told) != 0 {
		t.Errorf("a JSON notice reached the conversation: %v", told)
	}
	if log := h.serverLogTail("shell", 5); len(log) != 1 || !strings.Contains(log[0].Text, `notice: {"servers"`) {
		t.Errorf("not in the log: %+v", log)
	}
}

// /rephrase prompt: dun's until one is saved; saved, it leads the request;
// default goes back.
func TestRephrasePrompt(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if p, own := RephrasePrompt(); own || p != rephraseInstruction {
		t.Fatal("not dun's by default")
	}
	if err := SetRephrasePrompt("Make it terse."); err != nil {
		t.Fatal(err)
	}
	if p, own := RephrasePrompt(); !own || !strings.HasPrefix(p, "Make it terse.") || !strings.HasSuffix(p, "User's message:\n") {
		t.Errorf("saved: %q", p)
	}
	if err := SetRephrasePrompt(""); err != nil {
		t.Fatal(err)
	}
	if _, own := RephrasePrompt(); own {
		t.Error("default did not go back")
	}
}
