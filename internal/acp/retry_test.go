package acp

import (
	"strings"
	"testing"

	"github.com/iodesystems/dun/proto"
)

// A provider wait reaches the editor as a thought, so a resend chain that
// sleeps for minutes does not look like a hang.
func TestRetryShownAsThought(t *testing.T) {
	r := newRig(t)
	sid := r.newSession()
	<-r.d.creates
	r.ed.take()
	ch := r.prompt(sid, "go")
	r.d.next()
	const line = "provider failed (native: error), resending 2/10 in 8s"
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "go"}, &proto.Turn{Kind: "user"},
		&proto.Retry{Scope: "provider", Kind: "provider_fail", Attempt: 2, Text: line},
		&proto.Message{Role: "assistant", Content: "ok"}, &proto.Done{})
	if res := waitPrompt(t, ch); res.err != nil {
		t.Fatalf("prompt: %+v", res)
	}
	var thought []string
	for _, u := range r.ed.take() {
		if u.AgentThoughtChunk != nil {
			thought = append(thought, u.AgentThoughtChunk.Content.Text.Text)
		}
	}
	if got := strings.Join(thought, ""); !strings.Contains(got, line) {
		t.Errorf("thought %q: want the retry line", got)
	}
}
