package dun

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// envInt reads an integer from the environment, falling back to fallback.
func envInt(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	_, err := strconv.Atoi(fallback)
	if err == nil {
		return fallback
	}
	return "0"
}

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
	// was killed by a signal.
	Code int
	// Err carries a spawn failure (binary missing, bad cwd) that has no exit
	// status of its own.
	Err string
}

// Failed is the question every caller actually asks.
func (r ExecResult) Failed() bool { return r.Code != 0 || r.Err != "" }

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
	//
	// Output with no newlines in reach — a minified JSON blob, a base64 payload,
	// one enormous line — falls back to a byte cut, which must still land on a
	// RUNE boundary or the result is invalid UTF-8: corruption of a different
	// kind, and one that can break the transport rather than merely read badly.
	h := safeCut(out[:head])
	if i := strings.LastIndexByte(h, '\n'); i > 0 {
		h = h[:i+1]
	}
	t := safeCutFront(out[len(out)-tail:])
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:]
	}
	elided := len(out) - len(h) - len(t)

	where := "the rest was not saved"
	if spill != nil {
		if ref := spill(command, out); ref != "" {
			// A REF, not a path: under --docker the file is on the host and the
			// model's container cannot open it, so a path was never usable.
			//
			// And advise only what can WORK here. On output with no lines to
			// speak of, grep/head/tail all hand back the single line that was
			// the problem, so suggesting them sends the model down a road that
			// dead-ends; paging is the only way in.
			if longestLine(out) > execInlineMax {
				where = fmt.Sprintf("ref %q — ONE long line, so grep/head/tail cannot help: page it with "+
					"recap({ref:%q, at:0}), then the offset each page reports", ref, ref)
			} else {
				where = fmt.Sprintf("ref %q — read it with recap({ref:%q, grep:\"…\"}), or head/tail/full/at", ref, ref)
			}
		}
	}
	return fmt.Sprintf("%s\n…[%d characters elided — %s]…\n%s", h, elided, where, t)
}

// longestLine is how the clip decides whether line-based advice is honest.
func longestLine(s string) int {
	longest, start := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i-start > longest {
				longest = i - start
			}
			start = i + 1
		}
	}
	if len(s)-start > longest {
		longest = len(s) - start
	}
	return longest
}

// safeCut trims a trailing partial rune.
func safeCut(s string) string {
	for len(s) > 0 && !utf8.ValidString(s[len(s)-1:]) {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// safeCutFront trims a leading partial rune.
func safeCutFront(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			break
		}
		s = s[1:]
	}
	return s
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
	cmd := exec.CommandContext(ctx, "sh", shellFlags, command)
	cmd.Dir = h.Dir
	// Model-authored commands run git, ssh and friends. None of them may reach
	// dun's terminal — see detach.go. killGroup is also what makes the deadline
	// bite: it kills the whole process group, not just the `sh` that spawned it.
	killGroup(detach(cmd))
	return finish(ctx, w, cmd)
}

// HostShell is a persistent-shell exec backend: ONE long-lived shell process
// that every command is fed through, so shell state — a variable you `export`,
// an alias, a sourced file — survives from one command to the next, the way it
// does in a human's terminal.
//
// Why a persistent shell rather than "re-inject the remembered vars into each
// command": the agent can create state we cannot predict — `export X=…`,
// `source .env`, an alias. A persistent shell keeps all of it in its own
// memory without us having to parse every command the model writes.
//
// The one thing we DO control is the working directory. The model is told each
// call starts in the project root, so before every command we `cd` back to
// Dir. A `cd` the model runs is honored for the rest of THAT command only; the
// next command starts clean. That is the reset the user asked for — no `cd`
// bookkeeping in the model's head — while exports carry forward.
//
// Protocol: we write the command as a script, then a unique sentinel line. The
// last line of the script is `echo SENTINEL $?`, so the shell prints the
// sentinel followed by the command's exit code. A pump goroutine captures
// stdout+stderr into a line buffer; readUntil reads until it sees the sentinel
// and splits the command's output from the marker.
//
// One command at a time: the shell is one process with one stdin, so calls
// serialize on mu. A command that outlives its caller's patience does NOT hold
// that lock forever — its shell is DONATED to it (see RunPromotable) and the
// next call gets a fresh one. That is what makes a long command a background
// job rather than a stalled session.
type HostShell struct {
	Dir string

	mu  sync.Mutex
	p   *shellProc
	seq int
}

// shellProc is ONE live `sh` process: the pipes we talk to it through, the
// buffer its output lands in, and how we learn it died. They are a struct
// rather than fields on HostShell because donation moves all of them at once.
type shellProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	buf    *shLineBuf
	dead   chan struct{}    // closed when the process exits
	pumped chan struct{}    // closed when its output pipe reaches EOF
	state  *os.ProcessState // written BEFORE dead closes; read only after
}

const shSentinelPrefix = "DUN_EXEC_END_"

// pumpDrainGrace bounds how long a dying shell's output is waited for. EOF
// normally arrives the instant the process goes, but a grandchild holding the
// pipe open can delay it indefinitely, and a lost tail is a better outcome than
// a hang.
const pumpDrainGrace = 500 * time.Millisecond

// PromotableExec is an ExecBackend that can hand a running command the shell it
// occupies and step out of its way. Only the persistent shell needs this: a
// stateless backend gives every command its own process already, so there is
// nothing to donate and nothing queued behind it.
type PromotableExec interface {
	RunPromotable(ctx context.Context, command string, w io.Writer, promote <-chan struct{}) ExecResult
}

// Run feeds one command through the persistent shell, starting in Dir, and
// returns its combined output plus exit code.
func (h *HostShell) Run(ctx context.Context, command string, w io.Writer) ExecResult {
	return h.RunPromotable(ctx, command, w, nil)
}

// RunPromotable is Run with an escape hatch. When promote closes, the shell
// this command occupies is given away to it: HostShell drops its reference, the
// lock is released, and the next call starts a fresh shell. The command is not
// killed and not restarted — it keeps running in the shell it already has, its
// output keeps arriving, and that shell dies with it.
//
// A nil promote channel never fires, which is exactly plain Run.
func (h *HostShell) RunPromotable(ctx context.Context, command string, w io.Writer, promote <-chan struct{}) ExecResult {
	h.mu.Lock()
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			h.mu.Unlock()
		}
	}
	defer unlock()

	if h.p == nil || h.p.exited() {
		if err := h.start(); err != nil {
			return ExecResult{Code: -1, Err: "persistent shell: " + err.Error()}
		}
	}

	h.seq++
	sentinel := fmt.Sprintf("%s%d", shSentinelPrefix, h.seq)
	// cd back to the project root (the reset), run the command, then mark the
	// end with the exit code. $? is the command's status because it is the
	// immediately preceding command.
	script := "cd " + shellQuote(h.Dir) + " 2>/dev/null\n" +
		command + "\n" +
		"echo " + sentinel + " $?\n\n"
	if _, err := h.p.stdin.Write([]byte(script)); err != nil {
		// The shell died between calls. Start a fresh one and retry once.
		h.teardown()
		if err := h.start(); err != nil {
			return ExecResult{Code: -1, Err: "persistent shell: " + err.Error()}
		}
		if _, err := h.p.stdin.Write([]byte(script)); err != nil {
			return ExecResult{Code: -1, Err: "persistent shell: " + err.Error()}
		}
	}

	p := h.p
	res, donated := p.readUntil(ctx, w, sentinel, promote, func() {
		h.p = nil
		unlock()
	})
	if donated {
		// The donated shell existed only for this command; nothing else can
		// reach it, so it goes when the command does.
		p.kill()
		return res
	}
	if p.exited() {
		h.teardown()
	}
	return res
}

// readUntil pulls output until the sentinel line appears, returning everything
// before it plus the exit code the sentinel carries.
//
// It also watches the three things the sentinel will never arrive after:
// cancellation, promotion, and the shell dying underneath the command. That
// last one is not an error — `exit 7` and `kill $$` are commands, and the
// shell's own exit status IS the command's result. Missing it is what used to
// spin this loop forever.
//
// The bool reports whether the shell was donated; the caller owns it after.
func (p *shellProc) readUntil(ctx context.Context, w io.Writer, sentinel string, promote <-chan struct{}, onPromote func()) (ExecResult, bool) {
	var buf bytes.Buffer
	sink := io.Writer(&buf)
	if w != nil {
		sink = io.MultiWriter(&buf, w)
	}
	donated := false
	for {
		if line, ok := p.buf.popLine(); ok {
			if code, hit := splitSentinel(sink, line, sentinel); hit {
				return ExecResult{Output: buf.String(), Code: code}, donated
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ExecResult{Output: buf.String(), Code: -1, Err: "canceled"}, donated

		case <-promote:
			// A closed channel is always ready, so drop it after one firing.
			promote, donated = nil, true
			if onPromote != nil {
				onPromote()
			}

		case <-p.dead:
			// Let the pump reach EOF first, or the tail of the output is lost.
			select {
			case <-p.pumped:
			case <-time.After(pumpDrainGrace):
			}
			for {
				line, ok := p.buf.popLine()
				if !ok {
					break
				}
				if code, hit := splitSentinel(sink, line, sentinel); hit {
					return ExecResult{Output: buf.String(), Code: code}, donated
				}
			}
			code := -1
			if p.state != nil {
				code = p.state.ExitCode()
			}
			return ExecResult{Output: buf.String(), Code: code}, donated

		case <-time.After(20 * time.Millisecond):
		}
	}
}

// splitSentinel writes line to sink unless it carries the sentinel, in which
// case it writes only the part before the marker and reports the exit code.
func splitSentinel(sink io.Writer, line, sentinel string) (int, bool) {
	i := strings.Index(line, sentinel)
	if i < 0 {
		fmt.Fprintln(sink, line)
		return 0, false
	}
	if pre := line[:i]; pre != "" {
		fmt.Fprintln(sink, pre)
	}
	code := 0
	if rest := strings.TrimSpace(line[i+len(sentinel):]); rest != "" {
		if c, err := strconv.Atoi(rest); err == nil {
			code = c
		}
	}
	return code, true
}

// start spawns the persistent shell: a plain `sh` reading commands from stdin.
// Plain `sh` (no -c, no -i, no -l) gives the same reproducible, profile-free
// environment contract as HostExec's `sh -c`, but stays alive across calls.
//
// The shell must OUTLIVE any single call's context, so it is NOT created with
// CommandContext and carries no killGroup Cancel. It is detached so it can
// never race the TUI for the terminal, and killed explicitly.
//
// stdout and stderr are the SAME pipe write end, so the kernel interleaves them
// in the order they were actually written — one pipe, one pump, no cross-stream
// reordering. The pipe is ours rather than StdoutPipe's on purpose: Wait closes
// the pipes IT created, and doing that under a still-reading pump is precisely
// how a command's last lines go missing.
//
// Caller must hold h.mu.
func (h *HostShell) start() error {
	h.teardown()
	cmd := exec.Command("sh")
	cmd.Dir = h.Dir
	detach(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = pr.Close()
		_ = pw.Close()
		return err
	}
	// Drop the parent's copy of the write end, or the pump never sees EOF.
	_ = pw.Close()

	p := &shellProc{
		cmd: cmd, stdin: stdin, buf: newShLineBuf(),
		dead: make(chan struct{}), pumped: make(chan struct{}),
	}
	go func() {
		p.buf.pump(pr)
		_ = pr.Close()
		close(p.pumped)
	}()
	// Reap the shell when it exits — `exit 7` ends it, and without a Wait it
	// would linger as a zombie whose status nobody can read.
	//
	// state is stored BEFORE dead closes, and dead closes BEFORE any lock is
	// taken. Both orderings are load-bearing: readUntil observes the close and
	// then reads state, and a waiter that took h.mu first would deadlock
	// against the RunPromotable call that holds it for the whole command.
	go func() {
		_ = cmd.Wait()
		p.state = cmd.ProcessState
		close(p.dead)
	}()
	h.p = p
	return nil
}

// exited reports whether the shell process is gone. Safe without h.mu: dead is
// only ever closed, never reassigned, once the shellProc is published.
func (p *shellProc) exited() bool {
	select {
	case <-p.dead:
		return true
	default:
		return false
	}
}

// kill takes down the shell and everything it spawned. Setsid (detach) makes
// the shell its own group leader, so -pid reaches the command it was running —
// killing only the `sh` would orphan that.
func (p *shellProc) kill() {
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	// The waiter goroutine owns Wait. Wait for IT, bounded, rather than calling
	// Wait a second time.
	select {
	case <-p.dead:
	case <-time.After(killGroupDelay):
	}
}

// Close kills the shell this backend owns. Rehoist swaps the exec backend
// wholesale (host ↔ docker, or into a worktree), and the shell the outgoing one
// held would otherwise sit there for the life of the session.
func (h *HostShell) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.teardown()
}

// teardown kills the current shell and forgets it. Caller must hold h.mu.
func (h *HostShell) teardown() {
	if h.p != nil {
		h.p.kill()
		h.p = nil
	}
}

// shellQuote wraps a path in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shLineBuf is a thread-safe, line-oriented FIFO fed by two goroutines (the
// shell's stdout and stderr). It is bounded so a runaway command cannot grow
// memory without bound; Run drains it as it reads.
type shLineBuf struct {
	mu    sync.Mutex
	cond  *sync.Cond
	n     int
	lines []string
	full  bool
}

const shLineBufMax = 4096 // lines held before the pumpers block

func newShLineBuf() *shLineBuf {
	b := &shLineBuf{lines: make([]string, 0, 256)}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// pump reads lines from r and enqueues them. It blocks when the buffer is full
// (backpressure into the shell, which is fine — the shell simply waits for
// room, exactly like a full pipe would).
func (b *shLineBuf) pump(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		b.mu.Lock()
		for b.n >= shLineBufMax {
			b.full = true
			b.cond.Wait()
		}
		b.lines = append(b.lines, sc.Text())
		b.n++
		b.cond.Signal()
		b.mu.Unlock()
	}
}

// popLine returns the next line, or ("", false) if none is ready.
func (b *shLineBuf) popLine() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == 0 {
		return "", false
	}
	line := b.lines[0]
	b.lines = b.lines[1:]
	b.n--
	if b.full {
		b.full = false
		b.cond.Signal()
	}
	return line, true
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
	// --user matches the host UID:GID so files created in the container are
	// owned by the same user that runs the MCP servers on the host — without
	// it, exec creates root-owned files that node_edit can't overwrite.
	uid := envInt("UID", "0")
	gid := envInt("GID", "0")
	args := []string{
		"run", "--rm", "--name", name,
		"--user", uid + ":" + gid,
		"-v", d.Dir + ":/work", "-w", "/work",
	}
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
	return finish(ctx, w, cmd)
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
func finish(ctx context.Context, w io.Writer, cmd *exec.Cmd) ExecResult {
	var buf bytes.Buffer
	sink := io.Writer(&buf)
	if w != nil {
		sink = io.MultiWriter(&buf, w)
	}
	cmd.Stdout, cmd.Stderr = sink, sink
	err := cmd.Run()

	res := ExecResult{Output: buf.String()}
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

// execGrace is how long an exec call waits for a command before handing it to
// the background. Everything ordinary — a build, a focused test run, a git
// command — answers well inside this; anything that does not is by definition
// long enough to be worth watching asynchronously instead of blocking on.
//
// 30s because the failure it replaces was measured in TEN-MINUTE units: a
// wedged `go test` held a session silent for its full timeout, twice in one
// hour, while nothing in the loop could tell "working" from "stuck".
const execGrace = 30 * time.Second

// execToolDef is the tool the model calls to run commands.
func execToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "exec"
	td.Function.Description = "Run a shell command (build, test, git, ls, …) in the workspace's " +
		"project directory. Each call starts in the project root — you do not need to cd into it — " +
		"but a variable you `export` in one call persists into the next for this agent. Returns " +
		"combined stdout+stderr; a non-zero exit is shown as [exit: …]. Use this to verify edits " +
		"(build/test) and to run git.\n" +
		"A command still running after 30s is NOT killed and does NOT block you: it becomes a " +
		"background job and you get its number back immediately. From then on the job reports to " +
		"you — a notification when it finishes, and exec_monitor(job:N) whenever you want to see " +
		"what it has produced so far. Set `timeout` to wait longer or shorter before that handover, " +
		"or `background:true` to hand over at once for a command you already know is long. " +
		"Never run anything interactive: it has no terminal and no input, so it will sit there " +
		"doing nothing until the handover."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":    map[string]any{"type": "string", "description": "the shell command to run"},
			"timeout":    map[string]any{"type": "integer", "description": "seconds to wait for the command before it becomes a background job (default 30)"},
			"background": map[string]any{"type": "boolean", "description": "hand over to a background job immediately, without waiting"},
		},
		"required": []string{"command"},
	}
	return td
}

// withExec wraps a dispatcher so the built-in "exec" tool is handled locally.
//
// There is no foreground path any more. Every command starts as a job (see
// Harness.startJob) and the only question this asks is who reports it: if it
// finishes within its grace, the output comes back as an ordinary tool result
// and the job never surfaces; if it does not, the job is promoted — it takes
// over the shell, gets a number, and the model is told where to look.
//
// That is the whole point. A command can no longer hold the session: the worst
// case is a job the model was told about, not a turn that never ends.
func withExec(inner agent.ToolDispatcher, backend ExecBackend, onCall func(string, map[string]any, string), startJob func(command string, background bool) *bgJob, spill func(command, output string) string) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "exec" {
			return inner(ctx, tc)
		}
		var args struct {
			Command    string `json:"command"`
			Background bool   `json:"background"`
			Timeout    *int   `json:"timeout"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		if strings.TrimSpace(args.Command) == "" {
			return "ERROR: exec requires a non-empty command", nil
		}

		// NOT capped here: the cap is applied once, around every tool, in
		// withRecapWatch. Capping twice would spill the same output twice.
		if startJob == nil {
			// No job machinery (a bare dispatcher in a test): run it straight.
			out := backend.Run(ctx, args.Command, nil).Render()
			if onCall != nil {
				onCall("exec", map[string]any{"command": args.Command}, out)
			}
			return out, nil
		}

		grace := execGrace
		if args.Timeout != nil {
			grace = time.Duration(*args.Timeout) * time.Second
		}
		if args.Background || grace < 0 {
			grace = 0
		}

		j := startJob(args.Command, grace <= 0)
		if grace > 0 {
			t := time.NewTimer(grace)
			select {
			case <-j.doneCh:
			case <-t.C:
			case <-ctx.Done():
			}
			t.Stop()
		}
		res, done := j.settle()
		out := res.Render()
		if !done {
			out = j.handoffNotice(grace)
		}
		if onCall != nil {
			onCall("exec", map[string]any{"command": args.Command, "background": !done}, out)
		}
		return out, nil
	}
}
