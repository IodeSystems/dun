package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/internal/numfmt"
)

// /journal: a time-ordered list of what the agent did (USER 2026-11-17).
//
//	/journal          the entries, newest first: time, kind, title, tokens
//	/journal view <n> one entry's text
//	/journal now      a side call writes an entry for everything since the last one
//
// Three kinds of entry:
//   - fold: every compaction fold, from the summary the fold already wrote
//     (<session>.folds/<key>.md, sessionindex_dund.go); no extra model call.
//     A fold with no file (no dund link, or the rescue fold) keeps its
//     summary in the entry.
//   - now: /journal now, one side call (kind "journal") over the plan and the
//     session's entries since the last journal entry. Its text is written to
//     <session>.journal/<n>.md, so the sessions index can hold it as it holds
//     the folds.
//   - worklog: an account /worklog write wrote; the entry points at its file.
//   - ask: a message the person typed, closed (done, dropped) or still open
//     as the session ended, with what the turns that served it did (asks.go,
//     ledger.go). Open asks are listed after the kept entries, as open.
//   - turn: a turn that served no ask and made a tool call: its facts.
//
// Kept in <session>.journal.jsonl, one entry per line, append-only, so a
// resumed session keeps its journal. The root session's only, as the worklog.

// Journal entry kinds.
const (
	JournalFold    = "fold"
	JournalNow     = "now"
	JournalWorklog = "worklog"
)

// Bounds on a /journal now prompt.
const (
	journalEntryMax  = 1200  // one session entry, as the prompt carries it
	journalPromptMax = 24000 // all of them
	journalTitleMax  = 100
)

// JournalEntry is one line of the journal sidecar.
type JournalEntry struct {
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"`
	Title string    `json:"title"`
	// Tokens is the entry's own size (its text, estimated).
	Tokens int `json:"tokens,omitempty"`
	// Path is the file that holds the entry: a fold's span, a worklog
	// account, a /journal now text. Text is the entry when no file holds it.
	Path string `json:"path,omitempty"`
	Text string `json:"text,omitempty"`
	// Cost of the side call that wrote it (now), when the provider said.
	Cost     float64 `json:"cost,omitempty"`
	Currency string  `json:"currency,omitempty"`
	Costed   bool    `json:"costed,omitempty"`
	// Ask and Turn are a ledger entry's facts (kind ask, turn).
	Ask  *AskLedger  `json:"ask,omitempty"`
	Turn *TurnLedger `json:"turn,omitempty"`
}

// JournalFile is the session's journal sidecar.
func JournalFile(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".journal.jsonl"
}

// JournalDir is where /journal now writes its entries' text.
func JournalDir(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".journal"
}

// journalState is the journal of a session with no file (in memory).
type journalState struct {
	mu  sync.Mutex
	mem []JournalEntry
	// nowBusy: a /journal now is running; a second is refused.
	nowBusy bool
}

// journalAdd appends e to the journal. Sub-agents keep none.
func (h *Harness) journalAdd(e JournalEntry) error {
	if h.cfg.Parent != nil {
		return nil
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.Title = workClip(e.Title, journalTitleMax)
	h.journal.mu.Lock()
	defer h.journal.mu.Unlock()
	if h.cfg.SessionFile == "" {
		h.journal.mem = append(h.journal.mem, e)
		return nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(JournalFile(h.cfg.SessionFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Journal is the session's entries, oldest first: entry n is [n-1]. The
// open asks come last, not kept (asks.go).
func (h *Harness) Journal() []JournalEntry {
	h.journal.mu.Lock()
	out := h.journalLocked()
	h.journal.mu.Unlock()
	return append(out, h.openAskEntries()...)
}

func (h *Harness) journalLocked() []JournalEntry {
	if h.cfg.SessionFile == "" {
		return append([]JournalEntry(nil), h.journal.mem...)
	}
	f, err := os.Open(JournalFile(h.cfg.SessionFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []JournalEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var e JournalEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Kind != "" {
			out = append(out, e) // a torn last line is skipped
		}
	}
	return out
}

// Body is the entry's text: its own, or its file's (a fold's summary, not
// the span it replaced).
func (e JournalEntry) Body() string {
	switch {
	case e.Ask != nil:
		return e.Ask.Full()
	case e.Turn != nil:
		return e.Turn.Full()
	}
	if e.Path == "" {
		return e.Text
	}
	b, err := os.ReadFile(e.Path)
	if err != nil {
		if e.Text != "" {
			return e.Text
		}
		return "(the file is gone: " + e.Path + ")"
	}
	s := string(b)
	if e.Kind == JournalFold {
		if _, sum, ok := strings.Cut(s, "\n## Summary\n"); ok {
			sum, _, _ = strings.Cut(sum, "\n## Span\n")
			return strings.TrimSpace(sum)
		}
	}
	return strings.TrimSpace(s)
}

// AskOutcome is an ask entry's outcome (done, filed, dropped, gone), ""
// for an open ask or another kind: the /journal screen folds done and
// filed asks.
func (e JournalEntry) AskOutcome() string {
	if e.Ask == nil || e.Ask.Ended {
		return ""
	}
	return e.Ask.Outcome
}

// Line is the entry as /journal lists it.
func (e JournalEntry) Line(n int) string {
	title := e.Title
	switch {
	case e.Ask != nil:
		title = e.Ask.Row()
	case e.Turn != nil:
		title = e.Turn.Row()
	}
	s := fmt.Sprintf("#%d  %s  %-7s  %s", n, e.At.Local().Format("Jan 2 15:04"), e.Kind, title)
	if e.Tokens > 0 {
		s += " · " + numfmt.TokK(e.Tokens) + " tok"
	}
	if e.Costed {
		s += " · " + numfmt.TurnMoney(e.Cost, e.Currency)
	}
	return s
}

// estimate is a text's tokens under the session's measured ratio.
func (h *Harness) estimate(s string) int { return h.meter.Estimate(s) }

// journalFold is a fold's entry: the span's file when it was written,
// else the summary itself.
func (h *Harness) journalFold(title, summary, path string) {
	e := JournalEntry{Kind: JournalFold, Title: title, Tokens: h.estimate(summary), Path: path}
	if path == "" {
		e.Text = strings.TrimSpace(summary)
	}
	if e.Title == "" {
		e.Title = headLine(summary, journalTitleMax)
	}
	_ = h.journalAdd(e)
}

// JournalCmd runs /journal other than now: "" lists, "view <n>" shows one.
func (h *Harness) JournalCmd(action string) (string, error) {
	verb, rest, _ := strings.Cut(strings.TrimSpace(action), " ")
	all := h.Journal()
	switch strings.ToLower(verb) {
	case "", "list":
		if len(all) == 0 {
			return "journal: empty · folds, asks and turns add entries, and /journal now writes one", nil
		}
		lines := []string{"journal (newest first) · /journal view <n>"}
		for i := len(all) - 1; i >= 0; i-- {
			lines = append(lines, all[i].Line(i+1))
		}
		return strings.Join(lines, "\n"), nil
	case "view":
		n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(rest), "#"))
		if err != nil || n < 1 || n > len(all) {
			return "", fmt.Errorf("no entry %q: /journal lists them", strings.TrimSpace(rest))
		}
		e := all[n-1]
		head := e.Line(n)
		if e.Path != "" {
			head += "\n" + e.Path
		}
		return head + "\n\n" + e.Body(), nil
	}
	return "", fmt.Errorf("usage: /journal [view <n> | now]")
}

const journalInstruction = `Below are a coding agent's task plan and the part of its session since the last journal entry: the person's inputs, the agent's replies, tool calls and results, and notes. Write one journal entry: what the agent did in this span, for someone who was not there.

The first line is a title of at most 10 words, with no heading mark. Then short markdown bullets: what was done, what was found, what was decided, what is left.
Rules:
- Only what the record shows. Never invent a file, command, result or reason.
- Name files, commands and results exactly as they appear.
- Plain and short: no preamble, no closing remarks, no praise.
- Reply with only the entry.

`

// journalSince is the session's entries after t, as prompt lines: bounded
// one by one, then together (the first and the last kept).
func (h *Harness) journalSince(ctx context.Context, t time.Time) []string {
	if h.store == nil {
		return nil
	}
	entries, _ := h.store.Context(ctx, "dun")
	sortByTime(entries)
	var keep []agent.Entry
	for _, e := range entries {
		if e.Kind == agent.KindCompaction || e.CreatedAt <= t.UnixNano() {
			continue
		}
		keep = append(keep, e)
	}
	return boundLines(transcriptLines(keep, journalEntryMax), journalPromptMax)
}

// transcriptLines is entries as a side call's prompt reads them: one per
// entry, its kind (or tag) then its content, each cut at per runes.
func transcriptLines(entries []agent.Entry, per int) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		label := string(e.Kind)
		if e.Tag != "" {
			label = e.Tag
		}
		c := strings.TrimSpace(e.Content)
		if utf8.RuneCountInString(c) > per {
			c = string([]rune(c)[:per]) + " …[cut]"
		}
		out = append(out, "["+label+"] "+c)
	}
	return out
}

// JournalNow is /journal now: one side call writes an entry covering what
// happened since the last entry (or the start), and it is kept.
func (h *Harness) JournalNow(ctx context.Context) (JournalEntry, int, error) {
	h.journal.mu.Lock()
	if h.journal.nowBusy {
		h.journal.mu.Unlock()
		return JournalEntry{}, 0, fmt.Errorf("a journal entry is being written already")
	}
	h.journal.nowBusy = true
	all := h.journalLocked()
	h.journal.mu.Unlock()
	defer func() {
		h.journal.mu.Lock()
		h.journal.nowBusy = false
		h.journal.mu.Unlock()
	}()

	var since time.Time
	if len(all) > 0 {
		since = all[len(all)-1].At
	}
	lines := h.journalSince(ctx, since)
	if len(lines) == 0 {
		return JournalEntry{}, 0, fmt.Errorf("nothing happened since the last entry")
	}
	var b strings.Builder
	b.WriteString(journalInstruction)
	if p := h.planOpen(); p != "" {
		b.WriteString("The plan:\n" + p + "\n\n")
	}
	b.WriteString("The record:\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	at := time.Now()
	text, usage, err := h.askOnce(ctx, "journal", b.String())
	if err != nil {
		return JournalEntry{}, 0, err
	}
	if text == "" {
		return JournalEntry{}, 0, fmt.Errorf("the model wrote no entry")
	}
	title, _, _ := strings.Cut(text, "\n")
	title = strings.TrimSpace(strings.TrimLeft(title, "# "))
	e := JournalEntry{At: at, Kind: JournalNow, Title: title, Tokens: h.estimate(text), Text: text}
	if usage != nil && usage.Cost.Reported {
		e.Cost, e.Currency, e.Costed = usage.Cost.Amount, usage.Cost.Currency, true
	}
	n := len(all) + 1
	if h.cfg.SessionFile != "" {
		path := filepath.Join(JournalDir(h.cfg.SessionFile), strconv.Itoa(n)+".md")
		if err := fsutil.WriteAtomic(path, []byte("# "+title+"\n\n"+text+"\n"), 0o600); err == nil {
			e.Path, e.Text = path, ""
		}
	}
	if err := h.journalAdd(e); err != nil {
		return JournalEntry{}, 0, err
	}
	if e.Path != "" {
		h.journalIndex(ctx, e)
	}
	return e, n, nil
}

// journalIndex puts a /journal now entry in the sessions index beside the
// folds (kind:journal), so session_search finds it as it finds them. Nothing
// without a dund link; a failure is said by the index, once.
func (h *Harness) journalIndex(ctx context.Context, e JournalEntry) {
	x := h.sessionIndex()
	if x == nil {
		return
	}
	_ = x.Put(ctx, agent.SessionDoc{Key: "journal-" + strconv.FormatInt(e.At.UnixNano(), 10), Kind: "journal",
		Name: e.Title, Summary: e.Body()})
}

// journalWorklog is an account /worklog write wrote, as an entry.
func (h *Harness) journalWorklog(t WorkTask, path, text string) {
	_ = h.journalAdd(JournalEntry{Kind: JournalWorklog, Title: fmt.Sprintf("worklog #%d %s", t.N, t.Title),
		Tokens: h.estimate(text), Path: path})
}
