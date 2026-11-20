package harness

import (
	"testing"
	"time"
)

// addLiveJob puts a job into the harness's live set the way production does.
func addLiveJob(h *Harness, j *bgJob) {
	h.bgMu.Lock()
	if h.bgLive == nil {
		h.bgLive = map[*bgJob]struct{}{}
	}
	h.bgLive[j] = struct{}{}
	h.bgMu.Unlock()
}

// TestQuietIn_NoJobs: with nothing running, QuietIn is 0 and AllJobsQuiet is true.
func TestQuietIn_NoJobs(t *testing.T) {
	h := testHarness(t)
	if d := h.QuietIn(30 * time.Second); d != 0 {
		t.Errorf("QuietIn(30s) with no jobs = %v, want 0", d)
	}
	if !h.AllJobsQuiet(30 * time.Second) {
		t.Error("AllJobsQuiet(30s) with no jobs = false, want true")
	}
}

// TestQuietIn_QuietJob: a job that has produced no output for longer than the
// threshold is quiet.
func TestQuietIn_QuietJob(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 1, "sleep 60")
	j.mu.Lock()
	j.started = time.Now().Add(-2 * time.Minute) // started 2 min ago, no output
	j.mu.Unlock()
	addLiveJob(h, j)

	// Quiet for 30s: the job has been silent for 2 min, so it is quiet.
	if !h.AllJobsQuiet(30 * time.Second) {
		t.Error("AllJobsQuiet(30s) = false, want true (job silent for 2 min)")
	}
	if d := h.QuietIn(30 * time.Second); d != 0 {
		t.Errorf("QuietIn(30s) = %v, want 0 (job already quiet)", d)
	}
}

// TestQuietIn_ActiveJob: a job that has produced output recently is not quiet.
func TestQuietIn_ActiveJob(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 1, "build")
	j.mu.Lock()
	j.started = time.Now().Add(-2 * time.Minute)
	j.outAt = time.Now().Add(-5 * time.Second) // output 5s ago
	j.mu.Unlock()
	addLiveJob(h, j)

	// Not quiet for 30s: the job printed 5s ago.
	if h.AllJobsQuiet(30 * time.Second) {
		t.Error("AllJobsQuiet(30s) = true, want false (job active 5s ago)")
	}
	d := h.QuietIn(30 * time.Second)
	if d <= 0 || d > 30*time.Second {
		t.Errorf("QuietIn(30s) = %v, want (0, 30s]", d)
	}
	// Should be roughly 25s (30s - 5s).
	if d < 20*time.Second || d > 28*time.Second {
		t.Errorf("QuietIn(30s) = %v, want ~25s", d)
	}
}

// TestQuietIn_FreshJob: a job that just started and has no output counts from
// its start time.
func TestQuietIn_FreshJob(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 1, "build")
	j.mu.Lock()
	j.started = time.Now().Add(-10 * time.Second) // started 10s ago, no output
	j.mu.Unlock()
	addLiveJob(h, j)

	// Not quiet for 30s: the job has only been alive for 10s.
	if h.AllJobsQuiet(30 * time.Second) {
		t.Error("AllJobsQuiet(30s) = true, want false (job started 10s ago)")
	}
	d := h.QuietIn(30 * time.Second)
	if d <= 0 || d > 30*time.Second {
		t.Errorf("QuietIn(30s) = %v, want (0, 30s]", d)
	}
	// Should be roughly 20s (30s - 10s).
	if d < 15*time.Second || d > 25*time.Second {
		t.Errorf("QuietIn(30s) = %v, want ~20s", d)
	}
}

// TestQuietIn_MultipleJobs: QuietIn returns the MAXIMUM remaining time across
// all live jobs.
func TestQuietIn_MultipleJobs(t *testing.T) {
	h := testHarness(t)
	// Job 1: started 2 min ago, no output — quiet for 30s.
	j1 := testJob(h, 1, "done")
	j1.mu.Lock()
	j1.started = time.Now().Add(-2 * time.Minute)
	j1.mu.Unlock()
	// Job 2: started 1 min ago, output 10s ago — not quiet for 30s.
	j2 := testJob(h, 2, "busy")
	j2.mu.Lock()
	j2.started = time.Now().Add(-time.Minute)
	j2.outAt = time.Now().Add(-10 * time.Second)
	j2.mu.Unlock()
	addLiveJob(h, j1)
	addLiveJob(h, j2)

	// Not all quiet: job 2 is not quiet.
	if h.AllJobsQuiet(30 * time.Second) {
		t.Error("AllJobsQuiet(30s) = true, want false (job 2 not quiet)")
	}
	// QuietIn should be ~20s (the max: job 1 is 0, job 2 is ~20s).
	d := h.QuietIn(30 * time.Second)
	if d <= 0 || d > 30*time.Second {
		t.Errorf("QuietIn(30s) = %v, want (0, 30s]", d)
	}
	if d < 15*time.Second || d > 25*time.Second {
		t.Errorf("QuietIn(30s) = %v, want ~20s", d)
	}
}

// TestQuietIn_AllQuiet: when all jobs are quiet, QuietIn is 0.
func TestQuietIn_AllQuiet(t *testing.T) {
	h := testHarness(t)
	j1 := testJob(h, 1, "a")
	j1.mu.Lock()
	j1.started = time.Now().Add(-5 * time.Minute)
	j1.mu.Unlock()
	j2 := testJob(h, 2, "b")
	j2.mu.Lock()
	j2.started = time.Now().Add(-3 * time.Minute)
	j2.outAt = time.Now().Add(-4 * time.Minute) // output 4 min ago
	j2.mu.Unlock()
	addLiveJob(h, j1)
	addLiveJob(h, j2)

	if !h.AllJobsQuiet(30 * time.Second) {
		t.Error("AllJobsQuiet(30s) = false, want true (both jobs quiet)")
	}
	if d := h.QuietIn(30 * time.Second); d != 0 {
		t.Errorf("QuietIn(30s) = %v, want 0", d)
	}
}

// TestQuietIn_LargeThreshold: a job active 10s ago is quiet for a 5s threshold.
func TestQuietIn_LargeThreshold(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 1, "build")
	j.mu.Lock()
	j.started = time.Now().Add(-time.Minute)
	j.outAt = time.Now().Add(-10 * time.Second)
	j.mu.Unlock()
	addLiveJob(h, j)

	// Quiet for 5s: the job last printed 10s ago, which is more than 5s.
	if !h.AllJobsQuiet(5 * time.Second) {
		t.Error("AllJobsQuiet(5s) = false, want true (job silent for 10s)")
	}
}
