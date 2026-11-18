package tui

import (
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// The picker merges local and dund sessions by dund's rule: running first,
// then last activity, not id.
func TestOrderSessionsFollowsDundRule(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	infos := []harness.SessionInfo{
		{ID: "z-new-id-idle", ModTime: t0.Add(time.Hour)},
		{ID: "a-old-id-active", ModTime: t0.Add(3 * time.Hour)},
		{ID: "m-running", ModTime: t0},
	}
	live := map[string]bool{"m-running": true}
	got := orderSessions(infos, live, []proto.SessionSummary{{ID: "z-new-id-idle", LastActive: "2026-01-01T09:00:00Z"}})
	want := []string{"m-running", "z-new-id-idle", "a-old-id-active"}
	for i, w := range want {
		if got[i].ID != w {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
}
