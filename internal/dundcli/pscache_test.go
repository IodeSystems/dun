package dundcli

import (
	"errors"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/procps"
)

// --watch runs poly-lsp-mcp status at most every rootsEvery, failures
// included, and again for a new daemon.
func TestRootsCache_Interval(t *testing.T) {
	runs := 0
	c := &rootsCache{read: func(*procps.Proc) ([]procps.Root, error) {
		runs++
		return nil, errors.New("down")
	}}
	d := &procps.Proc{PID: 7}
	t0 := time.Now()
	for _, dt := range []time.Duration{0, 2 * time.Second, 9 * time.Second, 10 * time.Second, 12 * time.Second} {
		if _, err := c.get(d, t0.Add(dt)); err == nil {
			t.Fatal("error lost")
		}
	}
	if runs != 2 {
		t.Errorf("ran %d times, want 2", runs)
	}
	c.get(&procps.Proc{PID: 8}, t0.Add(13*time.Second))
	if runs != 3 {
		t.Errorf("new daemon not read: %d", runs)
	}
}
