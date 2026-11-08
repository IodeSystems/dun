package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/procstat"
)

// The jobs tool: one view of the session's background work (USER
// 2026-10-27: "Can we fuse the concept of a background job? Keep exec and
// agent launch different though. They just output a typed background job
// that is monitored."). Since 2026-11-06 it only MANAGES commands (kill,
// mute, report thresholds) and lists sub-agents; output is read from the
// job's log with file_read, and a sub-agent is managed with the agent tool.
//
// A child has exec, so it has jobs for its own commands. It never has
// agents of its own (no `agent` tool), and its harness's agent list is its
// own, empty — so it cannot see or touch its parent's or its siblings'.

// jobs MANAGES commands; it does not read them (USER 2026-11-06: "Tail wait
// grep die. Agent stuff moves."). A command's output is its log, a file read
// with file_read; its monitor reports when it finishes and, if asked, as its
// output grows (bytes or time), saying how much was written, not what. A
// sub-agent is managed with the agent tool (agent({id, tell|node|resume|
// dismiss})); jobs only lists it.
func jobsToolDef(commands, agents bool) llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "jobs"
	props := map[string]any{}
	desc := "Lists background work"
	if commands {
		props["job"] = map[string]any{"type": "integer", "description": "a command's job number"}
		props["kill"] = map[string]any{"type": "boolean", "description": "true ends the command and everything it started"}
		props["mute"] = map[string]any{"type": "boolean", "description": "true: no more reports about it, its finish included; it keeps running"}
		props["every_bytes"] = map[string]any{"type": "integer", "description": "report each time this many more bytes are written; 0 = off"}
		props["every_secs"] = map[string]any{"type": "integer", "description": "report new output at most this often; 0 = off"}
		desc = "Manage a command that outlived its exec timeout (job:N): a monitor is on it, and you are told when " +
			"it finishes, so there is nothing to check. Its output is its log; read it with file_read. Reports as " +
			"output grows are off until you set every_bytes or every_secs (whichever comes first); they say how " +
			"much was written, not what, and count only what the command prints to stdout/stderr: progress it " +
			"writes to a file is not seen. With no job, lists background work"
	}
	if agents {
		desc += "; sub-agents are listed here and managed with agent({id, …})"
	}
	td.Function.Description = desc + "."
	td.Function.Parameters = map[string]any{"type": "object", "properties": props}
	return td
}

// jobsArgs is every parameter of jobs.
type jobsArgs struct {
	Job        *int  `json:"job"`
	Kill       bool  `json:"kill"`
	Mute       *bool `json:"mute"`
	EveryBytes *int  `json:"every_bytes"`
	EverySecs  *int  `json:"every_secs"`
	// unknown is what else the call sent: refused, naming what jobs takes.
	unknown []string
}

var jobsParams = map[string]bool{"job": true, "kill": true, "mute": true, "every_bytes": true, "every_secs": true}

func withJobs(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "jobs" {
			return inner(ctx, tc)
		}
		var a jobsArgs
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
		var sent map[string]any
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &sent)
		for k := range sent {
			if !jobsParams[k] {
				a.unknown = append(a.unknown, k)
			}
		}
		// A plain look at the same job, asked again with nothing changed,
		// waits until it changes (USER 2026-10-27: a session polled until the
		// loop guard stopped the turn). Keyed by the job alone: a key that
		// carried tail let tail:5 and tail:10 alternate past it (Terminal-Bench,
		// 2026-11-06: six reads in five seconds).
		sig, plain := a.pollSig()
		out := runJobs(h, a)
		waited := plain && sig != "" && h.jobsRepeat(sig, out)
		if waited {
			if j := h.bgJobByID(*a.Job); j != nil {
				if note := j.waitChange(ctx, h); note != "" {
					out = note + "\n"
				} else {
					out = ""
				}
				out += "Nothing had changed since you last asked, so this call waited until it did.\n" + runJobs(h, a)
			}
		}
		if plain {
			h.jobsNote(sig, out)
		}
		if strings.Contains(out, jobsMessageStop) {
			h.loop.observe("", "")
		}
		if onCall != nil {
			onCall("jobs", a.reported(), out)
		}
		return out, nil
	}
}

// pollSig is a plain look at one job as a key; plain is false for anything
// that changes something, which never turns into a wait.
func (a jobsArgs) pollSig() (string, bool) {
	if a.Kill || a.Mute != nil || a.EveryBytes != nil || a.EverySecs != nil || len(a.unknown) > 0 {
		return "", false
	}
	if a.Job != nil {
		return fmt.Sprintf("job:%d", *a.Job), true
	}
	return "", true // the list: noted, but never waited on
}

// jobsMessageStop is what a wait broken by a message says.
const jobsMessageStop = "Stopped waiting because a message arrived (the user wrote, or another dun session did) — it follows."

var pollDigits = regexp.MustCompile(`[0-9]+`)

// jobsRepeat reports whether sig is the question asked last and out is the
// answer it got. Numbers are masked: "RUNNING for 12s" and "for 14s" are the
// same answer.
func (h *Harness) jobsRepeat(sig, out string) bool {
	h.jobsPollMu.Lock()
	defer h.jobsPollMu.Unlock()
	return h.jobsPollSig == sig && pollDigits.ReplaceAllString(out, "#") == h.jobsPollOut
}

// jobsNote records a plain read.
func (h *Harness) jobsNote(sig, out string) {
	h.jobsPollMu.Lock()
	defer h.jobsPollMu.Unlock()
	h.jobsPollSig, h.jobsPollOut = sig, pollDigits.ReplaceAllString(out, "#")
}

// reported is the argument map the UI shows: what was sent, no zeros.
func (a jobsArgs) reported() map[string]any {
	m := map[string]any{}
	if a.Job != nil {
		m["job"] = *a.Job
	}
	if a.Kill {
		m["kill"] = true
	}
	if a.Mute != nil {
		m["mute"] = *a.Mute
	}
	if a.EveryBytes != nil {
		m["every_bytes"] = *a.EveryBytes
	}
	if a.EverySecs != nil {
		m["every_secs"] = *a.EverySecs
	}
	return m
}

func runJobs(h *Harness, a jobsArgs) string {
	if len(a.unknown) > 0 {
		sort.Strings(a.unknown)
		return "ERROR: jobs takes job, kill, mute, every_bytes, every_secs (not " + strings.Join(a.unknown, ", ") +
			"). A job's output is its log: read it with file_read. Sub-agents: agent({id, …})."
	}
	if a.Job == nil {
		if a.Kill || a.Mute != nil || a.EveryBytes != nil || a.EverySecs != nil {
			// The model sent kill with no job three times in 54 Terminal-Bench
			// trials and retried with job:1 each time: give it the running jobs
			// to choose from in the same answer.
			return "ERROR: name the job: job:N. " + h.jobsRunning()
		}
		return h.jobsList()
	}
	if a.Kill {
		msg, err := h.KillJob(*a.Job, "model")
		if err != nil {
			return "ERROR: " + err.Error()
		}
		return msg
	}
	j := h.bgJobByID(*a.Job)
	if j == nil {
		return fmt.Sprintf("ERROR: no background job #%d.", *a.Job)
	}
	tuneJob(h, j, a.EveryBytes, a.EverySecs, a.Mute)
	return jobReport(j)
}

// jobsRunning is the background commands still running, with what their
// processes are doing: what a call that needs a job number is answered with
// when it has none.
func (h *Harness) jobsRunning() string {
	var lines []string
	for _, j := range h.bgJobList() {
		j.mu.Lock()
		done := j.done
		j.mu.Unlock()
		if !done {
			lines = append(lines, "job "+j.status()+j.usageSuffix())
		}
	}
	if len(lines) == 0 {
		return "No background command is running."
	}
	return "Running now:\n" + strings.Join(lines, "\n")
}

// usageSuffix is a running job's processes, on the same line (" · processes: …"), or ""
// when the job is done or its processes are unknown (a docker job).
func (j *bgJob) usageSuffix() string {
	j.mu.Lock()
	since := j.outAt
	if since.IsZero() {
		since = j.started
	}
	j.mu.Unlock()
	line, verdict, ok := j.usage(time.Since(since))
	if !ok {
		return ""
	}
	if verdict != "" {
		line += " — " + verdict
	}
	return " · processes: " + line
}

// jobsList is every job of both kinds, one line each, oldest first per kind.
func (h *Harness) jobsList() string {
	var lines []string
	for _, j := range h.bgJobList() {
		lines = append(lines, "job "+j.status()+j.usageSuffix())
	}
	for _, sa := range h.agentList() {
		line := "agent " + sa.line()
		if _, status, _, _ := sa.snapshot(); status == "" {
			line += " — task: " + oneLine(sa.prompt, 80)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "No background jobs or sub-agents."
	}
	return strings.Join(lines, "\n")
}

// waitChange blocks until the job writes more or finishes, the turn is
// cancelled, or a message arrives: what a repeated look is answered with.
// "" when the job moved; else why the wait stopped.
func (j *bgJob) waitChange(ctx context.Context, h *Harness) string {
	j.mu.Lock()
	written := j.written
	j.mu.Unlock()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		j.mu.Lock()
		moved := j.done || j.written != written
		id := j.id
		j.mu.Unlock()
		if moved {
			return ""
		}
		select {
		case <-j.doneCh:
		case <-tick.C:
		case <-ctx.Done():
			return fmt.Sprintf("job #%d is still running; the turn was cancelled.", id)
		case <-h.userWaiting():
			return jobsMessageStop
		}
	}
}

// tuneJob applies the monitor settings given.
func tuneJob(h *Harness, j *bgJob, everyBytes, everySecs *int, mute *bool) {
	if everyBytes == nil && everySecs == nil && mute == nil {
		return
	}
	j.mu.Lock()
	if everyBytes != nil {
		j.mon.EveryBytes = max(*everyBytes, 0)
	}
	if everySecs != nil {
		j.mon.EverySecs = max(*everySecs, 0)
	}
	if mute != nil {
		j.mon.Mute = *mute
	}
	note := j.reportLocked(false)
	j.armReportLocked()
	j.mu.Unlock()
	if note != "" {
		j.notify(note)
	}
	h.jobsChanged() // muting/unmuting changes what the row says about itself
}

// jobReport is one command's state: status, what its processes are doing,
// its monitor, and how much it wrote; never the output (that is its log).
func jobReport(j *bgJob) string {
	j.mu.Lock()
	mon, done, written, lines := j.mon, j.done, j.written, j.lines
	since := j.outAt
	if since.IsZero() {
		since = j.started
	}
	j.mu.Unlock()

	var b strings.Builder
	b.WriteString(j.status())
	if !done {
		if line, verdict, ok := j.usage(time.Since(since)); ok {
			b.WriteString("\nprocesses: " + line)
			if verdict != "" {
				b.WriteString(" — " + verdict)
			}
		}
	}
	b.WriteString("\n" + describeMonitor(mon))
	fmt.Fprintf(&b, "\nwritten: %s, %s to its combined stdout/stderr", procstat.Bytes(float64(written)), linesOf(lines))
	b.WriteString("\n" + j.logLine())
	return b.String()
}
