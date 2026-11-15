package dund

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/proto"
)

// A job's output is read from the log its session's jobs event named, a
// finished job's too: from an offset, or its tail; another job id, another
// user, or a path the engine did not name read nothing.
func TestJobOutput(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	logPath := filepath.Join(t.TempDir(), "job-3.log")
	if err := os.WriteFile(logPath, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ses, _ := svc.Get(alice, id)
	ses.log.Append(proto.NewEvent(&proto.Jobs{Jobs: []proto.JobInfo{{ID: 3, Log: logPath, State: "exited"}}}))

	o, err := svc.JobOutput(alice, id, 3, 0, 0, 0)
	if err != nil || o.Data != "line one\nline two\n" || o.Offset != 0 || o.Next != 18 || o.Size != 18 || o.State != "exited" {
		t.Fatalf("from 0: %+v %v", o, err)
	}
	if o, _ = svc.JobOutput(alice, id, 3, 9, 0, 0); o.Data != "line two\n" || o.Offset != 9 {
		t.Fatalf("since 9: %+v", o)
	}
	if o, _ = svc.JobOutput(alice, id, 3, -1, 4, 0); o.Data != "two\n" || o.Offset != 14 {
		t.Fatalf("tail 4: %+v", o)
	}
	if o, _ = svc.JobOutput(alice, id, 3, 0, 0, 4); o.Data != "line" || o.Next != 4 {
		t.Fatalf("limit 4: %+v", o)
	}
	if o, _ = svc.JobOutput(alice, id, 3, 99, 0, 0); o.Data != "" || o.Next != 18 {
		t.Fatalf("since past the end: %+v", o)
	}
	if _, err = svc.JobOutput(alice, id, 9, 0, 0, 0); err == nil || !strings.Contains(err.Error(), "#9") {
		t.Errorf("an unknown job: %v", err)
	}
	if _, err = svc.JobOutput(bob, id, 3, 0, 0, 0); err != ErrNoSession {
		t.Errorf("another user's session: %v", err)
	}
}
