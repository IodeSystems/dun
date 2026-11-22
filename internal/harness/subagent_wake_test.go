package harness

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// AB scenario "wakeup on job finish": a child that starts a background job
// during its turn must be woken when it finishes, and the woken turn must
// see the completion notice.
//
// Before the fix (subagent.go wake loop), the child's run ended when its
// turn did; a job that finished after that reached the child only via its
// parent. The loop in subAgent.work now blocks on h.Wake() while a job is
// in flight, and runs h.Continue on completion.
//
// Two wakes fire: one at promotion (timeout exceeded) and one at finish.
// Round 1 starts the job, round 2 is the promotion wake, round 3 is the
// finish wake — the one that must see "job #1".
func TestSubAgent_WokenWhenBackgroundJobFinishes(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()

	var mu sync.Mutex
	var finishMsgs []llm.Message
	child := &scriptRunner{rounds: []roundFn{
		callRound("exec1", "exec", `{"command":"sleep 2","timeout":1}`),
		textRound("promoted"),
		seen(&mu, &finishMsgs, textRound("the job finished")),
	}}

	h, err := Start(context.Background(), Config{
		Workspace:   dir,
		Servers:     []mcpserver.Server{},
		Client:      &scriptRunner{},
		ClientFor:   func(string, string) (ChildClient, error) { return ChildClient{Runner: child}, nil },
		Exec:        &HostShell{Dir: dir},
		SessionFile: filepath.Join(dir, "s.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)

	sa := spawnKid(t, h, "run a job")
	waitIdle(t, sa)

	mu.Lock()
	got := llmText(finishMsgs)
	mu.Unlock()

	if len(finishMsgs) == 0 {
		t.Fatal("the finish-wake turn was never called — the child was not woken by its own job")
	}
	if !strings.Contains(got, "job #1") {
		t.Errorf("the finish-wake turn must see the job completion notice; got: %q", got)
	}
	if n := child.calls(); n != 3 {
		t.Errorf("start + promotion wake + finish wake: want 3 rounds, got %d", n)
	}
}
