package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
)

// A provider-fail resend and an empty-reply end are ledger lines with the
// provider's finish words; readers of rounds skip them.
func TestRoundStats_EventsInLedger(t *testing.T) {
	dir := t.TempDir()
	h := &Harness{cfg: Config{SessionFile: "/x/20261124-1.jsonl"}}
	h.rounds.ledger = NewStatsLedger(dir)
	h.noteRoundEvent(RoundEvent{Kind: "provider_fail_retry", Finish: "stop", NativeFinish: "MALFORMED_FUNCTION_CALL", Attempt: 1, Max: 3})
	h.noteEmptyReply(&agent.EmptyReplyError{FinishReason: "stop", NativeFinishReason: "MALFORMED_FUNCTION_CALL"})
	b, err := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"kind":"provider_fail_retry"`) || !strings.Contains(lines[1], `"kind":"empty_reply"`) ||
		!strings.Contains(lines[1], `"native_finish":"MALFORMED_FUNCTION_CALL"`) {
		t.Fatalf("ledger:\n%s", b)
	}
	if s := h.StatsSummary(time.Now().Add(-time.Hour)); s.Rounds != 0 {
		t.Errorf("events counted as rounds: %+v", s)
	}
	if u := h.ModelUsage(time.Now().Add(-time.Hour)); len(u) != 0 {
		t.Errorf("events counted as requests: %+v", u)
	}
}
