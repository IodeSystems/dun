package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// A request dun canceled is not reported as a provider that cannot be
// reached; a real transport give-up still is.
func TestNoteFromEventCanceled(t *testing.T) {
	err := fmt.Errorf(`Post "https://openrouter.ai/api/v1/chat/completions": %w`, context.Canceled)
	n := noteFromEvent(llm.RetryEvent{Kind: llm.RetryGiveUp, Err: err,
		Reason: "cannot reach the provider and retrying will not help: " + err.Error()})
	if strings.Contains(n.Reason, "cannot reach") || !strings.Contains(n.Reason, "canceled") {
		t.Errorf("canceled: reason %q", n.Reason)
	}
	other := errors.New("dial tcp: no such host")
	n = noteFromEvent(llm.RetryEvent{Kind: llm.RetryGiveUp, Err: other,
		Reason: "cannot reach the provider and retrying will not help: " + other.Error()})
	if !strings.Contains(n.Reason, "cannot reach") {
		t.Errorf("transport: reason %q", n.Reason)
	}
}
