package dun

import (
	"context"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// The notification surface: onNotify fires for KindNotification appends (the
// proactive-RAG pings the FinderPreparer injects) and NOT for ordinary entries.
func TestMemStore_OnNotifyFiresForNotifications(t *testing.T) {
	var got []string
	s := newMemStore()
	s.onNotify = func(text string) { got = append(got, text) }
	ctx := context.Background()

	s.Append(ctx, "x", agent.Entry{Kind: agent.KindUser, Content: "hi"})
	s.Append(ctx, "x", agent.Entry{Kind: agent.KindAssistant, Content: "reply"})
	if len(got) != 0 {
		t.Fatalf("onNotify should not fire for user/assistant: %v", got)
	}

	s.Append(ctx, "x", agent.Entry{Kind: agent.KindNotification, Content: "relevant doc: README"})
	if len(got) != 1 || got[0] != "relevant doc: README" {
		t.Fatalf("onNotify should fire once for a notification: %v", got)
	}
}

// docsFinder returns a finder when raglit's search tool is present, else nil.
func TestDocsFinder_NilWithoutSearch(t *testing.T) {
	if docsFinder(nil, nil) != nil {
		t.Fatal("no tools → no finder")
	}
}
