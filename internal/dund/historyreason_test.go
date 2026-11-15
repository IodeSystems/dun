package dund

import (
	"testing"

	"github.com/iodesystems/dun/proto"
)

func historyReasonOf(t *testing.T, sub *proto.Subscription) string {
	t.Helper()
	h, _ := next(t, sub, "history").Payload.(*proto.History)
	if h == nil {
		t.Fatal("history with no payload")
	}
	return h.Reason
}

// dund says why an engine replays the conversation: a crash restart and a
// restart with the same flags are restarts, another model is a
// model_switch, a session outliving dund restarts.
func TestHistoryReason(t *testing.T) {
	svc, _, dir := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), []string{"--model", "a"}, "")
	ses, _ := svc.Get(alice, id)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 1000)
	defer sub.Close()

	if err := svc.Restart(alice, id, []string{"--model", "b"}); err != nil {
		t.Fatal(err)
	}
	if r := historyReasonOf(t, sub); r != proto.HistoryModelSwitch {
		t.Errorf("another model: %q", r)
	}
	if err := svc.Restart(alice, id, []string{"--model", "b"}); err != nil {
		t.Fatal(err)
	}
	if r := historyReasonOf(t, sub); r != proto.HistoryRestart {
		t.Errorf("the same model: %q", r)
	}
	if err := ses.send(proto.NewCommand("", &proto.User{Content: "crash"})); err != nil {
		t.Fatal(err)
	}
	if r := historyReasonOf(t, sub); r != proto.HistoryRestart {
		t.Errorf("after a crash: %q", r)
	}

	// A session read back from the registry was shown before dund restarted.
	svc.Close()
	svc2, err := New(dir, &fakeSpawner{})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	s2, _ := svc2.Get(alice, id)
	_, sub2, _ := s2.log.Subscribe(s2.log.Last(), 1000)
	defer sub2.Close()
	if err := s2.ensureRunning(); err != nil {
		t.Fatal(err)
	}
	if r := historyReasonOf(t, sub2); r != proto.HistoryRestart {
		t.Errorf("after dund restarted: %q", r)
	}
}

// The first engine of a session resumes its conversation.
func TestHistoryReason_FirstIsResume(t *testing.T) {
	s := &session{}
	if r := s.historyReason(); r != proto.HistoryResume {
		t.Errorf("first: %q", r)
	}
	if r := s.historyReason(); r != proto.HistoryRestart {
		t.Errorf("second: %q", r)
	}
}
