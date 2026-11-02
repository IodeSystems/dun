package dun

import (
	"testing"
	"time"

	"github.com/iodesystems/agentkit/mcpmgr"
)

func wakeHarness(wake bool) *Harness {
	return &Harness{store: &sessionStore{}, wake: make(chan struct{}, 4),
		specs: []Server{{ID: "para", Command: "para", Wake: wake}}}
}

func woke(h *Harness) bool {
	select {
	case <-h.wake:
		return true
	case <-time.After(300 * time.Millisecond):
		return false
	}
}

func say(h *Harness, params map[string]any) {
	h.noteServerNotification(mcpmgr.Notification{ServerID: "para", Method: "notifications/message", Params: params})
}

// A server set to wake starts a turn; one not set to queues, as ever.
func TestServerWake(t *testing.T) {
	h := wakeHarness(true)
	say(h, map[string]any{"data": "proposal p1 was decided: call para_read"})
	if !woke(h) {
		t.Error("a wake server did not wake the session")
	}
	h = wakeHarness(false)
	say(h, map[string]any{"data": "proposal p1 was decided"})
	if woke(h) {
		t.Error("a server not set to wake woke the session")
	}
	if h.Queued() != 1 {
		t.Errorf("the message should still queue: %d", h.Queued())
	}
}

// Bookkeeping never wakes, even on a wake server.
func TestServerWake_NotForJSON(t *testing.T) {
	h := wakeHarness(true)
	say(h, map[string]any{"data": `{"hosted":1}`})
	if woke(h) {
		t.Error("a JSON notice woke the session")
	}
}

func TestServerWake_Limits(t *testing.T) {
	var w serverWake
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if ok, _ := w.allow(now); !ok {
		t.Fatal("the first wake was refused")
	}
	if ok, after := w.allow(now.Add(10 * time.Second)); ok || after != 20*time.Second {
		t.Errorf("within the gap: %v %v", ok, after)
	}
	for i := 1; i < serverWakeMax; i++ {
		if ok, _ := w.allow(now.Add(time.Duration(i) * serverWakeGap)); !ok {
			t.Fatalf("wake %d refused", i)
		}
	}
	end := now.Add(serverWakeMax * serverWakeGap)
	if ok, after := w.allow(end); ok || after != 0 {
		t.Errorf("over the hour's limit: %v %v", ok, after)
	}
	if ok, _ := w.allow(now.Add(time.Hour)); !ok {
		t.Error("a new hour did not reset the count")
	}
}

func TestServerWake_SpecMerges(t *testing.T) {
	if !mergeSpec(ServerSpec{ID: "a", Wake: true}, ServerSpec{ID: "a", Command: "x"}).Wake {
		t.Error("omitted wake should inherit")
	}
}
