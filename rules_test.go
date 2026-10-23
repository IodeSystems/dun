package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// The project rules are a share of the system prompt, counted marginally
// when the endpoint counts, estimated when it does not, with their mode.
func TestBreakdownCountsTheRules(t *testing.T) {
	sys, defs, mcp := fixture()
	rules := " rule one two three four"
	bd := measureSystem(context.Background(), &fakeCounter{}, sys+rules, rules, RulesSummary, defs, mcp)
	if !bd.Exact || bd.Rules != 5 || bd.RulesMode != RulesSummary {
		t.Errorf("exact: rules %d mode %q", bd.Rules, bd.RulesMode)
	}
	bd = measureSystem(context.Background(), nonCounter{}, sys+rules, rules, RulesFull, defs, mcp)
	if bd.Exact || bd.Rules != len(rules)/4 || bd.RulesMode != RulesFull {
		t.Errorf("estimate: rules %d mode %q", bd.Rules, bd.RulesMode)
	}
	if bd := measureSystem(context.Background(), nonCounter{}, sys, "", "", defs, mcp); bd.RulesMode != "" {
		t.Error("no rules, no mode")
	}
}

// rootRules is the block in mode full, as the system prompt gets it.
func rootRules(ws string) string { return loadRules(ws).render(RulesFull, "") }

func writeRule(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func importNames(set rulesSet) []string {
	var out []string
	for _, d := range set.Imports {
		s := d.Name
		if d.Note != "" {
			s += " [" + d.Note + "]"
		}
		out = append(out, s)
	}
	return out
}

func TestImportPaths(t *testing.T) {
	text := "See @docs/style.md and @~/notes/x.md.\n" +
		"Ask @carl, mail carl@example.com.\n" +
		"Inline `@not/this.md` but @yes/this.md\n" +
		"```\n@in/fence.md\n```\n" +
		"~~~~\n@in/tilde.md\n~~~\n~~~~\n" +
		"(see @paren/x.md) @/abs/y.md @./dot.md"
	got := strings.Join(importPaths(text), " ")
	want := "docs/style.md ~/notes/x.md yes/this.md paren/x.md /abs/y.md ./dot.md"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestLoadRules_Imports(t *testing.T) {
	home := t.TempDir()
	defer func(f func() (string, error)) { userHomeDir = f }(userHomeDir)
	userHomeDir = func() (string, error) { return home, nil }
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "root rules\n@docs/a.md\n@~/global.md\n@gone.md\n```\n@docs/fenced.md\n```")
	writeRule(t, filepath.Join(ws, "docs", "a.md"), "A says @sub/b.md and @../AGENTS.md") // relative to docs/; back to root: a cycle
	writeRule(t, filepath.Join(ws, "docs", "sub", "b.md"), "B fact: @a2.md")
	writeRule(t, filepath.Join(ws, "docs", "sub", "a2.md"), "A2 fact")
	writeRule(t, filepath.Join(ws, "docs", "fenced.md"), "must not appear")
	writeRule(t, filepath.Join(home, "global.md"), "home rule")

	set := loadRules(ws)
	got := strings.Join(importNames(set), ", ")
	want := "docs/a.md, docs/sub/b.md, docs/sub/a2.md, ~/global.md, gone.md [not found]"
	if got != want {
		t.Errorf("imports: %s\nwant      %s", got, want)
	}
	block := set.render(RulesFull, "")
	for _, s := range []string{"root rules", "### docs/sub/b.md (imported by docs/a.md)", "A2 fact", "home rule", "(gone.md, imported by AGENTS.md: not found)"} {
		if !strings.Contains(block, s) {
			t.Errorf("block lacks %q:\n%s", s, block)
		}
	}
	if strings.Contains(block, "must not appear") || strings.Count(block, "root rules") != 1 {
		t.Errorf("fenced import expanded, or the cycle repeated the root:\n%s", block)
	}
}

func TestLoadRules_DepthCap(t *testing.T) {
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "@f1.md")
	for i := 1; i <= 7; i++ {
		writeRule(t, filepath.Join(ws, "f"+string(rune('0'+i))+".md"), "level "+string(rune('0'+i))+" @f"+string(rune('0'+i+1))+".md")
	}
	set := loadRules(ws)
	got := strings.Join(importNames(set), ", ")
	want := "f1.md, f2.md, f3.md, f4.md, f5.md, f6.md [not read: imports nest deeper than 5]"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestLoadRules_SizeCaps(t *testing.T) {
	ws := t.TempDir()
	var imports []string
	for i := range 5 {
		name := "big" + string(rune('a'+i)) + ".md"
		imports = append(imports, "@"+name)
		writeRule(t, filepath.Join(ws, name), strings.Repeat("x", rulesFileMax+100))
	}
	writeRule(t, filepath.Join(ws, "AGENTS.md"), strings.Join(imports, "\n"))
	set := loadRules(ws)
	total := 0
	for _, d := range append(set.Roots, set.Imports...) {
		total += len(d.Text)
		if len(d.Text) > rulesFileMax {
			t.Errorf("%s: %d bytes past the file cap", d.Name, len(d.Text))
		}
	}
	if total > rulesTotalMax {
		t.Errorf("total %d past the cap", total)
	}
	if !strings.Contains(set.Imports[0].Note, "cut: the first 64 KiB") {
		t.Errorf("first file note: %q", set.Imports[0].Note)
	}
	last := set.Imports[len(set.Imports)-1]
	if last.Text != "" || !strings.Contains(last.Note, "256 KiB total") {
		t.Errorf("the file past the total should be a note: %+v", last.Note)
	}
	if !strings.Contains(set.render(RulesFull, ""), "cut: the first 64 KiB") {
		t.Error("the cut is not said in the block")
	}
}

func TestLoadRules_AgentsAndClaude(t *testing.T) {
	// Both, different: both, AGENTS.md first.
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "agents rule")
	writeRule(t, filepath.Join(ws, "CLAUDE.md"), "claude rule")
	b := rootRules(ws)
	if !strings.Contains(b, "## Project rules (AGENTS.md, CLAUDE.md)") || strings.Index(b, "agents rule") > strings.Index(b, "claude rule") {
		t.Errorf("both:\n%s", b)
	}
	// A symlink: once.
	ws = t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "one rule")
	if err := os.Symlink("AGENTS.md", filepath.Join(ws, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if b := rootRules(ws); strings.Count(b, "one rule") != 1 || !strings.Contains(b, "(also CLAUDE.md)") {
		t.Errorf("symlink:\n%s", b)
	}
	// Identical bytes: once.
	ws = t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "same rule")
	writeRule(t, filepath.Join(ws, "CLAUDE.md"), "same rule")
	if b := rootRules(ws); strings.Count(b, "same rule") != 1 {
		t.Errorf("identical:\n%s", b)
	}
	// CLAUDE.md that imports AGENTS.md (the usual way to share one file): once.
	ws = t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "shared rule")
	writeRule(t, filepath.Join(ws, "CLAUDE.md"), "@AGENTS.md")
	if b := rootRules(ws); strings.Count(b, "shared rule") != 1 {
		t.Errorf("import of the other root:\n%s", b)
	}
	// CLAUDE.md alone.
	ws = t.TempDir()
	writeRule(t, filepath.Join(ws, "CLAUDE.md"), "only claude")
	if b := rootRules(ws); !strings.Contains(b, "only claude") || !strings.Contains(b, "(CLAUDE.md)") {
		t.Errorf("claude alone:\n%s", b)
	}
}

func TestRulesRender_Modes(t *testing.T) {
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "root verbatim @x.md @y.md")
	writeRule(t, filepath.Join(ws, "x.md"), "x full text")
	set := loadRules(ws)
	sum := set.render(RulesSummary, "the condensed text")
	if !strings.Contains(sum, "root verbatim") || !strings.Contains(sum, "the condensed text") || strings.Contains(sum, "x full text") || !strings.Contains(sum, "- y.md (not found)") {
		t.Errorf("summary:\n%s", sum)
	}
	idx := set.render(RulesIndex, "")
	if !strings.Contains(idx, "root verbatim") || !strings.Contains(idx, "- x.md") || strings.Contains(idx, "x full text") || !strings.Contains(idx, "search tool") {
		t.Errorf("index:\n%s", idx)
	}
}

// rulesHarness is a harness on a workspace with a project state dir.
func rulesHarness(t *testing.T, ws string) *Harness {
	t.Helper()
	t.Setenv("DUN_HOME", filepath.Join(t.TempDir(), ".dun"))
	h := newNoteHarness(t)
	h.cfg.Workspace = ws
	h.mgr = mcpmgr.NewManager()
	return h
}

// The mode is saved per project and survives a new harness; a summary
// is kept until the imports change, and then the block falls back to full.
func TestRulesMode_PersistAndSummaryInvalidation(t *testing.T) {
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "root @long.md")
	writeRule(t, filepath.Join(ws, "long.md"), strings.Repeat("a long rule that says little. ", 200))
	h := rulesHarness(t, ws)
	r := &cannedRephraseRunner{reply: "SHORT SUMMARY"}
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: r}

	if _, err := h.SetRulesMode(context.Background(), RulesIndex); err == nil || !strings.Contains(err.Error(), "/raglit on") {
		t.Errorf("index with rag off: %v", err)
	}
	msg, err := h.SetRulesMode(context.Background(), RulesSummary)
	if err != nil || r.called != 1 || !strings.Contains(msg, "mode summary") {
		t.Fatalf("summary: %q %v (calls %d)", msg, err, r.called)
	}
	// A new harness on the same project reads the saved mode.
	h2 := newNoteHarness(t)
	h2.cfg.Workspace, h2.mgr = ws, mcpmgr.NewManager()
	block, mode, _ := h2.rulesBlock(false)
	if mode != RulesSummary || !strings.Contains(block, "SHORT SUMMARY") || strings.Contains(block, "a long rule") {
		t.Fatalf("saved mode not used: %s\n%s", mode, block)
	}
	// Asking again with the same sources does not ask the model again.
	if _, err := h.SetRulesMode(context.Background(), RulesSummary); err != nil || r.called != 1 {
		t.Errorf("the cached summary was not reused: %v, calls %d", err, r.called)
	}
	// An import changes: full again, and the status says why.
	writeRule(t, filepath.Join(ws, "long.md"), strings.Repeat("a changed rule. ", 200))
	block, mode, _ = h2.rulesBlock(false)
	if mode != RulesFull || !strings.Contains(block, "a changed rule") {
		t.Errorf("stale summary used: %s", mode)
	}
	if st := h2.RulesStatus(); !strings.Contains(st, "saved: summary") || !strings.Contains(st, "out of date") {
		t.Errorf("status: %s", st)
	}
	// Back to full is saved too.
	if _, err := h.SetRulesMode(context.Background(), RulesFull); err != nil {
		t.Fatal(err)
	}
	if st := h.readRulesState(); st.Mode != "" {
		t.Errorf("full should save as the default: %+v", st)
	}
}

// A summary that is not smaller is refused, and the mode stays full.
func TestRulesSummary_NotSmallerIsRefused(t *testing.T) {
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "@a.md")
	writeRule(t, filepath.Join(ws, "a.md"), "tiny")
	h := rulesHarness(t, ws)
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: &cannedRephraseRunner{reply: strings.Repeat("longer ", 100)}}
	if _, err := h.SetRulesMode(context.Background(), RulesSummary); err == nil {
		t.Fatal("a larger summary was accepted")
	}
	if st := h.readRulesState(); st.Mode != "" || st.Summary != nil {
		t.Errorf("state changed: %+v", st)
	}
}

// The offer is made once per content, only past the threshold, and only
// when there are imports to make cheaper.
func TestRulesOffer(t *testing.T) {
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "@big.md")
	writeRule(t, filepath.Join(ws, "big.md"), strings.Repeat("rule ", rulesOfferTokens))
	h := rulesHarness(t, ws)
	var offers []string
	h.cfg.OnRules = func(m string) { offers = append(offers, m) }
	h.rulesBlock(false)
	h.rulesBlock(false)
	if len(offers) != 1 || !strings.Contains(offers[0], "/rules summary") {
		t.Fatalf("offers: %q", offers)
	}
	writeRule(t, filepath.Join(ws, "big.md"), strings.Repeat("rule! ", rulesOfferTokens))
	h.rulesBlock(false)
	if len(offers) != 2 {
		t.Errorf("a content change should offer again: %d", len(offers))
	}
	// Small, or no imports: no offer.
	ws2 := t.TempDir()
	writeRule(t, filepath.Join(ws2, "AGENTS.md"), strings.Repeat("rule ", rulesOfferTokens))
	h2 := rulesHarness(t, ws2)
	h2.cfg.OnRules = func(m string) { offers = append(offers, m) }
	h2.rulesBlock(false)
	if len(offers) != 2 {
		t.Errorf("no imports, nothing to offer: %q", offers[2:])
	}
}

// Index mode puts the imports in the rules index, and leaving it forgets
// them — from a later process too, since what is indexed is saved.
func TestSyncRulesIndex(t *testing.T) {
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "@a.md @b.md")
	writeRule(t, filepath.Join(ws, "a.md"), "A")
	writeRule(t, filepath.Join(ws, "b.md"), "B")
	h := rulesHarness(t, ws)
	var log []string
	sink := &mcpDocsSink{
		ingest: func(index string, paths []string) error {
			for _, p := range paths {
				log = append(log, "ingest "+index+" "+filepath.Base(p))
			}
			return nil
		},
		forget: func(index string, paths []string) error {
			for _, p := range paths {
				log = append(log, "forget "+index+" "+filepath.Base(p))
			}
			return nil
		},
	}
	step := func(h *Harness, mode, want string) {
		t.Helper()
		log = nil
		h.syncRulesIndex(sink, loadRules(ws), mode)
		if got := strings.Join(log, "; "); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	step(h, RulesIndex, "ingest rules a.md; ingest rules b.md")
	if !h.isRulesDoc(filepath.Join(ws, "a.md")) {
		t.Error("an indexed import is not known for surfacing")
	}
	step(h, RulesIndex, "")
	os.Remove(filepath.Join(ws, "b.md"))
	step(h, RulesIndex, "forget rules b.md")
	h2 := newNoteHarness(t)
	h2.cfg.Workspace, h2.mgr = ws, mcpmgr.NewManager()
	step(h2, RulesFull, "forget rules a.md")
	if h2.isRulesDoc(filepath.Join(ws, "a.md")) {
		t.Error("a forgotten import is still known")
	}
}
