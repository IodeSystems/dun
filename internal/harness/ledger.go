package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"

	"github.com/iodesystems/dun/internal/numfmt"
)

// ledger.go — the turn ledger (USER 2026-11-17): what each turn did, from
// its own tool events, with no model call. The facts of a turn go to the
// asks open during it (asks.go); a turn that served no ask and made a tool
// call is its own /journal entry (kind "turn").
//
// Recorded per tool call, at the outermost dispatcher (withLedger), for the
// root and its sub-agents alike (a child's calls carry its number):
//   - files: edit's "+N -M" (agentkit's own diff count); write and delete
//     count against the file as it was before the call (a line diff, exact);
//     move names both paths. Files a command changed are not seen.
//   - commands: exec's first line, exit, duration; one that outran its grace
//     is "job #N", and the job's end is recorded when it comes (bgjob.go).
//   - sub-agents started (spawnAgentFor) and finished (settleThen).
//   - tool errors: a failed call other than an exec that ran.
//
// Commits come from HEAD: each workspace's HEAD (the root's and each
// child's own tree) at the turn's start and end, and `git log` between
// (first parent, so a merge is one commit). Totals: the round stats of the
// turn (rounds, tokens, cost; sub-agents' included, as roundstats.go counts
// them), the tool calls, the wall time, and whether it was interrupted.
//
// Lists are capped (ledgerListMax); the totals count everything.

// ledgerListMax bounds each list a ledger keeps; the totals go on.
const ledgerListMax = 50

// LedgerFile is one file a turn changed.
type LedgerFile struct {
	Path    string `json:"path"`
	Agent   int    `json:"agent,omitempty"` // the sub-agent that changed it; 0 the main agent
	Added   int    `json:"added,omitempty"`
	Removed int    `json:"removed,omitempty"`
	Created bool   `json:"created,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	MovedTo string `json:"moved_to,omitempty"`
}

// LedgerCommit is one commit made during a turn.
type LedgerCommit struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
	Agent   int    `json:"agent,omitempty"`
}

// LedgerCmd is one exec call, or a background job's end (Job set, Ended).
type LedgerCmd struct {
	Command string `json:"command"`
	Agent   int    `json:"agent,omitempty"`
	// Exit is the status; Verdict says the rest when it is not a plain exit
	// ("killed", "did not run", "job #3 still running").
	Exit    int    `json:"exit"`
	Failed  bool   `json:"failed,omitempty"`
	Verdict string `json:"verdict,omitempty"`
	MS      int64  `json:"ms"`
	Job     int    `json:"job,omitempty"`
	Ended   bool   `json:"ended,omitempty"` // a background job that finished
}

// LedgerAgent is a sub-agent starting or finishing.
type LedgerAgent struct {
	N       int    `json:"n"`
	Title   string `json:"title,omitempty"`
	Event   string `json:"event"`             // started | finished
	Outcome string `json:"outcome,omitempty"` // done | failed | dismissed
}

// LedgerError is a failed tool call.
type LedgerError struct {
	Tool  string `json:"tool"`
	Line  string `json:"line"`
	Agent int    `json:"agent,omitempty"`
}

// LedgerFacts is what one or more turns did.
type LedgerFacts struct {
	Files   []LedgerFile   `json:"files,omitempty"`
	Commits []LedgerCommit `json:"commits,omitempty"`
	Cmds    []LedgerCmd    `json:"cmds,omitempty"`
	Agents  []LedgerAgent  `json:"agents,omitempty"`
	Errors  []LedgerError  `json:"errors,omitempty"`

	// The totals, past the lists' caps.
	NFiles   int `json:"n_files,omitempty"`
	Added    int `json:"added,omitempty"`
	Removed  int `json:"removed,omitempty"`
	NCommits int `json:"n_commits,omitempty"`
	NCmds    int `json:"n_cmds,omitempty"`
	NFailed  int `json:"n_failed,omitempty"`
	NJobs    int `json:"n_jobs,omitempty"`  // background jobs started
	NEnded   int `json:"n_ended,omitempty"` // background jobs that ended (in Cmds, Ended)
	NAgents  int `json:"n_agents,omitempty"`
	NErrors  int `json:"n_errors,omitempty"`

	Rounds     int     `json:"rounds,omitempty"`
	ToolCalls  int     `json:"tool_calls,omitempty"`
	Prompt     int     `json:"prompt,omitempty"`
	Cached     int     `json:"cached,omitempty"`
	Completion int     `json:"completion,omitempty"`
	Cost       float64 `json:"cost,omitempty"`
	Currency   string  `json:"cur,omitempty"`
	Costed     bool    `json:"costed,omitempty"`
	WallMS     int64   `json:"wall_ms,omitempty"`

	Interrupted bool   `json:"interrupted,omitempty"`
	Error       string `json:"error,omitempty"` // the turn's own error, first line
}

// add folds o into f: lists appended (a file merged by path and agent),
// totals summed. o's files past its cap are added to the totals only: a
// file one of them shares with f is then counted twice.
func (f *LedgerFacts) add(o LedgerFacts) {
	added, removed := 0, 0
	for _, x := range o.Files {
		f.addFile(x)
		added += x.Added
		removed += x.Removed
	}
	f.NFiles += max(0, o.NFiles-len(o.Files))
	f.Added += max(0, o.Added-added)
	f.Removed += max(0, o.Removed-removed)
	f.Commits = append(f.Commits, o.Commits...)
	f.Cmds = append(f.Cmds, o.Cmds...)
	f.Agents = append(f.Agents, o.Agents...)
	f.Errors = append(f.Errors, o.Errors...)
	f.NCommits += o.NCommits
	f.NCmds += o.NCmds
	f.NFailed += o.NFailed
	f.NJobs += o.NJobs
	f.NEnded += o.NEnded
	f.NAgents += o.NAgents
	f.NErrors += o.NErrors
	f.Rounds += o.Rounds
	f.ToolCalls += o.ToolCalls
	f.Prompt += o.Prompt
	f.Cached += o.Cached
	f.Completion += o.Completion
	if o.Costed {
		if !f.Costed {
			f.Currency = o.Currency
		}
		f.Cost += o.Cost
		f.Costed = true
	}
	f.WallMS += o.WallMS
	f.Interrupted = f.Interrupted || o.Interrupted
	if o.Error != "" {
		f.Error = o.Error
	}
}

// addFile merges one file change in by path and agent, and counts it.
func (f *LedgerFacts) addFile(x LedgerFile) {
	f.Added += x.Added
	f.Removed += x.Removed
	for i := range f.Files {
		e := &f.Files[i]
		if e.Path == x.Path && e.Agent == x.Agent {
			e.Added += x.Added
			e.Removed += x.Removed
			e.Created = e.Created || x.Created && !e.Deleted
			if x.Deleted {
				e.Deleted, e.Created = !e.Created, false
			}
			if x.MovedTo != "" {
				e.MovedTo = x.MovedTo
			}
			return
		}
	}
	f.Files = append(f.Files, x)
	f.NFiles++
}

// capLists cuts each list to ledgerListMax: the first ones kept (what the
// turn did first), the totals left as they are.
func (f *LedgerFacts) capLists() {
	if len(f.Files) > ledgerListMax {
		f.Files = f.Files[:ledgerListMax]
	}
	if len(f.Commits) > ledgerListMax {
		f.Commits = f.Commits[:ledgerListMax]
	}
	if len(f.Cmds) > ledgerListMax {
		f.Cmds = f.Cmds[:ledgerListMax]
	}
	if len(f.Agents) > ledgerListMax {
		f.Agents = f.Agents[:ledgerListMax]
	}
	if len(f.Errors) > ledgerListMax {
		f.Errors = f.Errors[:ledgerListMax]
	}
}

// Summary is the facts in one short line: "6 files (+120/−34) · 2 commits ·
// 11 cmds (1 failed)"; the parts that are zero are left out.
func (f LedgerFacts) Summary() string {
	var parts []string
	if f.NFiles > 0 {
		parts = append(parts, fmt.Sprintf("%s (+%d/−%d)", numfmt.Plural(f.NFiles, "file", "files"), f.Added, f.Removed))
	}
	if f.NCommits > 0 {
		parts = append(parts, numfmt.Plural(f.NCommits, "commit", "commits"))
	}
	if f.NCmds > 0 {
		s := numfmt.Plural(f.NCmds, "cmd", "cmds")
		if f.NFailed > 0 {
			s += fmt.Sprintf(" (%d failed)", f.NFailed)
		}
		parts = append(parts, s)
	}
	if f.NAgents > 0 {
		parts = append(parts, numfmt.Plural(f.NAgents, "agent", "agents"))
	}
	if f.NErrors > 0 {
		parts = append(parts, numfmt.Plural(f.NErrors, "error", "errors"))
	}
	return strings.Join(parts, " · ")
}

// more is "\n  … +N more" when a list was cut.
func moreLine(total, shown int) string {
	if total > shown {
		return fmt.Sprintf("\n  … +%d more", total-shown)
	}
	return ""
}

func agentTag(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" [agent #%d]", n)
}

// Text is the facts in full, one fact a line, for /journal view.
func (f LedgerFacts) Text() string {
	var b strings.Builder
	if len(f.Files) > 0 {
		fmt.Fprintf(&b, "files (%d, +%d/−%d):", f.NFiles, f.Added, f.Removed)
		for _, x := range f.Files {
			what := fmt.Sprintf("+%d/−%d", x.Added, x.Removed)
			switch {
			case x.MovedTo != "":
				what = "moved → " + x.MovedTo
			case x.Deleted:
				what += " deleted"
			case x.Created:
				what += " created"
			}
			fmt.Fprintf(&b, "\n  %s %s%s", x.Path, what, agentTag(x.Agent))
		}
		b.WriteString(moreLine(f.NFiles, len(f.Files)) + "\n")
	}
	if len(f.Commits) > 0 {
		fmt.Fprintf(&b, "commits (%d):", f.NCommits)
		for _, c := range f.Commits {
			fmt.Fprintf(&b, "\n  %s %s%s", c.Hash, c.Subject, agentTag(c.Agent))
		}
		b.WriteString(moreLine(f.NCommits, len(f.Commits)) + "\n")
	}
	if len(f.Cmds) > 0 {
		fmt.Fprintf(&b, "commands (%d, %d failed):", f.NCmds, f.NFailed)
		for _, c := range f.Cmds {
			v := "exit " + strconv.Itoa(c.Exit)
			if c.Verdict != "" {
				v = c.Verdict
			}
			pre := ""
			if c.Ended {
				pre = fmt.Sprintf("job #%d ended: ", c.Job)
			}
			fmt.Fprintf(&b, "\n  %s%s · %s · %s%s", pre, c.Command, v, workDuration(time.Duration(c.MS)*time.Millisecond), agentTag(c.Agent))
		}
		b.WriteString(moreLine(f.NCmds+f.NEnded, len(f.Cmds)) + "\n")
	}
	if len(f.Agents) > 0 {
		b.WriteString("sub-agents:")
		for _, a := range f.Agents {
			s := fmt.Sprintf("\n  #%d %s", a.N, a.Event)
			if a.Outcome != "" {
				s += " (" + a.Outcome + ")"
			}
			if a.Title != "" {
				s += ": " + a.Title
			}
			b.WriteString(s)
		}
		b.WriteString(moreLine(f.NAgents, len(f.Agents)) + "\n")
	}
	if len(f.Errors) > 0 {
		fmt.Fprintf(&b, "tool errors (%d):", f.NErrors)
		for _, e := range f.Errors {
			fmt.Fprintf(&b, "\n  %s: %s%s", e.Tool, e.Line, agentTag(e.Agent))
		}
		b.WriteString(moreLine(f.NErrors, len(f.Errors)) + "\n")
	}
	fmt.Fprintf(&b, "totals: %s, %s, %s tok in (%s cached), %s out",
		numfmt.Plural(f.Rounds, "round", "rounds"), numfmt.Plural(f.ToolCalls, "tool call", "tool calls"),
		strconv.Itoa(f.Prompt), strconv.Itoa(f.Cached), strconv.Itoa(f.Completion))
	if f.Costed {
		fmt.Fprintf(&b, ", %.4f %s", f.Cost, f.Currency)
	}
	b.WriteString(", " + workDuration(time.Duration(f.WallMS)*time.Millisecond))
	if f.Interrupted {
		b.WriteString("\ninterrupted")
	}
	if f.Error != "" {
		b.WriteString("\nturn error: " + f.Error)
	}
	return b.String()
}

// ── collection ──────────────────────────────────────────────────────────

// ledgerState is the root's open turn. Events that arrive between turns (a
// job or a sub-agent ending while idle) go into the next turn.
type ledgerState struct {
	mu      sync.Mutex
	open    bool
	turn    int // the session's turn number (asks.go counts them)
	start   time.Time
	stats0  TurnStats         // the round stats as the turn began
	heads   map[string]string // workspace → HEAD as the turn began
	facts   LedgerFacts
	touched []string // plan node ids changed during the turn
	// asked are the asks open during the turn: open at its start, or opened
	// in it (asks.go).
	asked []int
	// pendingOutcome is a finished turn's outcome, not yet written to the
	// rounds ledger: it is written when the NEXT user message arrives, which
	// is also when the user's correction or acceptance is recorded (the
	// reaction is unknowable before the message).
	pendingOutcome *TurnOutcome
}

// ledgerCallKey carries a call's ledgerCall to exec, which fills it.
type ledgerCallKey struct{}

// ledgerCall is what exec says of the command it ran.
type ledgerCall struct {
	set     bool
	exit    int
	failed  bool
	verdict string
	job     int
}

// markExecLedger records how an exec ended, for the ledger (withLedger).
func markExecLedger(ctx context.Context, r ExecResult, done bool, job int) {
	lc, ok := ctx.Value(ledgerCallKey{}).(*ledgerCall)
	if !ok {
		return
	}
	lc.set, lc.exit, lc.failed = true, r.Code, r.Failed()
	switch {
	case !done:
		lc.verdict, lc.job, lc.failed = fmt.Sprintf("job #%d still running", job), job, false
	case r.Killed != "":
		lc.verdict = "killed"
	case r.Err != "":
		lc.verdict = "did not run: " + oneLine(r.Err, 80)
	}
}

// ledgerAgentNum is the number this harness's facts carry: its own as a
// sub-agent, 0 for the root.
func (h *Harness) ledgerAgentNum() int {
	if h.isChild() {
		return h.cfg.AgentID
	}
	return 0
}

// fileTool is the file tool a call names, bare ("edit"), or "": the name
// as configured, with or without the toolset's prefix.
func fileTool(name string) string {
	for _, t := range []string{"edit", "write", "delete", "move"} {
		if name == t || strings.HasSuffix(name, "_"+t) {
			return t
		}
	}
	return ""
}

// withLedger records every dispatched call into the root's ledger. Outside
// every other wrapper: a refused call is a call too.
func withLedger(inner agent.ToolDispatcher, h *Harness) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		name := tc.Function.Name
		var args struct {
			Path    string `json:"path"`
			Command string `json:"command"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		ft := fileTool(name)
		var before *string
		if (ft == "write" || ft == "delete") && args.Path != "" {
			p := args.Path
			if !filepath.IsAbs(p) {
				p = filepath.Join(h.Workspace(), p)
			}
			if b, err := os.ReadFile(p); err == nil {
				s := string(b)
				before = &s
			}
		}
		lc := &ledgerCall{}
		t0 := time.Now()
		out, err := inner(context.WithValue(ctx, ledgerCallKey{}, lc), tc)
		res := out
		if err != nil && res == "" {
			res = "ERROR: " + err.Error()
		}
		h.ragRoot().ledgerNote(h, name, ft, args.Path, args.Command, before, res, err != nil, lc, time.Since(t0))
		return out, err
	}
}

var (
	editedRe  = regexp.MustCompile(`^edited (.+): \+(\d+) -(\d+)`)
	wroteRe   = regexp.MustCompile(`^wrote (.+) \((\d+) lines?, (new|overwritten; was (\d+) lines?)\)`)
	deletedRe = regexp.MustCompile(`^deleted (.+) \(`)
	movedRe   = regexp.MustCompile(`^moved (.+) → (.+)$`)
)

// ledgerNote files one call of h (the root or a child) into the root's turn.
func (h *Harness) ledgerNote(from *Harness, name, ft, path, command string, before *string, res string, failed bool, lc *ledgerCall, took time.Duration) {
	if h.isChild() {
		return
	}
	agentN := from.ledgerAgentNum()
	first := headLine(res, 160)
	var file *LedgerFile
	switch ft {
	case "edit":
		if m := editedRe.FindStringSubmatch(first); m != nil {
			a, _ := strconv.Atoi(m[2])
			r, _ := strconv.Atoi(m[3])
			file = &LedgerFile{Path: m[1], Added: a, Removed: r}
		}
	case "write":
		if m := wroteRe.FindStringSubmatch(first); m != nil {
			n, _ := strconv.Atoi(m[2])
			file = &LedgerFile{Path: m[1], Added: n}
			if m[3] == "new" {
				file.Created = true
			} else if before != nil {
				after, _ := readFileIn(from.Workspace(), path)
				file.Added, file.Removed = lineDiff(*before, after)
			} else {
				file.Removed, _ = strconv.Atoi(m[4])
			}
		}
	case "delete":
		if m := deletedRe.FindStringSubmatch(first); m != nil {
			file = &LedgerFile{Path: m[1], Deleted: true}
			if before != nil {
				file.Removed = len(splitLines(*before))
			}
		}
	case "move":
		if m := movedRe.FindStringSubmatch(first); m != nil {
			file = &LedgerFile{Path: m[1], MovedTo: m[2]}
		}
	}
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	f := &h.ledger.facts
	f.ToolCalls++
	if file != nil {
		file.Agent = agentN
		f.addFile(*file)
	}
	switch {
	case name == "exec" && lc.set:
		c := LedgerCmd{Command: headLine(command, 200), Agent: agentN, Exit: lc.exit, Failed: lc.failed,
			Verdict: lc.verdict, MS: took.Milliseconds(), Job: lc.job}
		f.Cmds = append(f.Cmds, c)
		f.NCmds++
		if c.Failed {
			f.NFailed++
		}
		if lc.job > 0 {
			f.NJobs++
		}
	case failed || ToolFailed(name, res):
		f.Errors = append(f.Errors, LedgerError{Tool: name, Line: first, Agent: agentN})
		f.NErrors++
	}
}

// readFileIn reads path, relative to dir when it is not absolute.
func readFileIn(dir, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

// ledgerJobEnd records a background job's end (bgJob.finish).
func (h *Harness) ledgerJobEnd(from *Harness, id int, command string, r ExecResult, took time.Duration) {
	if h.isChild() {
		return
	}
	c := LedgerCmd{Command: headLine(command, 200), Agent: from.ledgerAgentNum(), Exit: r.Code, Failed: r.Failed(),
		MS: took.Milliseconds(), Job: id, Ended: true}
	switch {
	case r.Killed != "":
		c.Verdict = "killed"
	case r.Err != "":
		c.Verdict = "did not run"
	}
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	h.ledger.facts.Cmds = append(h.ledger.facts.Cmds, c)
	h.ledger.facts.NEnded++
	if c.Failed {
		// A failed job is a failed command; its start was counted as one.
		h.ledger.facts.NFailed++
	}
}

// ledgerAgent records a sub-agent starting or finishing.
func (h *Harness) ledgerAgent(n int, title, event, outcome string) {
	if h.isChild() {
		return
	}
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	h.ledger.facts.Agents = append(h.ledger.facts.Agents, LedgerAgent{N: n, Title: oneLine(title, 80), Event: event, Outcome: outcome})
	if event == "started" {
		h.ledger.facts.NAgents++
	}
}

// ledgerTouched notes plan nodes a change touched, for attribution (asks.go).
func (h *Harness) ledgerTouched(cs []taskgraph.Change) {
	if h.isChild() {
		return
	}
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	for _, c := range cs {
		h.ledger.touched = append(h.ledger.touched, c.ID)
	}
}

// ── the turn ────────────────────────────────────────────────────────────

// ledgerBegin opens the turn: HEADs, round stats, the asks open now.
func (h *Harness) ledgerBegin() {
	if h.isChild() {
		return
	}
	heads := h.workspaceHeads()
	h.rounds.mu.Lock()
	s0 := h.rounds.turn
	h.rounds.mu.Unlock()
	open := h.asksOpenNow()
	turn := h.askTurnBegin()
	h.ledger.mu.Lock()
	h.ledger.turn = turn
	h.ledger.open, h.ledger.start, h.ledger.stats0, h.ledger.heads = true, time.Now(), s0, heads
	h.ledger.touched, h.ledger.asked = nil, open
	h.ledger.mu.Unlock()
}

// ledgerTake closes the turn and returns its facts, and the plan nodes and
// asks it touched; ok false when no turn was open.
func (h *Harness) ledgerTake(err error, interrupted bool) (f LedgerFacts, start time.Time, turn int, touched []string, asked []int, ok bool) {
	h.ledger.mu.Lock()
	if !h.ledger.open {
		h.ledger.mu.Unlock()
		return LedgerFacts{}, time.Time{}, 0, nil, nil, false
	}
	turn = h.ledger.turn
	heads0 := h.ledger.heads
	start, touched, asked = h.ledger.start, h.ledger.touched, h.ledger.asked
	s0 := h.ledger.stats0
	f = h.ledger.facts
	h.ledger.facts, h.ledger.open, h.ledger.touched, h.ledger.asked = LedgerFacts{}, false, nil, nil
	h.ledger.mu.Unlock()

	h.rounds.mu.Lock()
	s1 := h.rounds.turn
	h.rounds.mu.Unlock()
	if s1.Rounds < s0.Rounds { // the stats were closed during the turn
		s0 = TurnStats{}
	}
	f.Rounds = s1.Rounds - s0.Rounds
	if f.Rounds == 0 { // no spend meter: the turn's own count (turnlimit.go)
		f.Rounds = int(h.turnRounds.n.Load())
	}
	f.Prompt, f.Cached, f.Completion = s1.Prompt-s0.Prompt, s1.Cached-s0.Cached, s1.Completion-s0.Completion
	if s1.Costed > s0.Costed {
		f.Cost, f.Currency, f.Costed = s1.Cost-s0.Cost, s1.Currency, true
	}
	f.WallMS = time.Since(start).Milliseconds()
	f.Interrupted = interrupted
	if err != nil && !interrupted {
		f.Error = headLine(err.Error(), 200)
	}
	for _, c := range h.commitsSince(heads0) {
		f.NCommits++
		f.Commits = append(f.Commits, c)
	}
	f.capLists()
	return f, start, turn, touched, asked, true
}

// ledgerEnd closes the turn and files its facts: to the asks open during
// it, or, with none and a tool call made, as a turn entry (asks.go). The
// turn's outcome (commits, last test verdict) is stashed as pending for the
// rounds ledger: it is written when the NEXT user message arrives, which is
// also when the user's correction or acceptance is recorded.
func (h *Harness) ledgerEnd(ctx context.Context, err error) {
	if h.isChild() {
		return
	}
	interrupted := err != nil && (interruptedErr(err) || interrupted(ctx))
	f, start, turn, touched, asked, ok := h.ledgerTake(err, interrupted)
	if !ok {
		return
	}
	h.noteTurnOutcome(f, turn)
	h.asksFile(ctx, f, start, turn, touched, asked)
}

// noteTurnOutcome stashes a finished turn's outcome for the rounds ledger.
// It is written (appendTurnOutcome) when the next person-origin turn begins,
// carrying that turn's first user message as the reaction; a non-person turn
// in between writes the line without a reaction, so the person's message
// later does not overwrite it.
func (h *Harness) noteTurnOutcome(f LedgerFacts, turn int) {
	if h.rounds.ledger == nil {
		return
	}
	var commits []string
	for _, c := range f.Commits {
		hh := c.Hash
		if len(hh) > 8 {
			hh = hh[:8]
		}
		if len(commits) < TurnOutcomeCommits {
			commits = append(commits, hh)
		}
	}
	o := &TurnOutcome{
		Kind: "outcome", Time: time.Now(), Session: h.sessionName(),
		Turn: turn, Commits: commits, NCommits: f.NCommits,
		TestVerdict: lastTestVerdict(f.Cmds),
	}
	o.NFiles = f.NFiles
	h.ledger.mu.Lock()
	h.ledger.pendingOutcome = o
	h.ledger.mu.Unlock()
}

// appendTurnOutcome writes the stashed turn outcome to the rounds ledger.
// text is the reaction message (the person's next message) and o the turn's
// origin; an empty text leaves Reaction empty. It is a no-op with nothing
// stashed.
func (h *Harness) appendTurnOutcome(text string) {
	h.ledger.mu.Lock()
	o := h.ledger.pendingOutcome
	h.ledger.pendingOutcome = nil
	h.ledger.mu.Unlock()
	if o == nil || h.rounds.ledger == nil {
		return
	}
	if text != "" {
		o.Reaction = reactionLabel(text)
		o.Message = headLine(text, TurnOutcomeMessageCap)
	}
	_ = h.rounds.ledger.AppendOutcome(*o)
}

// ── git ─────────────────────────────────────────────────────────────────

// gitTimeout bounds one git call of the ledger.
const gitTimeout = 5 * time.Second

func ledgerRun(dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// ledgerWorkspaces are the trees whose HEAD the ledger watches: the root's,
// then each sub-agent's own (an isolated child's worktree), by agent number.
func (h *Harness) ledgerWorkspaces() map[string]int {
	ws := map[string]int{}
	if w := h.Workspace(); w != "" {
		ws[w] = 0
	}
	for _, sa := range h.agentList() {
		if c := sa.harness(); c != nil {
			if w := c.Workspace(); w != "" {
				if _, ok := ws[w]; !ok {
					ws[w] = sa.num
				}
			}
		}
	}
	return ws
}

// workspaceHeads is each watched tree's HEAD; a tree that is no repo is left out.
func (h *Harness) workspaceHeads() map[string]string {
	out := map[string]string{}
	for w := range h.ledgerWorkspaces() {
		if head, ok := ledgerRun(w, "rev-parse", "HEAD"); ok {
			out[w] = head
		}
	}
	return out
}

// commitsSince is the commits each watched tree made after heads0. A tree
// new since (a child's worktree made during the turn) counts from the root's
// HEAD at the start, which it branched from.
func (h *Harness) commitsSince(heads0 map[string]string) []LedgerCommit {
	var out []LedgerCommit
	seen := map[string]bool{}
	root := heads0[h.Workspace()]
	for w, n := range h.ledgerWorkspaces() {
		head, ok := ledgerRun(w, "rev-parse", "HEAD")
		if !ok {
			continue
		}
		base := heads0[w]
		if base == "" {
			base = root
		}
		if base == "" || base == head {
			continue
		}
		log, ok := ledgerRun(w, "log", "--first-parent", "--format=%h%x09%s", "-n", strconv.Itoa(ledgerListMax+1), base+".."+head)
		if !ok || log == "" {
			continue
		}
		for _, l := range strings.Split(log, "\n") {
			hash, subj, _ := strings.Cut(l, "\t")
			if seen[hash] {
				continue
			}
			seen[hash] = true
			out = append(out, LedgerCommit{Hash: hash, Subject: oneLine(subj, 120), Agent: n})
		}
	}
	return out
}

// ── line diff ───────────────────────────────────────────────────────────

// splitLines is s's lines; a final newline ends the last line, it does not
// start another.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// lineDiffMax bounds the edit distance lineDiff searches; past it, the
// count is the whole old and new middle (an upper bound).
const lineDiffMax = 4000

// lineDiff counts the lines added and removed from a to b: a shortest edit
// script's (Myers), on the middle the common head and tail leave.
func lineDiff(a, b string) (added, removed int) {
	al, bl := splitLines(a), splitLines(b)
	for len(al) > 0 && len(bl) > 0 && al[0] == bl[0] {
		al, bl = al[1:], bl[1:]
	}
	for len(al) > 0 && len(bl) > 0 && al[len(al)-1] == bl[len(bl)-1] {
		al, bl = al[:len(al)-1], bl[:len(bl)-1]
	}
	n, m := len(al), len(bl)
	if n == 0 || m == 0 {
		return m, n
	}
	limit := min(n+m, lineDiffMax)
	off := limit + 1
	v := make([]int, 2*limit+3)
	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || k != d && v[off+k-1] < v[off+k+1] {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && al[x] == bl[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return (d + m - n) / 2, (d - m + n) / 2
			}
		}
	}
	return m, n
}
