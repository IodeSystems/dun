package dun

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// A context file switched off leaves the block, which names it; it goes into
// the rules index whatever the mode. A mode set at a scope wins over /rules'
// saved one, and summary is written by the model once, without changing the
// project's /rules mode.
func TestContextFiles_Switches(t *testing.T) {
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "root rules @big.md @small.md")
	writeRule(t, filepath.Join(ws, "big.md"), strings.Repeat("a big rule nobody needs every turn. ", 100))
	writeRule(t, filepath.Join(ws, "small.md"), "keep tests green")
	h := rulesHarness(t, ws)
	h.cfg.SessionFile = filepath.Join(t.TempDir(), "s.jsonl")
	off := false
	if _, err := h.setSwitch(h.rulesSwitchKind(), "big.md", ScopeProject, &off); err != nil {
		t.Fatal(err)
	}
	block, mode, set := h.rulesBlock(false)
	if mode != RulesFull || strings.Contains(block, "a big rule") || !strings.Contains(block, "keep tests green") ||
		!strings.Contains(block, "kept out of these instructions") || !strings.Contains(block, "\n- big.md") {
		t.Fatalf("block (%s):\n%s", mode, block)
	}
	if want := rulesIndexWant(set, mode); len(want) != 1 {
		t.Errorf("an off file is indexed in any mode: %v", want)
	}
	v, _ := h.SwitchView(rulesKind)
	if len(v.Items) != 3 || v.Items[0].Source != "root" || v.Items[1].Name != "big.md" || v.Items[1].On || v.Items[1].Tokens < 500 {
		t.Errorf("view: %+v", v.Items)
	}
	r := &cannedRephraseRunner{reply: "SHORT"}
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: r}
	on := true
	if _, err := h.setSwitch(h.rulesSwitchKind(), "big.md", ScopeSession, &on); err != nil {
		t.Fatal(err)
	}
	if _, err := h.setSwitchMode(h.rulesSwitchKind(), RulesSummary, ScopeSession); err != nil || r.called != 1 {
		t.Fatalf("summary mode: %v (calls %d)", err, r.called)
	}
	if block, mode, _ := h.rulesBlock(false); mode != RulesSummary || !strings.Contains(block, "SHORT") {
		t.Errorf("session mode not used: %s\n%s", mode, block)
	}
	if st := h.readRulesState(); st.Mode != "" {
		t.Errorf("the project's /rules mode changed: %q", st.Mode)
	}
}

// /rules is a tree (USER 2026-11-06: "we should have those in a tree, with
// the estimated ctx size, and ... a lower pane that shows our search
// paths"): each import under the file that imports it, depth first, after
// its root; a missing import is a row that says so; the search lines name
// each root file found or not, and the import rule.
func TestContextFiles_TreeAndSearch(t *testing.T) {
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "root @docs/a.md @gone.md")
	writeRule(t, filepath.Join(ws, "docs", "a.md"), "a imports @b.md")
	writeRule(t, filepath.Join(ws, "docs", "b.md"), "b rule")
	writeRule(t, filepath.Join(ws, "CLAUDE.md"), "claude @docs/c.md")
	writeRule(t, filepath.Join(ws, "docs", "c.md"), "c rule")
	h := rulesHarness(t, ws)
	v, _ := h.SwitchView(rulesKind)
	var got []string
	for _, it := range v.Items {
		got = append(got, it.Name+"<"+it.Parent)
	}
	want := []string{"AGENTS.md<", "docs/a.md<AGENTS.md", "docs/b.md<docs/a.md", "gone.md<AGENTS.md", "CLAUDE.md<", "docs/c.md<CLAUDE.md"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("tree:\n got %v\nwant %v", got, want)
	}
	for _, it := range v.Items {
		if it.Name == "gone.md" && (it.Tokens != 0 || !strings.Contains(it.Source, "not found")) {
			t.Errorf("missing import: %+v", it)
		}
	}
	search := strings.Join(v.Search, "\n")
	for _, w := range []string{"✓ AGENTS.md", "· agents.md (not read: AGENTS.md is)", "✓ CLAUDE.md", "@path", "5 deep", ".claude/"} {
		if !strings.Contains(search, w) {
			t.Errorf("search lacks %q:\n%s", w, search)
		}
	}
	if s := strings.Join(rulesSearch(t.TempDir()), "\n"); !strings.Contains(s, "✗ AGENTS.md (not there)") || !strings.Contains(s, "✗ agents.md") || !strings.Contains(s, "✗ CLAUDE.md") {
		t.Errorf("an empty workspace:\n%s", s)
	}
}

// /rules <mode> sets the project's mode in the rules switches and clears the
// mode rules.json held, so a mode has one home per scope; a mode a scope
// sets is what RulesStatus reports.
func TestRules_ModeIsTheSwitchesProjectMode(t *testing.T) {
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "root rules @small.md")
	writeRule(t, filepath.Join(ws, "small.md"), "keep tests green")
	h := rulesHarness(t, ws)
	if err := h.updateRulesState(func(s *rulesState) { s.Mode = RulesSummary }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetRulesMode(t.Context(), RulesFull); err != nil {
		t.Fatal(err)
	}
	v, _ := h.SwitchView(rulesKind)
	if v.Mode != RulesFull || v.ModeFrom != ScopeProject || v.Slash != "rules" {
		t.Errorf("view: mode %q from %q, slash %q", v.Mode, v.ModeFrom, v.Slash)
	}
	if st := h.readRulesState(); st.Mode != "" {
		t.Errorf("rules.json still holds mode %q", st.Mode)
	}
	if got := h.RulesStatus(); !strings.HasPrefix(got, "rules: mode full") {
		t.Errorf("status: %s", got)
	}
}
