package main

import (
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// A command's time runs to the first event that answers it, under any of
// its keys; one answer ends it, and an event nobody waits for files nothing.
func TestCmdClock(t *testing.T) {
	var got []string
	c := &cmdClock{note: func(cmd string, d time.Duration) {
		if d < 10*time.Millisecond {
			t.Errorf("%s took %s, want at least the wait", cmd, d)
		}
		got = append(got, cmd)
	}}
	at := time.Now().Add(-20 * time.Millisecond)
	c.start(ctrlCmdName("slash", "/mcp"), at, "slash")
	c.also("srv:mcp", at) // routed to the server switch
	c.answered(&proto.ServerEvent{ID: "mcp", Background: true})
	c.answered(&proto.ServerEvent{ID: "mcp"})
	c.answered(&proto.ControlEvent{ID: "slash"}) // the same command: already answered
	c.start(ctrlCmdName("jobs", ""), at, "jobs")
	c.answered(&proto.ControlEvent{ID: "until"})
	c.answered(&proto.ControlEvent{ID: "jobs"})
	if len(got) != 2 || got[0] != "/mcp" || got[1] != "/jobs" {
		t.Errorf("filed %v", got)
	}
}
