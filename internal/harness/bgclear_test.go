package harness

import "testing"

// ClearDoneJobs drops finished jobs and keeps running ones, and pushes the
// list so every client sees it.
func TestClearDoneJobs(t *testing.T) {
	var pushed []JobInfo
	h := &Harness{bgJobs: map[int]*bgJob{
		1: {id: 1, done: true},
		2: {id: 2},
		3: {id: 3, done: true},
	}}
	h.cfg.OnJobs = func(js []JobInfo) { pushed = js }
	if n := h.ClearDoneJobs(); n != 2 {
		t.Fatalf("cleared %d, want 2", n)
	}
	if len(h.bgJobs) != 1 || h.bgJobs[2] == nil {
		t.Fatalf("left %v, want only job 2", h.bgJobs)
	}
	if len(pushed) != 1 || pushed[0].ID != 2 {
		t.Errorf("pushed %+v, want only job 2", pushed)
	}
	if n := h.ClearDoneJobs(); n != 0 {
		t.Errorf("second clear dropped %d", n)
	}
}
