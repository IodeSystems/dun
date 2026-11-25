package harness

import (
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
)

// agentkit's provider-fail resend becomes a retry note whose one-liner names
// the native finish reason, the attempt and the wait.
func TestProviderFailNote(t *testing.T) {
	n := providerFailNote(agent.ProviderFailRetry{Attempt: 2, Max: 10, FinishReason: "stop",
		NativeFinishReason: "error", Wait: 7600 * time.Millisecond})
	if want := "provider failed (native: error), resending 2/10 in 8s"; n.String() != want {
		t.Errorf("String() = %q, want %q", n.String(), want)
	}
	if !n.Retrying() || n.Attempt != 2 || n.Delay != 7600*time.Millisecond {
		t.Errorf("note %+v", n)
	}
	// No native reason: fall back to the normalized one.
	n = providerFailNote(agent.ProviderFailRetry{Attempt: 1, Max: 3, FinishReason: "stop", Wait: time.Second})
	if !strings.Contains(n.String(), "(native: stop)") {
		t.Errorf("String() = %q", n.String())
	}
}
