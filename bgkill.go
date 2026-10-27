package dun

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Killing a background job.
//
// A job used to be unkillable: the model could mute one, not stop it, and
// an interrupt leaves the turn's jobs running on purpose. So a runaway
// `go test ./...` burned the machine until it finished on its own.
//
// A kill cancels the job's context. Each backend turns that into the end of
// EVERYTHING the command started, not only its shell:
//
//   - HostShell (the default): the shell is a setsid group leader and the
//     command runs in its group, so termGroup sends SIGTERM to the group,
//     then SIGKILL to what is left after killTermGrace.
//   - HostExec: the same, through killGroup on the exec.Cmd.
//   - DockerExec: `docker stop --time 2` on the named container (SIGTERM to
//     its PID 1, SIGKILL after 2s, which ends every process in it), then the
//     `docker run` client's group as above.
//
// The job then finishes like any other, with ExecResult.Killed set, so the
// log, the pane and the completion notice all say it was killed and by whom.

// jobKillWait bounds how long a kill waits for the job to report that it is
// gone: the SIGTERM grace, plus time for docker stop and the output drain.
const jobKillWait = killTermGrace + 4*time.Second

// kill asks the job to end and records who asked. It reports false when the
// job had already finished. Calling it again is harmless: the first caller's
// name is kept and the context is already cancelled.
func (j *bgJob) kill(by string) bool {
	j.mu.Lock()
	if j.done {
		j.mu.Unlock()
		return false
	}
	if j.killedBy == "" {
		j.killedBy, j.killAfter = by, time.Since(j.started)
	}
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

// killAndWait kills the job and waits, bounded, for it to finish. The answer
// is what the caller shows: killed, already finished, or still exiting.
func (j *bgJob) killAndWait(by string) string {
	if !j.kill(by) {
		return fmt.Sprintf("job %s already finished: %s", j.name(), j.status())
	}
	select {
	case <-j.doneCh:
		j.mu.Lock()
		verdict := j.res.Killed
		if verdict == "" {
			verdict = "finished on its own before the kill took effect: " + outcome(j.res)
		}
		j.mu.Unlock()
		return fmt.Sprintf("job %s %s, with everything it started — `%s`", j.name(), verdict, j.command)
	case <-time.After(jobKillWait):
		return fmt.Sprintf("job %s was sent SIGTERM and SIGKILL but has not exited yet — `%s`", j.name(), j.command)
	}
}

// name is how a message refers to the job: its number, or "(in grace)" for a
// command that has not been handed to the background yet.
func (j *bgJob) name() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.id == 0 {
		return "(not yet backgrounded)"
	}
	return fmt.Sprintf("#%d", j.id)
}

// KillJob ends background job id and everything it started: SIGTERM to its
// process group, SIGKILL after killTermGrace. by is who asked ("user",
// "model"); the job's log and its completion notice name them. A job that
// already finished answers so; only an unknown id is an error.
func (h *Harness) KillJob(id int, by string) (string, error) {
	j := h.bgJobByID(id)
	if j == nil {
		return "", fmt.Errorf("no background job #%d", id)
	}
	return j.killAndWait(by), nil
}

// KillAllJobs ends every job still running, including commands still inside
// their grace (no number yet: the tool call that started them gets the
// "killed" result inline). Returns one line per job, or a note when none run.
func (h *Harness) KillAllJobs(by string) string {
	live := h.liveJobs()
	if len(live) == 0 {
		return "no background jobs are running"
	}
	out := make([]string, len(live))
	done := make(chan struct{})
	for i, j := range live {
		go func() {
			out[i] = j.killAndWait(by)
			done <- struct{}{}
		}()
	}
	for range live {
		<-done
	}
	return strings.Join(out, "\n")
}

// liveJobs is every job not yet finished, numbered ones first in id order,
// then the unnumbered ones oldest first.
func (h *Harness) liveJobs() []*bgJob {
	h.bgMu.Lock()
	out := make([]*bgJob, 0, len(h.bgLive))
	for j := range h.bgLive {
		out = append(out, j)
	}
	h.bgMu.Unlock()
	type key struct {
		id      int
		started time.Time
	}
	keys := make(map[*bgJob]key, len(out))
	for _, j := range out {
		j.mu.Lock()
		keys[j] = key{j.id, j.started}
		j.mu.Unlock()
	}
	sort.Slice(out, func(a, b int) bool {
		ka, kb := keys[out[a]], keys[out[b]]
		if (ka.id == 0) != (kb.id == 0) {
			return ka.id != 0
		}
		if ka.id != kb.id {
			return ka.id < kb.id
		}
		return ka.started.Before(kb.started)
	})
	return out
}

// RunningJobs is every numbered job still running, oldest first.
func (h *Harness) RunningJobs() []JobInfo {
	var out []JobInfo
	for _, j := range h.bgJobList() {
		if info := j.info(); info.State == "running" {
			out = append(out, info)
		}
	}
	return out
}

// jobsStartedSince is the jobs started at or after t that are still running.
// A command still inside its grace is handed to the background first, so it
// has a number the user can kill it by.
func (h *Harness) jobsStartedSince(t time.Time) []JobInfo {
	var out []JobInfo
	for _, j := range h.liveJobs() {
		j.mu.Lock()
		started := j.started
		j.mu.Unlock()
		if started.Before(t) {
			continue
		}
		if _, done := j.settle(); done {
			continue
		}
		if info := j.info(); info.State == "running" {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// JobLine is one running job in a short list: "#3 go test ./... (2m)".
func JobLine(j JobInfo, now time.Time) string {
	return fmt.Sprintf("#%d %s (%s)", j.ID, ShortCommand(j.Command, 60), ShortAge(now.Sub(time.Unix(j.Started, 0))))
}

// ShortCommand is a command's first line, cut to max runes.
func ShortCommand(c string, max int) string {
	c = strings.TrimSpace(c)
	if i := strings.IndexByte(c, '\n'); i >= 0 {
		c = c[:i] + " …"
	}
	if r := []rune(c); len(r) > max {
		c = string(r[:max-1]) + "…"
	}
	return c
}

// ShortAge is a duration as people say it: 42s, 2m, 1h5m.
func ShortAge(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	}
	return fmt.Sprintf("%dh%dm", s/3600, s%3600/60)
}

// lastLine is the last non-empty line of b.
func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
