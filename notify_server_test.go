package dun

import (
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// An unsolicited server notification becomes a conversation notification, so
// the MODEL learns of it. A merge conflict appearing under a running agent is
// the motivating case: from that moment every symbol in the file may combine
// both sides, and the agent would otherwise find out only if it asked again.
func TestServerNotificationBecomesAConversationNotification(t *testing.T) {
	h := &Harness{store: &sessionStore{}, wake: make(chan struct{}, 4)}

	h.noteServerNotification(mcpmgr.Notification{
		ServerID: "code",
		Method:   "notifications/message",
		Params: map[string]any{
			"level": "warning",
			"data":  "UNRESOLVED merge conflict appeared in w.go",
		},
	})

	h.noteMu.Lock()
	q := append([]queued(nil), h.queue...)
	h.noteMu.Unlock()

	if len(q) != 1 {
		t.Fatalf("want one queued notification; got %d", len(q))
	}
	if q[0].kind != queuedNotification {
		t.Errorf("it must reach the model as a notification, not a user turn")
	}
	// The server is named: several run at once, and "a conflict appeared" is
	// a different fact depending on which workspace said it.
	if !strings.HasPrefix(q[0].text, "[code] ") {
		t.Errorf("the notification should name its server; got %q", q[0].text)
	}
	if !strings.Contains(q[0].text, "merge conflict") {
		t.Errorf("the payload should survive; got %q", q[0].text)
	}
}

// Only notifications/message is lifted. The rest are protocol bookkeeping the
// manager already acts on, and forwarding them would train the model to skim
// what it sees here.
func TestOnlyLogMessagesAreLifted(t *testing.T) {
	h := &Harness{store: &sessionStore{}, wake: make(chan struct{}, 4)}

	for _, n := range []mcpmgr.Notification{
		{ServerID: "code", Method: "notifications/tools/list_changed"},
		{ServerID: "code", Method: "notifications/progress", Params: map[string]any{"progress": 1}},
		{ServerID: "code", Method: "notifications/message", Params: map[string]any{"data": "   "}},
		{ServerID: "code", Method: "notifications/message"},
	} {
		h.noteServerNotification(n)
	}
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	if len(h.queue) != 0 {
		t.Errorf("bookkeeping and empty payloads must not reach the model; got %+v", h.queue)
	}
}
