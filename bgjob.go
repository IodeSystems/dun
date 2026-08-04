package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Background jobs — what a long command looks like while it is still running.
//
// A background job used to be ONE event. `startBackground` called Run, which
// buffered everything, and nothing reached the model until the process exited:
// `sleep 10; echo hi; sleep 30` delivered "hi" at t≈40s. A job that died in its
// second second and a job still working looked identical for as long as it ran
// — the same shape as the foreground hang that motivated the exec deadline,
// except a deadline is exactly what a background job must not have.
//
// Two changes. Output streams to a FILE, and the path goes to the model, so a
// 50k-line build log is grep-able instead of spent on the context window. And
// the model can ask to hear about a job WHILE it runs — bounded, because
// streaming without a bound is worse than silence: an unfiltered `go build`
// would narrate itself into the context window one line at a time.

// bgTailBytes is how much of a log the completion notice and a monitor read
// carry inline. Enough to see the failure; the file has the rest.
const bgTailBytes = 4000

// bgMonitor is what a job is allowed to say while it runs. The zero value is
// the old behaviour — silent until it exits — which stays the DEFAULT: a job
// nobody asked about should not spend context.
type bgMonitor struct {
	// BufferBytes reports once this many matched-but-unreported bytes pile up.
	// 0 = never report progress.
	BufferBytes int
	// Grep, when set, limits reports to lines matching it. Compiled once.
	Grep string
	// Ignore mutes the job completely, INCLUDING its completion. For a job
	// whose answer stopped mattering — the model moved on, the check was
	// superseded — where the notification is pure noise.
	Ignore bool
}

// bgJob is one background command: its log, its monitor settings, and whatever
// it has produced so far.
type bgJob struct {
	id      int
	command string
	logPath string
	logRef  string // how the model addresses this log (see logLine)
	started time.Time
	h       *Harness

	// hb is the "still running" reminder (heartbeat.go). Debounced by anything
	// this job says, which is why every notification goes through j.notify.
	hb *heartbeat

	mu      sync.Mutex
	mon     bgMonitor
	re      *regexp.Regexp
	log     *os.File
	partial []byte // bytes since the last newline; a line is not reportable yet
	pending []byte // matched lines not yet reported
	written int64
	done    bool
	res     ExecResult
	ended   time.Time
}

// Write is the tee end of ExecBackend.Run: the command's combined output
// arrives here as it is produced. Everything goes to the log; only complete,
// matching lines are candidates for a progress report, because half a line
// tells the model nothing and a regexp cannot judge one.
func (j *bgJob) Write(p []byte) (int, error) {
	j.mu.Lock()
	if j.log != nil {
		_, _ = j.log.Write(p)
	}
	j.written += int64(len(p))

	j.partial = append(j.partial, p...)
	if i := lastNewline(j.partial); i >= 0 {
		complete := j.partial[:i+1]
		j.partial = append([]byte(nil), j.partial[i+1:]...)
		j.pending = append(j.pending, j.filter(complete)...)
	}
	note := j.reportLocked(false)
	j.mu.Unlock()

	// Notify outside the lock: it takes the harness's queue lock and may call
	// out to a UI callback, and nothing good comes of holding a job's lock
	// across either.
	if note != "" {
		j.notify(note)
	}
	return len(p), nil
}

// notify is every word this job says to the model, and the ONE place the
// heartbeat's debounce is reset. Hooking it here rather than at each event
// means a new kind of report cannot forget to count as a sign of life — and the
// reminder itself deliberately does NOT go through here, or it would reset the
// silence it is reporting on.
func (j *bgJob) notify(text string) {
	if j.hb != nil {
		j.hb.spoke()
	}
	j.h.notifyAndWake(text)
}

// filter keeps the lines a monitor asked for. No pattern = everything.
func (j *bgJob) filter(b []byte) []byte {
	if j.re == nil {
		return b
	}
	var keep []byte
	for _, line := range strings.SplitAfter(string(b), "\n") {
		if line != "" && j.re.MatchString(line) {
			keep = append(keep, line...)
		}
	}
	return keep
}

// reportLocked returns the progress note to send, if one is due, and clears
// what it reports. force ignores the byte threshold (a monitor read wants
// whatever is there now).
func (j *bgJob) reportLocked(force bool) string {
	if j.mon.Ignore || len(j.pending) == 0 {
		return ""
	}
	if !force && (j.mon.BufferBytes <= 0 || len(j.pending) < j.mon.BufferBytes) {
		return ""
	}
	body := string(j.pending)
	j.pending = nil
	return fmt.Sprintf("background job #%d — `%s` (still running, %s):\n%s",
		j.id, j.command, roundDur(time.Since(j.started)), strings.TrimRight(body, "\n"))
}

// finished records the outcome and returns the completion notice.
func (j *bgJob) finished(res ExecResult) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.done, j.res, j.ended = true, res, time.Now()
	if j.log != nil {
		_ = j.log.Close()
		j.log = nil
	}
	if j.mon.Ignore {
		return ""
	}
	return fmt.Sprintf("background job #%d %s — `%s` (%s)\n%s\n%s",
		j.id, outcome(res), j.command, roundDur(j.ended.Sub(j.started)),
		j.logLine(), tailOf(res.Output, bgTailBytes))
}

// logLine cites the job's log by REF. It used to hand over a host path with
// "grep it with exec", which under --docker names a file the model's container
// cannot open, and with no exec backend names one it has no way to read at all.
func (j *bgJob) logLine() string {
	if j.logPath == "" {
		return "(no log file)"
	}
	if j.logRef == "" && j.h != nil {
		j.logRef = j.h.registerSpill("job", j.logPath)
	}
	if j.logRef == "" {
		return "full output: " + j.logPath
	}
	return fmt.Sprintf("full output: ref %q — read it with recap({ref:%q, grep:\"…\"}), or head/tail",
		j.logRef, j.logRef)
}

// status is the one-line answer to "what is job N doing?".
func (j *bgJob) status() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done {
		return fmt.Sprintf("#%d %s in %s — `%s`", j.id, outcome(j.res), roundDur(j.ended.Sub(j.started)), j.command)
	}
	return fmt.Sprintf("#%d RUNNING for %s (%d bytes out) — `%s`",
		j.id, roundDur(time.Since(j.started)), j.written, j.command)
}

func outcome(r ExecResult) string {
	switch {
	case r.TimedOut:
		return "TIMED OUT"
	case r.Err != "":
		return "FAILED to run (" + r.Err + ")"
	case r.Code != 0:
		return fmt.Sprintf("FAILED (exit %d)", r.Code)
	}
	return "succeeded"
}

// tailOf keeps the END of a log: a failure is at the bottom, and the top of a
// build is the part nobody needs.
func tailOf(s string, max int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= max {
		if s == "" {
			return "(no output)"
		}
		return s
	}
	cut := s[len(s)-max:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:]
	}
	return "…(truncated — see the log file)\n" + cut
}

func lastNewline(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' {
			return i
		}
	}
	return -1
}

func roundDur(d time.Duration) time.Duration { return d.Round(time.Second) }

// notifyAndWake is Notify plus the driver poke, which every background report
// needs and nothing else does.
func (h *Harness) notifyAndWake(text string) {
	h.Notify(text)
	select {
	case h.wake <- struct{}{}:
	default: // wake is buffered; a full buffer just means a turn is already due
	}
}

// notifyQuietly is what a REMINDER gets: the human sees it immediately, the
// model reads it whenever it next runs a turn anyway, and it never buys a turn
// of its own.
//
// The distinction matters because a heartbeat is not news. It reports the
// ABSENCE of news — "still running, still quiet" — and routing it through
// notifyAndWake made that absence cost a full turn: the wake ran continueTurn,
// which called the model, which (until this change) also bought a suggestion
// call. A session with one long job and nobody typing therefore billed two
// requests per heartbeat, on a schedule that starts at one minute, for saying
// that nothing had happened.
//
// Aside is the existing primitive for exactly this — see Harness.Aside: it can
// never CAUSE a turn, and it waits indefinitely if none comes, which is the
// correct fate for a reminder nobody needed. The onNotify ping is kept so the
// human still sees the line the moment it fires.
func (h *Harness) notifyQuietly(text string) {
	h.Aside(text)
	if cb := h.store.notifyCallback(); cb != nil {
		cb(text)
	}
}

// bgLogDir is where job logs live: beside the session file when there is one,
// so a resumed session's logs are still findable, else a temp dir.
func (h *Harness) bgLogDir() string {
	dir := filepath.Join(os.TempDir(), "dun-bg")
	if h.cfg.SessionFile != "" {
		dir = filepath.Join(filepath.Dir(h.cfg.SessionFile), "bg")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

// startBackground runs command asynchronously via backend (a container when
// DockerExec); on completion it injects a completion notification and wakes the
// driver. Returns the job.
func (h *Harness) startBackground(backend ExecBackend, command string) *bgJob {
	h.bgMu.Lock()
	h.bgSeq++
	id := h.bgSeq
	h.bgRun++
	if h.bgJobs == nil {
		h.bgJobs = map[int]*bgJob{}
	}
	j := &bgJob{id: id, command: command, started: time.Now(), h: h, hb: newHeartbeat()}
	h.bgJobs[id] = j
	h.bgMu.Unlock()

	if dir := h.bgLogDir(); dir != "" {
		p := filepath.Join(dir, fmt.Sprintf("job-%d.log", id))
		// A log we cannot open is not worth failing the job over — the output
		// still reaches the model, it is just not grep-able.
		if f, err := os.Create(p); err == nil {
			j.log, j.logPath = f, p
		}
	}
	// After the log is opened, so the row carries a path the human can grep.
	h.jobsChanged()
	go j.heartbeat()

	go func() {
		// Exempt from defaultExecTimeout: an unbounded run is the entire reason
		// to send a command here instead of running it in the foreground.
		res := backend.Run(WithoutExecTimeout(context.Background()), command, j)
		h.bgMu.Lock()
		h.bgRun--
		h.bgMu.Unlock()
		note := j.finished(res)
		// Pushed even for a MUTED job: `ignore` silences the model, not the
		// human, and a row stuck on "running" forever is the bug the pane
		// exists to stop.
		h.jobsChanged()
		if note != "" {
			j.notify(note)
		}
	}()
	return j
}

// heartbeat says "still running" on the bgHeartbeats schedule until the job
// finishes. One goroutine per job, which exits with it.
//
// A MUTED job stays silent here too: `ignore` means the model stopped caring
// about this job's outcome, and a heartbeat is precisely the outcome-adjacent
// noise it asked not to hear. The human still sees the row in the pane, which
// is where a muted job is supposed to remain visible.
func (j *bgJob) heartbeat() {
	if j.hb == nil {
		return
	}
	j.hb.run(
		func() bool {
			j.mu.Lock()
			defer j.mu.Unlock()
			return j.done
		},
		func(quiet time.Duration) {
			j.mu.Lock()
			muted, written := j.mon.Ignore, j.written
			j.mu.Unlock()
			// A muted job stays silent here too: `ignore` means the model stopped
			// caring about this job's outcome, and a reminder is exactly that
			// noise. The human still sees the row in the pane.
			if muted {
				return
			}
			out := "no new output"
			if written > 0 {
				out = fmt.Sprintf("%d bytes of output, none new", written)
			}
			// notifyQuietly, NOT j.notify: the reminder must not reset the
			// silence it is reporting on, and must not buy a turn to report it
			// — a job being quiet is not a reason to call the model.
			j.h.notifyQuietly(fmt.Sprintf(
				"exec #%d is STILL RUNNING, quiet for %s — `%s` (%s).\n%s\n"+
					"Nothing is necessarily wrong; this is so a wedged job does not look like a working one. "+
					"exec_monitor(job:%d) to see what it has produced.",
				j.id, roundDur(quiet), j.command, out, j.logLine(), j.id))
		},
	)
}

// JobInfo is one background job as the UI sees it. The counterpart of
// AgentInfo, and deliberately the same shape of thing: the two together are the
// session's activity, and the pane that shows them does not care which is which.
//
// Times are UNIX SECONDS, not a pre-computed elapsed. A job is pushed on start
// and on finish and not in between — pushing per output chunk would fire on
// every write of a build log — so a duration computed at push time would sit
// frozen on screen for the whole run. The UI ticks its own clock against these.
type JobInfo struct {
	ID      int    `json:"id"`
	Command string `json:"command"`
	Log     string `json:"log,omitempty"`
	State   string `json:"state"` // running · ok · failed · timeout · error
	Bytes   int64  `json:"bytes"`
	Started int64  `json:"started"`
	Ended   int64  `json:"ended,omitempty"` // 0 while running
	Code    int    `json:"code,omitempty"`
	// Muted is exec_monitor's `ignore` — a job whose completion the model asked
	// not to hear about. Shown because a silenced job is a choice, and an
	// invisible silenced job is indistinguishable from a lost one.
	Muted bool `json:"muted,omitempty"`
}

// info snapshots a job under its lock.
func (j *bgJob) info() JobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := JobInfo{
		ID: j.id, Command: j.command, Log: j.logPath, State: "running",
		Bytes: j.written, Started: j.started.Unix(), Muted: j.mon.Ignore,
	}
	if j.done {
		out.State, out.Code, out.Ended = jobState(j.res), j.res.Code, j.ended.Unix()
	}
	return out
}

// jobState is outcome() as a token rather than a sentence — the same verdict,
// but one the UI can switch on instead of matching prose.
func jobState(r ExecResult) string {
	switch {
	case r.TimedOut:
		return "timeout"
	case r.Err != "":
		return "error"
	case r.Code != 0:
		return "failed"
	}
	return "ok"
}

// Jobs is every background job this session has started, oldest first.
func (h *Harness) Jobs() []JobInfo {
	list := h.bgJobList()
	out := make([]JobInfo, 0, len(list))
	for _, j := range list {
		out = append(out, j.info())
	}
	return out
}

// jobsChanged pushes the job list to the UI, on the same terms as
// agentsChanged: every transition a human would want to see. Start and finish
// are the only ones — progress is a byte count, and a pane that repainted on
// every chunk of a build log would be a firehose, not a monitor.
func (h *Harness) jobsChanged() {
	if h == nil || h.cfg.OnJobs == nil {
		return
	}
	h.cfg.OnJobs(h.Jobs())
}

// spillExec saves a foreground result too large to hand the model whole, and
// returns the path. Beside the background job logs, because it is the same kind
// of thing: output that exists, is wanted occasionally, and must not be paid
// for on every turn.
func (h *Harness) spillExec(command, output string) string {
	dir := h.bgLogDir()
	if dir == "" {
		return ""
	}
	h.bgMu.Lock()
	h.bgSeq++
	n := h.bgSeq
	h.bgMu.Unlock()

	path := filepath.Join(dir, fmt.Sprintf("exec-%d.out", n))
	body := fmt.Sprintf("$ %s\n\n%s", command, output)
	if os.WriteFile(path, []byte(body), 0o600) != nil {
		return ""
	}
	return path
}

// bgJobByID looks a job up for the monitor tool.
func (h *Harness) bgJobByID(id int) *bgJob {
	h.bgMu.Lock()
	defer h.bgMu.Unlock()
	return h.bgJobs[id]
}

// bgJobList is every job this session has started, oldest first.
func (h *Harness) bgJobList() []*bgJob {
	h.bgMu.Lock()
	defer h.bgMu.Unlock()
	out := make([]*bgJob, 0, len(h.bgJobs))
	for _, j := range h.bgJobs {
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].id < out[b].id })
	return out
}

// execMonitorToolDef is how the model tunes the chatter it gets from a job.
func execMonitorToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "exec_monitor"
	td.Function.Description = "Check on background jobs (exec with background:true). With no `job`, lists them all. " +
		"With one, reports its state and hands back whatever it has produced since the last report, and can " +
		"change how much it tells you while it runs: `buffer_bytes` reports once that many bytes have piled up " +
		"(0 = silent until it exits), `grep` limits reports to matching lines, `ignore` mutes the job entirely " +
		"including its completion. Every job also writes a log file — grep THAT with exec rather than asking " +
		"for a big log here."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"job":          map[string]any{"type": "integer", "description": "job id; omit to list every job"},
			"buffer_bytes": map[string]any{"type": "integer", "description": "report progress once this many matching bytes accumulate (0 = never)"},
			"grep":         map[string]any{"type": "string", "description": "only report lines matching this regexp (empty string clears it)"},
			"ignore":       map[string]any{"type": "boolean", "description": "mute this job completely, including its completion notice"},
		},
	}
	return td
}

func withExecMonitor(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "exec_monitor" {
			return inner(ctx, tc)
		}
		var args struct {
			Job         *int    `json:"job"`
			BufferBytes *int    `json:"buffer_bytes"`
			Grep        *string `json:"grep"`
			Ignore      *bool   `json:"ignore"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		out := execMonitor(h, args.Job, args.BufferBytes, args.Grep, args.Ignore)
		if onCall != nil {
			a := map[string]any{}
			if args.Job != nil {
				a["job"] = *args.Job
			}
			onCall("exec_monitor", a, out)
		}
		return out, nil
	}
}

func execMonitor(h *Harness, id, bufferBytes *int, grep *string, ignore *bool) string {
	if id == nil {
		jobs := h.bgJobList()
		if len(jobs) == 0 {
			return "No background jobs. Start one with exec(background:true)."
		}
		lines := make([]string, 0, len(jobs))
		for _, j := range jobs {
			lines = append(lines, j.status())
		}
		return strings.Join(lines, "\n")
	}
	j := h.bgJobByID(*id)
	if j == nil {
		return fmt.Sprintf("ERROR: no background job #%d.", *id)
	}

	j.mu.Lock()
	if bufferBytes != nil {
		j.mon.BufferBytes = *bufferBytes
	}
	if ignore != nil {
		j.mon.Ignore = *ignore
	}
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
	tuned := bufferBytes != nil || ignore != nil || grep != nil
	mon, done, res, logPath := j.mon, j.done, j.res, j.logPath
	// Hand back whatever has accumulated, whether or not it met the threshold —
	// an explicit ask is not the same as a scheduled report.
	pending := string(j.pending)
	j.pending = nil
	j.mu.Unlock()
	if tuned {
		h.jobsChanged() // muting/unmuting changes what the row says about itself
	}

	var b strings.Builder
	b.WriteString(j.status())
	b.WriteString("\n" + describeMonitor(mon))
	if logPath != "" {
		b.WriteString("\nlog: " + logPath)
	}
	switch {
	case done:
		b.WriteString("\n" + tailOf(res.Output, bgTailBytes))
	case pending != "":
		b.WriteString("\nsince the last report:\n" + strings.TrimRight(pending, "\n"))
	default:
		b.WriteString("\n(nothing new since the last report)")
	}
	return b.String()
}

func describeMonitor(m bgMonitor) string {
	if m.Ignore {
		return "monitor: MUTED (no progress, no completion notice)"
	}
	parts := []string{"completion notice on"}
	if m.BufferBytes > 0 {
		parts = append(parts, fmt.Sprintf("progress every %d bytes", m.BufferBytes))
	} else {
		parts = append(parts, "no progress reports")
	}
	if m.Grep != "" {
		parts = append(parts, "matching /"+m.Grep+"/")
	}
	return "monitor: " + strings.Join(parts, ", ")
}
