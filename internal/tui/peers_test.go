package tui

import (
	"strings"
	"testing"
)

func TestDunMsgRender(t *testing.T) {
	pv, _ := renderToolResult(renderCtx{tool: "dun_msg", args: map[string]any{"to": "raglit:230000", "text": "is it rebuilt?"},
		result: "delivered to raglit:230000 (20261021-230000): it is busy and reads it in its current turn."})
	if !strings.Contains(pv, "raglit:230000") || !strings.Contains(pv, "is it rebuilt?") {
		t.Fatalf("preview %q", pv)
	}
	pv, _ = renderToolResult(renderCtx{tool: "dun_msg", args: map[string]any{"to": "x", "text": "y"},
		result: "ERROR: rate limit: at most 6 messages a minute to x"})
	if !strings.Contains(pv, "rate limit") {
		t.Fatalf("refusal preview %q", pv)
	}
}
