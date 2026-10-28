package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/llm"
)

// /worklog: a per-session tree of tasks the person opens and closes, and the
// session's events recorded into whichever task was open (docs/plan.md,
// "Sessions and agents").
//
//	/worklog start <title>                    open a task; under an open task it is a subtask
//	/worklog end [done|dropped|blocked] <why> close the open task, back to its parent
//	/worklog                                  the tree, with times and outcomes
//	/worklog view [n]                         one task's events (bare: the open one)
//	/worklog write [n]                        the model writes the account of a closed task
//
// Events are the person's inputs, tool calls by name with the first line of
// their result, recaps (the summary text) and notes (Notify: a job finished,
// a server said something). Each is bounded when recorded.
//
// The account is one side call (kind "worklog"): the task's events go to the
// model in one message and the answer is written to
// <session>.worklog/<n>-<slug>.md; nothing enters the session's context. dun
// writes the header (model, effort, harness, version) from HarnessInfo, so it
// is exact.
//
// Kept in <session>.worklog.json, so a resumed session keeps its worklog.
// Person-driven only: the model does not open or close tasks.

// Worklog outcomes.
const (
	WorkDone    = "done"
	WorkDropped = "dropped"
	WorkBlocked = "blocked"
)

// Worklog event kinds.
const (
	WorkInput = "input"
	WorkTool  = "tool"
	WorkRecap = "recap"
	WorkNote  = "note"
)

// Bounds on what one event keeps, and on what one account's prompt carries.
const (
	workInputMax  = 2000
	workResultMax = 160
	workArgsMax   = 160
	workRecapMax  = 1500
	workNoteMax   = 300
	workEventsMax = 2000 // per task; past it, the oldest after the first few go
	workPromptMax = 24000
)

// Worklog is the sidecar's content.
type Worklog struct {
	Tasks []*WorkTask `json:"tasks"`
	// Open is the open task's N; 0 is none.
	Open int `json:"open,omitempty"`
}

// WorkTask is one task. N counts from 1 in the order tasks were started.
type WorkTask struct {
	N       int         `json:"n"`
	Title   string      `json:"title"`
	Parent  int         `json:"parent,omitempty"`
	Start   time.Time   `json:"start"`
	End     *time.Time  `json:"end,omitempty"`
	Outcome string      `json:"outcome,omitempty"`
	Why     string      `json:"why,omitempty"`
	Events  []WorkEvent `json:"events,omitempty"`
	// Dropped counts events let go at workEventsMax.
	Dropped int `json:"dropped,omitempty"`
	// Account is the path of the last account written for it.
	Account string `json:"account,omitempty"`
}

// WorkEvent is one thing that happened while its task was open.
type WorkEvent struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	// Text is the input, the tool's name, the recap's summary or the note.
	Text string `json:"text"`
	// Result is a tool call's first line of output.
	Result string `json:"result,omitempty"`
}

// task is the task numbered n; nil when there is none.
func (w *Worklog) task(n int) *WorkTask {
	if n < 1 || n > len(w.Tasks) {
		return nil
	}
	return w.Tasks[n-1]
}

// Begin opens a task under the open one (if any) and makes it the open one.
func (w *Worklog) Begin(title string, now time.Time) (*WorkTask, error) {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return nil, fmt.Errorf("a task needs a title: /worklog start <title>")
	}
	t := &WorkTask{N: len(w.Tasks) + 1, Title: title, Parent: w.Open, Start: now}
	w.Tasks = append(w.Tasks, t)
	w.Open = t.N
	return t, nil
}

// Finish closes the open task with an outcome and why, and the parent
// becomes the open one.
func (w *Worklog) Finish(outcome, why string, now time.Time) (*WorkTask, error) {
	t := w.task(w.Open)
	if t == nil {
		return nil, fmt.Errorf("no task is open: /worklog start <title>")
	}
	switch outcome {
	case WorkDone, WorkDropped, WorkBlocked:
	default:
		return nil, fmt.Errorf("outcome %q: done, dropped or blocked", outcome)
	}
	why = strings.Join(strings.Fields(why), " ")
	if why == "" {
		return nil, fmt.Errorf("say why: /worklog end [done|dropped|blocked] <why>")
	}
	t.End, t.Outcome, t.Why = &now, outcome, why
	w.Open = t.Parent
	return t, nil
}

// Record adds an event to the open task; false when none is open.
func (w *Worklog) Record(e WorkEvent) bool {
	t := w.task(w.Open)
	if t == nil {
		return false
	}
	if len(t.Events) >= workEventsMax {
		// Keep the first few (what was asked) and the latest.
		const head = 20
		t.Events = append(t.Events[:head], t.Events[head+1:]...)
		t.Dropped++
	}
	t.Events = append(t.Events, e)
	return true
}

// depth is how many parents t has.
func (w *Worklog) depth(t *WorkTask) int {
	d := 0
	for p := w.task(t.Parent); p != nil && d < len(w.Tasks); p = w.task(p.Parent) {
		d++
	}
	return d
}

// children are the tasks started directly under n, in order.
func (w *Worklog) children(n int) []*WorkTask {
	var out []*WorkTask
	for _, t := range w.Tasks {
		if t.Parent == n {
			out = append(out, t)
		}
	}
	return out
}

// Tree is /worklog: every task, indented under its parent, with its times
// and outcome.
func (w *Worklog) Tree(now time.Time) string {
	if len(w.Tasks) == 0 {
		return "worklog: no tasks · /worklog start <title> opens one"
	}
	var b strings.Builder
	open := "none open"
	if w.Open != 0 {
		open = fmt.Sprintf("#%d open", w.Open)
	}
	fmt.Fprintf(&b, "worklog: %d task%s, %s", len(w.Tasks), plural(len(w.Tasks)), open)
	var walk func(parent int)
	walk = func(parent int) {
		for _, t := range w.children(parent) {
			b.WriteString("\n" + strings.Repeat("  ", w.depth(t)+1) + t.line(now))
			walk(t.N)
		}
	}
	walk(0)
	b.WriteString("\n/worklog view <n> · /worklog write <n> (a closed task)")
	return b.String()
}

// line is a task's row in the tree.
func (t *WorkTask) line(now time.Time) string {
	clock := t.Start.Local().Format("15:04")
	if t.End == nil {
		return fmt.Sprintf("#%d %s · %s– open %s · %d event%s", t.N, t.Title, clock,
			workDuration(now.Sub(t.Start)), len(t.Events), plural(len(t.Events)))
	}
	s := fmt.Sprintf("#%d %s · %s–%s %s · %s: %s", t.N, t.Title, clock, t.End.Local().Format("15:04"),
		workDuration(t.End.Sub(t.Start)), t.Outcome, t.Why)
	if t.Account != "" {
		s += " · written"
	}
	return s
}

func workDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// View is /worklog view <n>: one task's events, one line each.
func (w *Worklog) View(n int, now time.Time) (string, error) {
	t := w.task(n)
	if t == nil {
		return "", fmt.Errorf("no task #%d (/worklog lists them)", n)
	}
	var b strings.Builder
	b.WriteString(t.line(now))
	if p := w.task(t.Parent); p != nil {
		fmt.Fprintf(&b, "\nunder #%d %s", p.N, p.Title)
	}
	if t.Account != "" {
		b.WriteString("\naccount: " + t.Account)
	}
	if len(t.Events) == 0 {
		b.WriteString("\nno events recorded")
	}
	for i, e := range t.Events {
		if t.Dropped > 0 && i == 20 {
			fmt.Fprintf(&b, "\n  … %d earlier events not kept", t.Dropped)
		}
		b.WriteString("\n  " + e.At.Local().Format("15:04:05") + " " + e.line(200))
	}
	return b.String(), nil
}

// line is the event in one line of at most max runes of text.
func (e WorkEvent) line(max int) string {
	switch e.Kind {
	case WorkInput:
		return "you: " + workClip(e.Text, max)
	case WorkTool:
		if e.Result == "" {
			return "tool " + e.Text
		}
		return "tool " + e.Text + " → " + workClip(e.Result, max)
	default:
		return e.Kind + ": " + workClip(e.Text, max)
	}
}

// workClip is s on one line, cut at max runes.
func workClip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}

// headLine is the first non-blank line of s, cut at max runes.
func headLine(s string, max int) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return workClip(l, max)
		}
	}
	return ""
}

// clipBlock keeps line breaks and cuts at max runes.
func clipBlock(s string, max int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}

// ── the account ─────────────────────────────────────────────────────────

const worklogInstruction = `Below is the record of one task from a coding agent's session: the person's inputs, the tool calls with the first line of each result, the summaries the agent wrote of its own history (recaps), and notes. Write a clean account of the task for someone who was not there.

Sections, as markdown "##" headings: What was asked; What was done; Decisions and why; What is left.
Rules:
- Only what the record shows. Never invent a file, command, result or reason; when the record does not say why, say so.
- Name files, commands and results exactly as they appear.
- Plain and short: no preamble, no closing remarks, no praise.
- Do not repeat the header; it is written for you.
- Reply with only the account.

`

// WorklogHeader is the account's header: the task, and what wrote it.
func WorklogHeader(t *WorkTask, info HarnessInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", t.Title)
	model := info.Model
	if info.Provider != "" {
		model += " (" + info.Provider + ")"
	}
	fmt.Fprintf(&b, "- Model: %s · effort: %s\n", model, info.Effort)
	fmt.Fprintf(&b, "- Harness: %s %s %s\n", info.Harness, info.Version, info.Icon)
	if info.SessionID != "" {
		fmt.Fprintf(&b, "- Session: %s · task #%d\n", info.SessionID, t.N)
	} else {
		fmt.Fprintf(&b, "- Task #%d\n", t.N)
	}
	if info.Git != nil && info.Git.Branch != "" {
		fmt.Fprintf(&b, "- Branch: %s", info.Git.Branch)
		if info.Git.Commit != "" {
			fmt.Fprintf(&b, " @ %s", info.Git.Commit)
		}
		b.WriteString("\n")
	}
	end := "open"
	if t.End != nil {
		end = t.End.UTC().Format(time.RFC3339) + " (" + workDuration(t.End.Sub(t.Start)) + ")"
	}
	fmt.Fprintf(&b, "- Started: %s · ended: %s\n", t.Start.UTC().Format(time.RFC3339), end)
	if t.Outcome != "" {
		fmt.Fprintf(&b, "- Outcome: %s — %s\n", t.Outcome, t.Why)
	}
	return b.String()
}

// eventText is an event as the account's prompt carries it: an input or a
// recap whole (as bounded when recorded), a tool call on one line.
func (e WorkEvent) eventText() string {
	at := e.At.UTC().Format("15:04:05")
	switch e.Kind {
	case WorkInput:
		return at + " INPUT: " + e.Text
	case WorkRecap:
		return at + " RECAP: " + e.Text
	case WorkTool:
		return at + " TOOL " + e.Line()
	default:
		return at + " NOTE: " + e.Text
	}
}

// Line is a tool event's name and result.
func (e WorkEvent) Line() string {
	if e.Result == "" {
		return e.Text
	}
	return e.Text + " → " + e.Result
}

// AccountPrompt is the one message the model gets for task n: the
// instruction, the header, the task's subtasks, and its events, at most
// workPromptMax characters of events (the first and the last kept, the
// middle said to be left out).
func (w *Worklog) AccountPrompt(n int, info HarnessInfo) (string, error) {
	t := w.task(n)
	if t == nil {
		return "", fmt.Errorf("no task #%d (/worklog lists them)", n)
	}
	if t.End == nil {
		return "", fmt.Errorf("task #%d is open: /worklog end it first", n)
	}
	var b strings.Builder
	b.WriteString(worklogInstruction)
	b.WriteString("The header (written for you):\n" + WorklogHeader(t, info) + "\n")
	if p := w.task(t.Parent); p != nil {
		fmt.Fprintf(&b, "Part of task #%d: %s\n", p.N, p.Title)
	}
	if kids := w.children(t.N); len(kids) > 0 {
		b.WriteString("Subtasks (their events are in their own accounts):\n")
		for _, k := range kids {
			s := "open"
			if k.End != nil {
				s = k.Outcome + ": " + k.Why
			}
			fmt.Fprintf(&b, "- #%d %s — %s\n", k.N, k.Title, s)
		}
	}
	b.WriteString("\nThe record:\n")
	lines := make([]string, len(t.Events))
	for i, e := range t.Events {
		lines[i] = e.eventText()
	}
	if t.Dropped > 0 && len(lines) > 20 {
		lines = append(append(lines[:20:20], fmt.Sprintf("(%d events not kept)", t.Dropped)), lines[20:]...)
	}
	if len(lines) == 0 {
		b.WriteString("(no events were recorded)\n")
	}
	for _, l := range boundLines(lines, workPromptMax) {
		b.WriteString(l + "\n")
	}
	return b.String(), nil
}

// boundLines keeps lines from the start and the end, at most max runes in
// all, with one line in between saying how many were left out.
func boundLines(lines []string, max int) []string {
	total := 0
	for _, l := range lines {
		total += utf8.RuneCountInString(l) + 1
	}
	if total <= max {
		return lines
	}
	var head, tail []string
	used, i, j := 0, 0, len(lines)-1
	for i <= j {
		// Alternate: the start says what was asked, the end what came of it.
		l := lines[i]
		if n := utf8.RuneCountInString(l) + 1; used+n <= max/2 || len(head) == 0 {
			head = append(head, clipBlock(l, max/2))
			used += min(n, max/2)
			i++
		} else {
			break
		}
	}
	for j >= i {
		l := lines[j]
		n := utf8.RuneCountInString(l) + 1
		if used+n > max {
			break
		}
		tail = append([]string{l}, tail...)
		used += n
		j--
	}
	out := head
	if left := j - i + 1; left > 0 {
		out = append(out, fmt.Sprintf("(%d events left out here)", left))
	}
	return append(out, tail...)
}

// ── the session's worklog ───────────────────────────────────────────────

// WorklogFile is the session's worklog sidecar.
func WorklogFile(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".worklog.json"
}

// WorklogDir is where the session's accounts are written.
func WorklogDir(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".worklog"
}

// worklogLocked is the session's worklog, read from its sidecar on first use
// (and again if the session's file changed). Callers hold wlMu.
func (h *Harness) worklogLocked() *Worklog {
	if h.wl != nil && h.wlFile == h.cfg.SessionFile {
		return h.wl
	}
	h.wl, h.wlFile = &Worklog{}, h.cfg.SessionFile
	if h.cfg.SessionFile != "" {
		if b, err := os.ReadFile(WorklogFile(h.cfg.SessionFile)); err == nil {
			var w Worklog
			if json.Unmarshal(b, &w) == nil {
				h.wl = &w
			}
		}
	}
	return h.wl
}

// saveWorklogLocked writes the sidecar; a session in memory keeps it there.
func (h *Harness) saveWorklogLocked() error {
	if h.cfg.SessionFile == "" || h.wl == nil {
		return nil
	}
	b, err := json.MarshalIndent(h.wl, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(WorklogFile(h.cfg.SessionFile), append(b, '\n'), 0o600)
}

// workRecord adds an event to the open task. Sub-agents record nothing: the
// worklog is the root session's.
func (h *Harness) workRecord(kind, text, result string) {
	if h.cfg.Parent != nil {
		return
	}
	h.wlMu.Lock()
	defer h.wlMu.Unlock()
	w := h.worklogLocked()
	if w.Open == 0 {
		return
	}
	if w.Record(WorkEvent{At: time.Now(), Kind: kind, Text: text, Result: result}) {
		_ = h.saveWorklogLocked()
	}
}

func (h *Harness) workInput(text string) {
	h.workRecord(WorkInput, clipBlock(text, workInputMax), "")
}

func (h *Harness) workTool(name, result string) {
	h.workRecord(WorkTool, name, headLine(result, workResultMax))
}

// workToolText is a tool call as an event names it: the tool and, short,
// its arguments (the command, the path), which an account names.
func workToolText(tool string, args map[string]any) string {
	if len(args) == 0 {
		return tool
	}
	b, err := json.Marshal(args)
	if err != nil {
		return tool
	}
	return tool + " " + workClip(string(b), workArgsMax)
}

func (h *Harness) workRecap(summary string) {
	h.workRecord(WorkRecap, clipBlock(summary, workRecapMax), "")
}

func (h *Harness) workNote(text string) {
	h.workRecord(WorkNote, workClip(text, workNoteMax), "")
}

// WorklogCmd runs a /worklog action other than write: "", "start <title>",
// "end [outcome] <why>", "view [n]". The answer is for the person.
func (h *Harness) WorklogCmd(action string) (string, error) {
	verb, rest, _ := strings.Cut(strings.TrimSpace(action), " ")
	rest = strings.TrimSpace(rest)
	now := time.Now()
	h.wlMu.Lock()
	defer h.wlMu.Unlock()
	w := h.worklogLocked()
	switch strings.ToLower(verb) {
	case "", "list", "tree":
		return w.Tree(now), nil
	case "start":
		parent := w.task(w.Open)
		t, err := w.Begin(rest, now)
		if err != nil {
			return "", err
		}
		if err := h.saveWorklogLocked(); err != nil {
			return "", err
		}
		if parent != nil {
			return fmt.Sprintf("worklog: #%d %s — a subtask of #%d %s", t.N, t.Title, parent.N, parent.Title), nil
		}
		return fmt.Sprintf("worklog: #%d %s — started", t.N, t.Title), nil
	case "end":
		outcome := WorkDone
		if o, why, _ := strings.Cut(rest, " "); o == WorkDone || o == WorkDropped || o == WorkBlocked {
			outcome, rest = o, strings.TrimSpace(why)
		}
		t, err := w.Finish(outcome, rest, now)
		if err != nil {
			return "", err
		}
		if err := h.saveWorklogLocked(); err != nil {
			return "", err
		}
		msg := fmt.Sprintf("worklog: #%d %s — %s: %s (%s)", t.N, t.Title, t.Outcome, t.Why, workDuration(t.End.Sub(t.Start)))
		if p := w.task(w.Open); p != nil {
			msg += fmt.Sprintf(" · back to #%d %s", p.N, p.Title)
		}
		return msg + " · /worklog write " + strconv.Itoa(t.N) + " writes its account", nil
	case "view":
		n := w.Open
		if rest != "" {
			var err error
			if n, err = taskNumber(rest); err != nil {
				return "", err
			}
		}
		if n == 0 {
			return "", fmt.Errorf("no task is open: /worklog view <n>")
		}
		return w.View(n, now)
	}
	return "", fmt.Errorf("usage: /worklog [start <title> | end [done|dropped|blocked] <why> | view [n] | write [n]]")
}

func taskNumber(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not a task number (/worklog lists them)", s)
	}
	return n, nil
}

// WriteWorklog is /worklog write [n]: one side call writes the account of
// closed task n (bare: the one closed last) to <session>.worklog/<n>-<slug>.md.
// It returns the file's path.
func (h *Harness) WriteWorklog(ctx context.Context, arg string) (string, error) {
	if h.Session == nil || h.Session.Runner == nil {
		return "", fmt.Errorf("no model to ask")
	}
	h.wlMu.Lock()
	w := h.worklogLocked()
	n := 0
	if strings.TrimSpace(arg) != "" {
		var err error
		if n, err = taskNumber(arg); err != nil {
			h.wlMu.Unlock()
			return "", err
		}
	} else {
		var last time.Time
		for _, t := range w.Tasks {
			if t.End != nil && !t.End.Before(last) {
				n, last = t.N, *t.End
			}
		}
		if n == 0 {
			h.wlMu.Unlock()
			return "", fmt.Errorf("no task is closed: /worklog end one first")
		}
	}
	info := h.HarnessInfo()
	prompt, err := w.AccountPrompt(n, info)
	var t WorkTask
	if err == nil {
		t = *w.task(n)
	}
	h.wlMu.Unlock()
	if err != nil {
		return "", err
	}

	body, err := h.worklogSideCall(ctx, prompt)
	if err != nil {
		return "", err
	}
	text := WorklogHeader(&t, info) + "\n" + body + "\n"

	dir := ""
	if h.cfg.SessionFile != "" {
		dir = WorklogDir(h.cfg.SessionFile)
	} else {
		dir = filepath.Join(os.TempDir(), "dun-worklog")
	}
	path := filepath.Join(dir, fmt.Sprintf("%d-%s.md", t.N, slug(t.Title)))
	if err := writeFileAtomic(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	h.wlMu.Lock()
	if wt := h.worklogLocked().task(n); wt != nil {
		wt.Account = path
		_ = h.saveWorklogLocked()
	}
	h.wlMu.Unlock()
	return path, nil
}

// worklogSideCall asks the model once, outside the conversation.
func (h *Harness) worklogSideCall(ctx context.Context, prompt string) (string, error) {
	start := time.Now()
	ch, err := untee(h.Session.Runner).ChatStream(ctx, []llm.Message{{Role: "user", Content: prompt}}, nil, nil)
	if err != nil {
		h.noteSideCall("worklog", start, nil)
		return "", err
	}
	var out strings.Builder
	var usage *llm.Usage
	for c := range ch {
		if c.Error != "" {
			h.noteSideCall("worklog", start, nil)
			return "", fmt.Errorf("%s", c.Error)
		}
		out.WriteString(c.Content)
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	h.noteSideCall("worklog", start, usage)
	text := strings.TrimSpace(out.String())
	if text == "" {
		return "", fmt.Errorf("the model wrote no account")
	}
	return text, nil
}
