package dun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Isolation, tier 2 — the exec tool.
//
// None of the three MCP servers runs arbitrary commands (mcpshell is sandboxed;
// poly-lsp only gives diagnostics), but a coding agent must build/test/git. exec
// is that command-runner. The DANGEROUS part — running model-authored commands —
// is contained by a Docker container (DockerExec); HostExec is the escape hatch
// for a trusted/throwaway environment. Either way the command runs against the
// session's worktree, so it sees the agent's edits.

// ExecBackend runs a shell command against the workspace.
//
// w, when non-nil, receives combined stdout+stderr AS IT ARRIVES — that is the
// only way a caller can watch a command that has not finished (see bgjob.go).
// The same bytes are always in the returned ExecResult; w is a tee, not a
// replacement.
type ExecBackend interface {
	Run(ctx context.Context, command string, w io.Writer) ExecResult
}

// ExecResult is one command's outcome. It exists because the outcome used to be
// a STRING, and "did it fail?" was therefore `strings.Contains(out, "[exit:")`
// — which a command that merely PRINTS that marker turns into a false failure,
// and which reads as a silent PASS anywhere the marker gets lost. The exit code
// is the fact; the marker is now only a rendering of it.
type ExecResult struct {
	// Output is combined stdout+stderr, verbatim.
	Output string
	// Code is the process exit status. 0 is success; -1 means it never ran, or
	// was killed by a signal (a timeout is the usual reason).
	Code int
	// TimedOut is set when the deadline killed it — distinct from Code == -1,
	// which a command can also reach by being killed for other reasons.
	TimedOut bool
	// Limit is the deadline in force, for reporting. 0 = unbounded.
	Limit time.Duration
	// Err carries a spawn failure (binary missing, bad cwd) that has no exit
	// status of its own.
	Err string
}

// Failed is the question every caller actually asks.
func (r ExecResult) Failed() bool { return r.TimedOut || r.Code != 0 || r.Err != "" }

// Render is the model-facing form: the output, plus an "[exit: …]" line when
// something went wrong. The marker stays because the model reads it, not
// because anything parses it any more.
func (r ExecResult) Render() string {
	if !r.Failed() {
		return r.Output
	}
	s := r.Output
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	switch {
	case r.TimedOut:
		s += fmt.Sprintf("[exit: TIMED OUT after %s and was killed. Output above is partial. "+
			"If this command legitimately needs longer, run it with background:true; if it was "+
			"waiting for input, it will never get any — do not retry it as-is.]", r.Limit)
	case r.Err != "":
		s += fmt.Sprintf("[exit: %s]", r.Err)
	default:
		s += fmt.Sprintf("[exit: status %d]", r.Code)
	}
	return s
}

// execInlineMax is how much of a foreground result reaches the model.
//
// Background jobs have had this since the day they were written: stream to a
// file, hand the model a bounded tail and the path. The foreground path never
// did, and one `cat` of a 252 KB log put 255,720 characters — about 64k tokens
// — into the window, to answer a question whose answer was "30". Every turn
// after that paid for it again.
//
// The cap is generous on purpose: the model ASKED for this output, so the
// budget is "enough to work with" rather than "enough to notice something went
// wrong". What does not fit is still on disk and grep-able, which is the same
// bargain exec_monitor already offers for a long build.
const execInlineMax = 20000

// execHeadShare is how much of the budget goes to the START of the output. A
// failure is at the bottom — that is why tailOf keeps the end — but a listing,
// a file, or a help text is useful from the top, and the foreground path sees
// both. So both ends survive and the middle is what goes.
const execHeadShare = 3

// capExecOutput bounds one foreground result, spilling the whole thing to a
// file the model can grep. Applied AFTER Render, so the "[exit: …]" verdict —
// which lives at the end — is inside the tail that is kept.
func capExecOutput(out, command string, spill func(command, output string) string) string {
	if len(out) <= execInlineMax {
		return out
	}
	head := execInlineMax / execHeadShare
	tail := execInlineMax - head
	// Cut on line boundaries: half a line is not something the model can act on,
	// and it reads as corruption rather than as truncation.
	h := out[:head]
	if i := strings.LastIndexByte(h, '\n'); i > 0 {
		h = h[:i+1]
	}
	t := out[len(out)-tail:]
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:]
	}
	elided := len(out) - len(h) - len(t)

	where := "the full output was not saved"
	if spill != nil {
		if path := spill(command, out); path != "" {
			where = "full output: " + path + " — grep it with exec rather than asking for it whole"
		}
	}
	return fmt.Sprintf("%s\n…[%d characters elided — %s]…\n%s", h, elided, where, t)
}

// defaultExecTimeout bounds a FOREGROUND exec. Nothing the model runs in the
// foreground is worth blocking the session indefinitely: a command that waits
// on input it can never receive (`gh auth login` polls a device flow for 15
// minutes with stdin on /dev/null) looks identical, from dun's side, to a
// command doing slow work. Measured: one such call held a session for 14
// minutes before the remote gave up.
//
// It is a deadline, not a heuristic — the model is told to move long work to
// background:true, which is exempt.
const defaultExecTimeout = 5 * time.Minute

// noTimeoutKey marks a context as exempt from defaultExecTimeout.
type noTimeoutKey struct{}

// WithoutExecTimeout exempts ctx from the foreground deadline. Background jobs
// are the only caller: the whole point of background:true is the long build,
// and it blocks nothing while it runs.
func WithoutExecTimeout(ctx context.Context) context.Context {
	return context.WithValue(ctx, noTimeoutKey{}, true)
}

// bound applies defaultExecTimeout, and returns the limit in force so a kill
// can be reported as a timeout rather than as a bare signal.
//
// A caller that already set a deadline keeps it — ship's checks carry
// ship.checkTimeout, which is a deliberate per-repo number and must not be
// silently shortened to five minutes.
func bound(ctx context.Context) (context.Context, context.CancelFunc, time.Duration) {
	if d, ok := ctx.Deadline(); ok {
		return ctx, func() {}, time.Until(d)
	}
	if exempt, _ := ctx.Value(noTimeoutKey{}).(bool); exempt {
		return ctx, func() {}, 0
	}
	c, cancel := context.WithTimeout(ctx, defaultExecTimeout)
	return c, cancel, defaultExecTimeout
}

// shellFlags is how every command is run: NON-LOGIN and NON-INTERACTIVE.
//
// It used to be `-lc`. A login shell sources the operator's profile, which
// means the agent's commands inherit whatever that human's dotfiles happen to
// set — and one of those was EDITOR=vim, which is how `git rebase --continue`
// came to sit on a vim prompt for ten minutes inside `go test`. The agent is
// not that person's interactive session: it must not pick up their aliases,
// their prompt hooks, their PATH edits, or their editor, because none of it is
// reproducible on another machine and all of it can block.
//
// The cost is real and accepted: a tool the operator installed only via their
// profile will not be found. That is the correct failure — it says so, once,
// instead of behaving differently for every user.
const shellFlags = "-c"

// HostExec runs commands on the host, in dir. Use only for a trusted or
// throwaway workspace — there is no sandbox.
type HostExec struct{ Dir string }

func (h HostExec) Run(ctx context.Context, command string, w io.Writer) ExecResult {
	ctx, cancel, limit := bound(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", shellFlags, command)
	cmd.Dir = h.Dir
	// Model-authored commands run git, ssh and friends. None of them may reach
	// dun's terminal — see detach.go. killGroup is also what makes the deadline
	// bite: it kills the whole process group, not just the `sh` that spawned it.
	killGroup(detach(cmd))
	return finish(ctx, limit, w, cmd)
}

// DockerExec runs each command in a fresh container of Image with Dir mounted at
// /work (the cwd). The container is the sandbox: model-authored commands can't
// touch the host, only the mounted worktree.
type DockerExec struct {
	Dir   string
	Image string
	// Network, if false, runs with --network none (no egress). Default false.
	Network bool
	// ExtraMounts are additional host paths mounted read-only inside the
	// container at /<name>. These are how external dependencies (e.g. a
	// sibling module referenced by go.mod replace) become accessible inside
	// the container.
	ExtraMounts []MountSpec
}

// runArgs builds the `docker run` argv for one command in a container called
// name.
func (d DockerExec) runArgs(name, command string) []string {
	// --name is what makes the run addressable by `docker stop`. Killing the
	// `docker run` CLIENT does not stop what it started: the container runs to
	// completion and only then does --rm remove it. Survivable when the only
	// canceller was session teardown; with a 5m deadline on every foreground
	// exec it is a routine leak — a timed-out `go test` would keep burning the
	// machine's cores long after dun reported it killed.
	args := []string{"run", "--rm", "--name", name, "-v", d.Dir + ":/work", "-w", "/work"}
	if !d.Network {
		args = append(args, "--network", "none")
	}
	// Mount extra paths read-only at /<name>.
	for _, m := range d.ExtraMounts {
		args = append(args, "-v", m.Source+":/"+m.Name+":ro")
	}
	return append(args, d.Image, "sh", shellFlags, command)
}

func (d DockerExec) Run(ctx context.Context, command string, w io.Writer) ExecResult {
	ctx, cancel, limit := bound(ctx)
	defer cancel()
	name := containerName()
	cmd := killGroup(detach(exec.CommandContext(ctx, "docker", d.runArgs(name, command)...)))
	// Stop the CONTAINER on cancel, then let killGroup take the client. Best
	// effort and deliberately unchecked: if the daemon is gone or the container
	// already exited, there is nothing to report and nothing to do about it.
	kill := cmd.Cancel
	cmd.Cancel = func() error {
		_ = detach(dockerStop(name)).Run()
		return kill()
	}
	return finish(ctx, limit, w, cmd)
}

// containerName makes a run addressable by `docker stop`. Uniqueness only has
// to hold among live containers, so pid + a counter is enough — two dun
// processes cannot collide, and one dun cannot collide with itself.
var containerSeq atomic.Int64

func containerName() string {
	return fmt.Sprintf("dun-%d-%d", os.Getpid(), containerSeq.Add(1))
}

// dockerStopGrace is how long the container gets to exit on SIGTERM before
// docker kills it. Short on purpose: this path only runs when the command has
// ALREADY overrun its deadline, so a slow, polite shutdown is just more waiting.
const dockerStopGrace = "2"

func dockerStop(name string) *exec.Cmd {
	return exec.Command("docker", "stop", "--time", dockerStopGrace, name)
}

// finish runs cmd, capturing combined output and teeing it to w as it arrives.
//
// Stdout and Stderr are set to the SAME writer value on purpose: os/exec gives
// both streams one pipe (and one copying goroutine) when the two are interface-
// equal, which is what makes the interleaving faithful and race-free. Assigning
// two separate-but-equivalent writers would silently get two goroutines racing
// on the buffer.
func finish(ctx context.Context, limit time.Duration, w io.Writer, cmd *exec.Cmd) ExecResult {
	var buf bytes.Buffer
	sink := io.Writer(&buf)
	if w != nil {
		sink = io.MultiWriter(&buf, w)
	}
	cmd.Stdout, cmd.Stderr = sink, sink
	err := cmd.Run()

	res := ExecResult{Output: buf.String(), Limit: limit}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Code = ee.ExitCode()
		} else {
			// No exit status at all: the binary was missing, the cwd was bad,
			// or Wait itself failed. -1 with the reason kept.
			res.Code, res.Err = -1, err.Error()
		}
	}
	return res
}

// execToolDef is the tool the model calls to run commands.
func execToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "exec"
	td.Function.Description = "Run a shell command (build, test, git, ls, …) in the workspace. " +
		"Returns combined stdout+stderr; a non-zero exit is shown as [exit: …]. Use this to " +
		"verify edits (build/test) and to run git. Foreground commands are KILLED after " +
		defaultExecTimeout.String() + ", so never run anything interactive (it has no terminal and no " +
		"input — it will just hang until killed). For a LONG command (the full test suite, a " +
		"build), set background:true and keep working — background jobs have no time limit and " +
		"you'll get a notification when one finishes."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":    map[string]any{"type": "string", "description": "the shell command to run"},
			"background": map[string]any{"type": "boolean", "description": "run asynchronously; get notified when it finishes"},
		},
		"required": []string{"command"},
	}
	return td
}

// withExec wraps a dispatcher so the built-in "exec" tool is handled locally:
// synchronous by default, or async via startBg when background:true (its
// completion arrives later as a notification). Everything else routes to MCP.
func withExec(inner agent.ToolDispatcher, backend ExecBackend, onCall func(string, map[string]any, string), startBg func(command string) *bgJob, spill func(command, output string) string) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "exec" {
			return inner(ctx, tc)
		}
		var args struct {
			Command    string `json:"command"`
			Background bool   `json:"background"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		if strings.TrimSpace(args.Command) == "" {
			return "ERROR: exec requires a non-empty command", nil
		}
		if args.Background && startBg != nil {
			j := startBg(args.Command)
			res := fmt.Sprintf("Started background job #%d: `%s`. It has no time limit and runs in "+
				"the sandbox; you'll be notified when it finishes. Continue with other work in the "+
				"meantime.\n%s\nIt is SILENT until then — call exec_monitor(job:%d) to check on it, "+
				"or to ask for progress while it runs.", j.id, args.Command, j.logLine(), j.id)
			if onCall != nil {
				onCall("exec", map[string]any{"command": args.Command, "background": true}, res)
			}
			return res, nil
		}
		out := capExecOutput(backend.Run(ctx, args.Command, nil).Render(), args.Command, spill)
		if onCall != nil {
			onCall("exec", map[string]any{"command": args.Command}, out)
		}
		return out, nil
	}
}
