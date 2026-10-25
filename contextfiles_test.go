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
	if _, err := h.setSwitch(h.contextFileKind(), "big.md", ScopeProject, &off); err != nil {
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
	v, _ := h.SwitchView(contextFilesKind)
	if len(v.Items) != 3 || v.Items[0].Source != "root" || v.Items[1].Name != "big.md" || v.Items[1].On || v.Items[1].Tokens < 500 {
		t.Errorf("view: %+v", v.Items)
	}
	r := &cannedRephraseRunner{reply: "SHORT"}
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: r}
	on := true
	if _, err := h.setSwitch(h.contextFileKind(), "big.md", ScopeSession, &on); err != nil {
		t.Fatal(err)
	}
	if _, err := h.setSwitchMode(h.contextFileKind(), RulesSummary, ScopeSession); err != nil || r.called != 1 {
		t.Fatalf("summary mode: %v (calls %d)", err, r.called)
	}
	if block, mode, _ := h.rulesBlock(false); mode != RulesSummary || !strings.Contains(block, "SHORT") {
		t.Errorf("session mode not used: %s\n%s", mode, block)
	}
	if st := h.readRulesState(); st.Mode != "" {
		t.Errorf("the project's /rules mode changed: %q", st.Mode)
	}
}
