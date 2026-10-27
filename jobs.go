package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// The jobs tool: one view of the session's background work (USER
// 2026-10-27: "Can we fuse the concept of a background job? Keep exec and
// agent launch different though. They just output a typed background job
// that is monitored.").
//
// Two kinds of job, each started by its own tool: a command exec handed to
// the background (bgjob.go, "background job #N", jobs(job:N)) and a
// sub-agent (subagent.go, "agent #N", jobs(agent:N)). What a model does with
// either is the same few things — list, wait, read the end of, end — so they
// share one tool; what only one kind can do (tell an agent, grep a command)
// is named as that kind's, and asking it of the other kind is an error that
// says what does apply.
//
// A child has exec, so it has jobs for its own commands. It never has
// agents of its own (no `agent` tool), and its harness's agent list is its
// own, empty — so it cannot see or touch its parent's or its siblings'.
//
// The old tools exec_monitor and agent_monitor still dispatch here, hidden
// from the list, so a resumed session or a model's habit still works.
// agent_monitor's quit is kill.

// jobsToolNames are the names withJobs answers: the tool and its hidden
// aliases.
var jobsToolNames = map[string]bool{"jobs": true, "exec_monitor": true, "agent_monitor": true}

// jobsToolDef describes the kinds this harness has: commands when it has
// exec, agents when it can spawn them.
func jobsToolDef(commands, agents bool) llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "jobs"
	props := map[string]any{
		"wait": map[string]any{"type": "boolean", "description": "block until it finishes; a new message stops the wait"},
	}
	var kinds, tail, kill []string
	if commands {
		kinds = append(kinds, "a command that outlived its exec timeout (job:N)")
		tail = append(tail, "a command's log")
		kill = append(kill, "ends a command and everything it started")
		props["job"] = map[string]any{"type": "integer", "description": "a command's job number"}
		props["grep"] = map[string]any{"type": "string", "description": "command only: while it runs, report only lines matching this regexp; \"\" clears it"}
		props["buffer_bytes"] = map[string]any{"type": "integer", "description": "command only: report progress each time this many matching bytes pile up; 0 = never"}
		props["ignore"] = map[string]any{"type": "boolean", "description": "command only: true mutes it, its finish notice too; it keeps running"}
	}
	if agents {
		kinds = append(kinds, "a sub-agent (agent:N)")
		tail = append(tail, "an agent's transcript")
		kill = append(kill, "dismisses an agent")
		props["agent"] = map[string]any{"type": "integer", "description": "a sub-agent's number"}
		props["tell"] = map[string]any{"type": "string", "description": "agent only: new work, or the answer to its question"}
		props["resume"] = map[string]any{"type": "boolean", "description": "agent only: restart a stopped agent from its transcript"}
	}
	props["tail"] = map[string]any{"type": "integer", "description": "show the last N lines of " + strings.Join(tail, " or ")}
	props["kill"] = map[string]any{"type": "boolean", "description": "true " + strings.Join(kill, ", or ")}
	td.Function.Description = "Background work: " + strings.Join(kinds, " and ") + ". You are notified when one " +
		"finishes, so do not poll. With no number, lists them all."
	td.Function.Parameters = map[string]any{"type": "object", "properties": props}
	return td
}

// jobsArgs is every parameter of jobs and of its two aliases.
type jobsArgs struct {
	Job         *int    `json:"job"`
	Agent       *int    `json:"agent"`
	Wait        bool    `json:"wait"`
	Tail        int     `json:"tail"`
	Kill        bool    `json:"kill"`
	Quit        bool    `json:"quit"` // agent_monitor's kill
	Tell        string  `json:"tell"`
	Resume      bool    `json:"resume"`
	Grep        *string `json:"grep"`
	BufferBytes *int    `json:"buffer_bytes"`
	Ignore      *bool   `json:"ignore"`
}

func withJobs(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if !jobsToolNames[tc.Function.Name] {
			return inner(ctx, tc)
		}
		var a jobsArgs
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
		a.Kill = a.Kill || a.Quit
		out := runJobs(ctx, h, a)
		if onCall != nil {
			onCall("jobs", a.reported(), out)
		}
		return out, nil
	}
}

// reported is the argument map the UI shows: what was sent, no zeros.
func (a jobsArgs) reported() map[string]any {
	m := map[string]any{}
	if a.Job != nil {
		m["job"] = *a.Job
	}
	if a.Agent != nil {
		m["agent"] = *a.Agent
	}
	if a.Wait {
		m["wait"] = true
	}
	if a.Tail > 0 {
		m["tail"] = a.Tail
	}
	if a.Kill {
		m["kill"] = true
	}
	if a.Tell != "" {
		m["tell"] = a.Tell
	}
	if a.Resume {
		m["resume"] = true
	}
	return m
}

const (
	jobsCommandParams = "A command's job (job:N) takes wait, tail, kill, grep, buffer_bytes, ignore."
	jobsAgentParams   = "An agent (agent:N) takes wait, tail, kill, tell, resume."
)

func runJobs(ctx context.Context, h *Harness, a jobsArgs) string {
	commandOnly := a.Grep != nil || a.BufferBytes != nil || a.Ignore != nil
	agentOnly := a.Tell != "" || a.Resume
	switch {
	case a.Job != nil && a.Agent != nil:
		return "ERROR: give job or agent, not both."
	case a.Job == nil && a.Agent == nil:
		if commandOnly || agentOnly || a.Wait || a.Tail > 0 || a.Kill {
			return "ERROR: name the job: job:N for a command, agent:N for a sub-agent. With no arguments, jobs lists them."
		}
		return h.jobsList()
	case a.Agent != nil:
		if commandOnly {
			return "ERROR: grep, buffer_bytes and ignore are for a command's job. " + jobsAgentParams
		}
		return agentMonitor(ctx, h, a.Agent, a.Tail, a.Tell, a.Wait, a.Resume, a.Kill)
	}
	if agentOnly {
		return "ERROR: tell and resume are for a sub-agent. " + jobsCommandParams
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
	if msg := tuneJob(h, j, a.BufferBytes, a.Grep, a.Ignore); msg != "" {
		return msg
	}
	var b strings.Builder
	if a.Wait {
		if note := j.wait(ctx, h); note != "" {
			b.WriteString(note + "\n")
		}
	}
	b.WriteString(jobReport(j, a.Tail))
	return b.String()
}

// jobsList is every job of both kinds, one line each, oldest first per kind.
func (h *Harness) jobsList() string {
	var lines []string
	for _, j := range h.bgJobList() {
		lines = append(lines, "job "+j.status())
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

// wait blocks until the job finishes, the turn is cancelled, or a message
// arrives for the session (the user's, or another session's) — the same
// rules as an agent's wait. It returns why it stopped early, or "" when the
// job finished.
//
// While someone waits, the job's own finish notice is not sent (see
// completionNote): the waiter's result carries it.
func (j *bgJob) wait(ctx context.Context, h *Harness) string {
	j.mu.Lock()
	if j.done {
		j.mu.Unlock()
		return ""
	}
	j.waiters++
	j.mu.Unlock()
	note := ""
	select {
	case <-j.doneCh:
	case <-ctx.Done():
		note = fmt.Sprintf("job #%d is still running; the turn was cancelled.", j.id)
	case <-h.userWaiting():
		note = "Stopped waiting because a message arrived (the user wrote, or another dun session did) — it follows."
	}
	j.mu.Lock()
	j.waiters--
	done := j.done
	j.mu.Unlock()
	if done {
		return ""
	}
	return note
}

// tuneJob applies the command-only settings; "" or an error message.
func tuneJob(h *Harness, j *bgJob, bufferBytes *int, grep *string, ignore *bool) string {
	if bufferBytes == nil && grep == nil && ignore == nil {
		return ""
	}
	j.mu.Lock()
	if grep != nil {
		if strings.TrimSpace(*grep) == "" {
			j.mon.Grep, j.re = "", nil
		} else {
			re, err := regexp.Compile(*grep)
			if err != nil {
				j.mu.Unlock()
				return fmt.Sprintf("ERROR: grep %q is not a valid regexp: %v", *grep, err)
			}
			j.mon.Grep, j.re = *grep, re
		}
	}
	if bufferBytes != nil {
		j.mon.BufferBytes = *bufferBytes
	}
	if ignore != nil {
		j.mon.Ignore = *ignore
	}
	j.mu.Unlock()
	h.jobsChanged() // muting/unmuting changes what the row says about itself
	return ""
}

// jobReport is one command's state: its status, its monitor, its log, and
// either the last tail lines of the log or what it produced since the last
// report (all of its output's end, once it finished).
func jobReport(j *bgJob, tail int) string {
	j.mu.Lock()
	mon, done, res, logPath := j.mon, j.done, j.res, j.logPath
	// Hand back whatever has accumulated, whether or not it met the threshold —
	// an explicit ask is not the same as a scheduled report.
	pending := string(j.pending)
	j.pending = nil
	early := string(j.early)
	j.mu.Unlock()

	var b strings.Builder
	b.WriteString(j.status())
	b.WriteString("\n" + describeMonitor(mon))
	if logPath != "" {
		b.WriteString("\nlog: " + logPath)
	}
	switch {
	case tail > 0:
		text := early
		if done {
			text = res.Output
		}
		if logPath != "" {
			if t, err := readTail(logPath, 1<<20); err == nil {
				text = t
			}
		}
		fmt.Fprintf(&b, "\nlast %d lines:\n%s", tail, lastLines(text, tail))
	case done:
		b.WriteString("\n" + tailOf(res.Output, bgTailBytes))
	case pending != "":
		b.WriteString("\nsince the last report:\n" + strings.TrimRight(pending, "\n"))
	default:
		b.WriteString("\n(nothing new since the last report)")
	}
	return b.String()
}

// readTail reads at most max bytes from the end of a file.
func readTail(path string, max int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if off := st.Size() - max; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(f)
	return string(b), err
}

// lastLines is the last n lines of s.
func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "(no output)"
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
