package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// ledgerRepo is a git repository with one committed file, a.txt.
func ledgerRepo(t *testing.T) string {
	t.Helper()
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t",
		"GIT_COMMITTER_EMAIL": "t@t", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1"} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "a.txt"}, {"commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// startLedger starts an interactive root session in dir that the script
// drives; mut adjusts the config.
func startLedger(t *testing.T, r *nudgeRunner, dir, sessionFile string, mut func(*Config)) *Harness {
	t.Helper()
	cfg := Config{Workspace: dir, Servers: []mcpserver.Server{}, Client: r, Exec: &HostShell{Dir: dir},
		SessionFile: sessionFile, PlanNudgeRounds: -1, PlanNudgeFails: -1, PlanStallRounds: -1}
	if mut != nil {
		mut(&cfg)
	}
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// script is a turn's calls by request index, then text.
func script(calls ...string) func(int, bool) *llm.ToolCall {
	return func(i int, _ bool) *llm.ToolCall {
		if i >= len(calls) {
			return nil
		}
		return planNudgeCall(fmt.Sprintf("c%d", i), calls[i])
	}
}

func journalOf(h *Harness, kind string) []JournalEntry {
	var out []JournalEntry
	for _, e := range h.Journal() {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// A turn with a write, an edit, three commands (one failing) and a commit,
// that closes the person's ask: one ask entry with those facts and counts.
// The session resumed has it, and numbers its next turn 2.
func TestLedger_AskEntryFactsAndResume(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := ledgerRepo(t)
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	r := &nudgeRunner{reply: script(
		`asks {"asks":[{"id":"intake","split":[{"title":"please add b"}]}]}`,
		`file_write {"path":"b.txt","content":"x\ny\n"}`,
		`file_edit {"path":"a.txt","edits":[{"old":"two\n","new":"zwei\ndeux\n"}]}`,
		`exec {"command":"true"}`,
		`exec {"command":"false"}`,
		`exec {"command":"git add -A && git commit -qm 'feat: add b'"}`,
		`asks {"asks":[{"id":1,"state":"done","note":"b.txt added"}]}`,
	)}
	h := startLedger(t, r, dir, sf, nil)
	if _, err := h.Ask(context.Background(), "please add b\nand fix a"); err != nil {
		t.Fatal(err)
	}
	asks := journalOf(h, JournalAsk)
	if len(asks) != 1 || asks[0].Ask == nil {
		t.Fatalf("ask entries %+v", h.Journal())
	}
	a := asks[0].Ask
	f := a.Facts
	if a.Outcome != "done" || a.Note != "b.txt added" || a.Text != "please add b\nand fix a" {
		t.Errorf("ask %+v", a)
	}
	if f.NFiles != 2 || f.Added != 4 || f.Removed != 1 {
		t.Errorf("files %d (+%d/−%d), want 2 (+4/−1): %+v", f.NFiles, f.Added, f.Removed, f.Files)
	}
	if i := slices.IndexFunc(f.Files, func(x LedgerFile) bool { return x.Path == "b.txt" }); i < 0 || !f.Files[i].Created {
		t.Errorf("b.txt not created: %+v", f.Files)
	}
	if f.NCmds != 3 || f.NFailed != 1 || f.Cmds[1].Exit != 1 || !f.Cmds[1].Failed || f.Cmds[1].Command != "false" {
		t.Errorf("cmds %+v", f.Cmds)
	}
	if f.NCommits != 1 || f.Commits[0].Subject != "feat: add b" || len(f.Commits[0].Hash) < 7 {
		t.Errorf("commits %+v", f.Commits)
	}
	if f.ToolCalls != 7 || f.Rounds != 8 || f.Interrupted {
		t.Errorf("totals: %d calls, %d rounds, interrupted %v", f.ToolCalls, f.Rounds, f.Interrupted)
	}
	row := asks[0].Line(1)
	want := `ask 1 · done · "please add b" · turn 1 · 2 files (+4/−1) · 1 commit · 3 cmds (1 failed) · `
	if !strings.Contains(row, want) {
		t.Errorf("row %q\nwant %q", row, want)
	}
	view, err := h.JournalCmd("view 1")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"b.txt +2/−0 created", "a.txt +2/−1", "false · exit 1", "feat: add b", "done ", "b.txt added", "turns (1): 1 ("} {
		if !strings.Contains(view, w) {
			t.Errorf("view lacks %q:\n%s", w, view)
		}
	}
	h.Close()

	h2 := startLedger(t, &nudgeRunner{reply: script(`exec {"command":"true"}`)}, dir, sf, func(c *Config) { c.NoAsks = true })
	t.Cleanup(h2.Close)
	if got := journalOf(h2, JournalAsk); len(got) != 1 || got[0].Ask.Facts.NCommits != 1 {
		t.Fatalf("resumed journal %+v", h2.Journal())
	}
	if _, err := h2.Ask(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if ts := journalOf(h2, JournalTurn); len(ts) != 1 || ts[0].Turn.N != 2 {
		t.Errorf("the resumed session's turn: %+v", ts)
	}
}

// A turn with no tool call gets no entry; one with a call and no ask is a
// turn entry. An interrupted turn says so.
func TestLedger_TurnEntries(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	r := &nudgeRunner{}
	h := startLedger(t, r, dir, filepath.Join(dir, "s.jsonl"), func(c *Config) { c.NoAsks = true })
	t.Cleanup(h.Close)
	if _, err := h.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if j := h.Journal(); len(j) != 0 {
		t.Fatalf("a tool-less turn made %+v", j)
	}
	r.mu.Lock()
	r.choice, r.reply = nil, script(`exec {"command":"echo hi"}`)
	r.mu.Unlock()
	if _, err := h.Ask(context.Background(), "run it"); err != nil {
		t.Fatal(err)
	}
	ts := journalOf(h, JournalTurn)
	if len(ts) != 1 || ts[0].Turn.N != 2 || ts[0].Turn.Facts.NCmds != 1 {
		t.Fatalf("turn entries %+v", h.Journal())
	}
	if row := ts[0].Line(1); !strings.Contains(row, "turn 2 · 1 cmd · ") {
		t.Errorf("row %q", row)
	}
	// Interrupted: the turn's facts are kept and flagged.
	h.ledgerBegin()
	h.ledgerNote(h, "exec", "", "", "sleep 9", nil, "", false, &ledgerCall{set: true, verdict: "killed", failed: true, exit: -1}, time.Second)
	h.ledgerEnd(context.Background(), ErrInterrupted)
	ts = journalOf(h, JournalTurn)
	if len(ts) != 2 || !ts[1].Turn.Facts.Interrupted || !strings.Contains(ts[1].Line(3), "· interrupted ·") ||
		!strings.Contains(ts[1].Body(), "\ninterrupted") {
		t.Errorf("interrupted turn %+v", ts)
	}
}

// Lists stop at the cap and say how many more; the totals count all.
func TestLedger_Caps(t *testing.T) {
	var f LedgerFacts
	for i := range 60 {
		f.addFile(LedgerFile{Path: fmt.Sprintf("f%d.go", i), Added: 1})
		f.Cmds = append(f.Cmds, LedgerCmd{Command: "true"})
		f.NCmds++
	}
	f.capLists()
	if len(f.Files) != ledgerListMax || f.NFiles != 60 || f.Added != 60 || len(f.Cmds) != ledgerListMax || f.NCmds != 60 {
		t.Fatalf("capped: %d files (%d, +%d), %d cmds (%d)", len(f.Files), f.NFiles, f.Added, len(f.Cmds), f.NCmds)
	}
	text := f.Text()
	if strings.Count(text, "… +10 more") != 2 {
		t.Errorf("no \"+10 more\" for both lists:\n%s", text)
	}
	if s := f.Summary(); s != "60 files (+60/−0) · 60 cmds" {
		t.Errorf("summary %q", s)
	}
}

func TestLedger_LineDiff(t *testing.T) {
	for _, c := range []struct {
		a, b   string
		ad, rm int
	}{
		{"", "x\ny\n", 2, 0},
		{"a\nb\nc\n", "a\nc\n", 0, 1},
		{"a\nb\nc\n", "a\nB\nc\nd\n", 2, 1},
		{"a\nb\n", "a\nb\n", 0, 0},
		{"1\n2\n3\n4\n", "4\n3\n2\n1\n", 3, 3},
	} {
		if ad, rm := lineDiff(c.a, c.b); ad != c.ad || rm != c.rm {
			t.Errorf("lineDiff(%q, %q) = +%d −%d, want +%d −%d", c.a, c.b, ad, rm, c.ad, c.rm)
		}
	}
}

// ── asks ────────────────────────────────────────────────────────────────

// A person's message, acknowledged, is an ask in the ledger, not a plan
// node; an automatic one never reaches the intake. The turn-end check names
// the intake.
func TestAsks_NodesAndTheTurnEndCheck(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	h := startLedger(t, &nudgeRunner{}, dir, filepath.Join(dir, "s.jsonl"), nil)
	t.Cleanup(h.Close)
	if !h.askOpen("fix the flaky test\nit fails one run in ten") {
		t.Fatal("a person's message is not an ask")
	}
	for _, auto := range []string{"[automatic] resume after the upgrade", "[control/plan] Before you stop", "[settle] The user has been away"} {
		if h.intakeAdd(auto) {
			t.Errorf("%q went on the intake", auto)
		}
	}
	h.askOpen("also update the docs")
	if asks, _ := asksBy(h); len(asks) != 2 || asks[1].Title != "fix the flaky test" || asks[2].Title != "also update the docs" || len(h.Plan().Rows) != 0 {
		t.Fatalf("asks %+v, plan rows %+v", asks, h.Plan().Rows)
	}
	h.turnBegin(originPerson)
	h.SayAs(NewMessageID(), "and the changelog")
	note, _ := h.idleNote(1)
	if !strings.Contains(note, `- intake: 1 message, not acknowledged: "and the changelog"`) || strings.Contains(note, "1 ask:") {
		t.Errorf("the turn-end check:\n%s", note)
	}
	if got := h.asksOpenNow(); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("open asks %v", got)
	}
	if h.cfg.NonInteractive = true; h.intakeAdd("a task") {
		t.Error("--non-interactive took a message")
	}
}

// Facts go to the ask the turn's plan changes worked under; with no sign,
// to every open ask, marked shared. A dropped ask gets its entry with why;
// an open one is listed as open, and gets one entry when the session ends.
func TestAsks_AttributionDropAndSessionEnd(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	sf := filepath.Join(dir, "s.jsonl")
	h := startLedger(t, &nudgeRunner{}, dir, sf, nil)
	h.askOpen("fix the flaky test")
	h.askOpen("update the docs")
	edit := func(path string) {
		h.ledgerNote(h, "file_edit", "edit", path, "", nil, "edited "+path+": +3 -1", false, &ledgerCall{}, time.Millisecond)
	}
	// A node that serves ask 2, made outside a turn.
	planCall(t, h, "plan_add", `{"title":"write the page","done_when":"written"}`)
	h.asksCall(`{"asks":[{"id":2,"nodes":["1"]}]}`)
	// Turn 1: a change to that node, and an edit: ask 2's only.
	h.ledgerBegin()
	planCall(t, h, "plan_update", `{"id":"1","details":"the docs page"}`)
	edit("docs/a.md")
	h.ledgerEnd(context.Background(), nil)
	// Turn 2: no plan change: both asks', shared.
	h.ledgerBegin()
	edit("x.go")
	h.ledgerEnd(context.Background(), nil)

	open := journalOf(h, JournalAsk)
	if len(open) != 2 || open[0].Ask.outcomeWord() != "open" {
		t.Fatalf("open asks as listed: %+v", open)
	}
	a1, a2 := open[0].Ask, open[1].Ask
	if a1.NTurns != 1 || a1.Turns[0].N != 2 || !a1.Turns[0].Shared || a1.Facts.NFiles != 1 || a1.Facts.Files[0].Path != "x.go" {
		t.Errorf("ask 1: %+v", a1)
	}
	if a2.NTurns != 2 || a2.Turns[0].Shared || !a2.Turns[1].Shared || a2.Facts.NFiles != 2 || a2.Shared != 1 {
		t.Errorf("ask 2: %+v", a2)
	}
	if !strings.Contains(open[1].Line(2), `ask 2 · open · "update the docs" · turns 1–2 · 2 files (+6/−2)`) {
		t.Errorf("row %q", open[1].Line(2))
	}
	// Drop ask 1; the next turn's end writes its entry.
	h.asksCall(`{"asks":[{"id":1,"state":"done","note":"cannot reproduce"}]}`)
	h.ledgerBegin()
	h.ledgerEnd(context.Background(), nil)
	kept := h.journalLocked()
	if len(kept) != 1 || kept[0].Ask == nil || kept[0].Ask.Outcome != "done" || kept[0].Ask.Note != "cannot reproduce" {
		t.Fatalf("kept %+v", kept)
	}
	if !strings.Contains(kept[0].Line(1), `ask 1 · done · "fix the flaky test"`) {
		t.Errorf("row %q", kept[0].Line(1))
	}
	// The session ends with ask 2 open: one entry, once.
	h.Close()
	h.asksSessionEnd()
	kept = h.journalLocked()
	if len(kept) != 2 || kept[1].Ask.N != 2 || !kept[1].Ask.Ended || !strings.Contains(kept[1].Line(2), "open at session end") {
		t.Fatalf("session end: %+v", kept)
	}
}

// The ask entry goes into the sessions index as kind:ask.
func TestAsks_Indexed(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	h := startLedger(t, &nudgeRunner{}, dir, sf, func(c *Config) { c.Rag = &fakeRag{} })
	t.Cleanup(h.Close)
	h.askOpen("fix the flaky test")
	h.ledgerBegin()
	h.ledgerNote(h, "file_edit", "edit", "t.go", "", nil, "edited t.go: +1 -1", false, &ledgerCall{}, time.Millisecond)
	h.asksCall(`{"asks":[{"id":1,"state":"done","note":"retry added"}]}`)
	h.ledgerEnd(context.Background(), nil)
	files, _ := filepath.Glob(filepath.Join(foldsDir(sf), "ask-1-*.md"))
	if len(files) != 1 {
		t.Fatalf("index files %v", files)
	}
	b, _ := os.ReadFile(files[0])
	for _, w := range []string{"kind: ask", "t.go +1/−1", "retry added"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("indexed doc lacks %q:\n%s", w, b)
		}
	}
}
