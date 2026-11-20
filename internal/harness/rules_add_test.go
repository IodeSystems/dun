package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
)

// /rules add makes a file a root file at each scope: in the block, a row
// with its scope, kept relative to the workspace inside it and absolute
// outside; remove takes it back, and is refused where it was not added.
func TestRulesAdd_Scopes(t *testing.T) {
	home := userHome(t)
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "root rules")
	writeRule(t, filepath.Join(ws, "PARA.md"), "session para rule")
	writeRule(t, filepath.Join(ws, "docs", "proj.md"), "project rule")
	writeRule(t, filepath.Join(home, "glob.md"), "global rule")
	h := rulesHarness(t, ws)
	h.cfg.SessionFile = filepath.Join(t.TempDir(), "s.jsonl")
	for _, c := range []struct{ path, scope string }{{"./PARA.md", mcpserver.ScopeSession}, {"docs/proj.md", mcpserver.ScopeProject}, {"~/glob.md", mcpserver.ScopeGlobal}} {
		msg, err := h.AddRule(c.path, c.scope)
		if err != nil || !strings.Contains(msg, "added for") {
			t.Fatalf("add %s: %q %v", c.path, msg, err)
		}
	}
	f := loadSwitchFile(h.switchFile(rulesKind, mcpserver.ScopeSession))
	if len(f.Add) != 1 || f.Add[0] != "PARA.md" {
		t.Errorf("session file: %+v", f)
	}
	if g := loadSwitchFile(h.switchFile(rulesKind, mcpserver.ScopeGlobal)); len(g.Add) != 1 || g.Add[0] != filepath.Join(home, "glob.md") {
		t.Errorf("global file: %+v", g)
	}
	// The project's file is read from its worktrees and subdirectories too.
	if p := loadSwitchFile(h.switchFile(rulesKind, mcpserver.ScopeProject)); len(p.Add) != 1 || p.Add[0] != filepath.Join(ws, "docs", "proj.md") {
		t.Errorf("project file: %+v", p)
	}
	block, _, _ := h.rulesBlock(false)
	before(t, block, "## User rules", "global rule", "## Project rules", "root rules", "session para rule", "project rule")
	set := h.loadRules()
	for _, d := range set.Roots {
		if d.Name == "~/glob.md" && !d.User || d.Name == "PARA.md" && d.User {
			t.Errorf("budget group: %+v", d)
		}
	}
	v, _ := h.SwitchView(rulesKind)
	src := map[string]string{}
	for _, it := range v.Items {
		src[it.Name] = it.Source
	}
	if src["PARA.md"] != "added · session" || src["docs/proj.md"] != "added · project" || src["~/glob.md"] != "added · global" {
		t.Errorf("rows: %v", src)
	}
	if s := h.RulesStatus(); !strings.Contains(s, "added · session") || !strings.Contains(s, "/rules add|remove") {
		t.Errorf("status:\n%s", s)
	}
	// Switched by its name, as any root file.
	off := false
	if _, err := h.setSwitch(h.rulesSwitchKind(), "PARA.md", mcpserver.ScopeSession, &off); err != nil {
		t.Fatal(err)
	}
	if block, _, _ := h.rulesBlock(false); strings.Contains(block, "session para rule") || !strings.Contains(block, "\n- PARA.md") {
		t.Errorf("off:\n%s", block)
	}
	if _, err := h.RemoveRule("PARA.md", mcpserver.ScopeProject); err == nil {
		t.Error("remove at a scope it was not added at")
	}
	if msg, err := h.RemoveRule(filepath.Join(ws, "PARA.md"), mcpserver.ScopeSession); err != nil || !strings.Contains(msg, "taken back") {
		t.Fatalf("remove: %q %v", msg, err)
	}
	if _, err := h.RemoveRule("~/glob.md", mcpserver.ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	set = h.loadRules()
	if len(set.Roots) != 2 || set.Roots[1].Name != "docs/proj.md" {
		t.Errorf("after remove: %+v", set.Roots)
	}
}

// A session add is kept in the session's sidecar, so a new harness on the
// same session file (a resume) reads it again.
func TestRulesAdd_SessionSurvivesResume(t *testing.T) {
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "PARA.md"), "para rule")
	h := rulesHarness(t, ws)
	sess := filepath.Join(t.TempDir(), "s.jsonl")
	h.cfg.SessionFile = sess
	if _, err := h.AddRule("PARA.md", mcpserver.ScopeSession); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(strings.TrimSuffix(sess, ".jsonl") + ".context-files.json")
	if err != nil {
		t.Fatal(err)
	}
	var f SwitchFile
	if json.Unmarshal(b, &f) != nil || len(f.Add) != 1 {
		t.Fatalf("sidecar: %s", b)
	}
	h2 := newNoteHarness(t)
	h2.cfg.Workspace, h2.cfg.SessionFile, h2.mgr = ws, sess, h.mgr
	if block, _, _ := h2.rulesBlock(false); !strings.Contains(block, "para rule") {
		t.Errorf("resumed block:\n%s", block)
	}
}

// An add of a file already in the block (a root file, an import) is a
// no-op that says so; an add of a file that is not there is kept, said, and
// listed with a note.
func TestRulesAdd_DedupAndMissing(t *testing.T) {
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "root @docs/a.md")
	writeRule(t, filepath.Join(ws, "docs", "a.md"), "a rule")
	h := rulesHarness(t, ws)
	h.cfg.SessionFile = filepath.Join(t.TempDir(), "s.jsonl")
	for p, want := range map[string]string{"AGENTS.md": "a root file", "docs/a.md": "imported by AGENTS.md"} {
		msg, err := h.AddRule(p, mcpserver.ScopeSession)
		if err != nil || !strings.Contains(msg, "already loaded") || !strings.Contains(msg, want) {
			t.Errorf("add %s: %q %v", p, msg, err)
		}
	}
	if f := loadSwitchFile(h.switchFile(rulesKind, mcpserver.ScopeSession)); len(f.Add) != 0 {
		t.Errorf("a no-op was kept: %+v", f)
	}
	msg, err := h.AddRule("later.md", mcpserver.ScopeSession)
	if err != nil || !strings.Contains(msg, "not there") {
		t.Fatalf("missing: %q %v", msg, err)
	}
	set := h.loadRules()
	last := set.Roots[len(set.Roots)-1]
	if last.Name != "later.md" || last.Note != "not found" || last.Added != mcpserver.ScopeSession {
		t.Errorf("missing root: %+v", last)
	}
	if block, _, _ := h.rulesBlock(false); !strings.Contains(block, "(later.md, added with /rules add: not found)") {
		t.Errorf("block:\n%s", block)
	}
	v, _ := h.SwitchView(rulesKind)
	for _, it := range v.Items {
		if it.Name == "later.md" && (it.Tokens != 0 || it.Source != "added · session · not found") {
			t.Errorf("row: %+v", it)
		}
	}
	if s := h.RulesStatus(); !strings.Contains(s, "not found  added · session") {
		t.Errorf("status:\n%s", s)
	}
}

// ~/CLAUDE.md that only imports ~/AGENTS.md is its own small row; it says
// the file it includes was already loaded, so it does not read as a copy.
func TestRules_IncludesAlreadyLoaded(t *testing.T) {
	home := userHome(t)
	ws := t.TempDir()
	writeRule(t, filepath.Join(home, "AGENTS.md"), "the user's rules")
	writeRule(t, filepath.Join(home, "CLAUDE.md"), "@AGENTS.md")
	h := rulesHarness(t, ws)
	set := h.loadRules()
	if len(set.Roots) != 2 || len(set.Imports) != 0 || set.Roots[1].includesNote(set) != "includes ~/AGENTS.md (already loaded)" {
		t.Fatalf("roots %+v imports %+v", set.Roots, set.Imports)
	}
	v, _ := h.SwitchView(rulesKind)
	found := false
	for _, it := range v.Items {
		if it.Name == "~/CLAUDE.md" {
			found = it.Source == "user · includes ~/AGENTS.md (already loaded)"
		}
	}
	if !found {
		t.Errorf("rows: %+v", v.Items)
	}
	if s := h.RulesStatus(); !strings.Contains(s, "includes ~/AGENTS.md (already loaded)") {
		t.Errorf("status:\n%s", s)
	}
}

// The include note claims "already loaded" only for a file that is in the
// block: not for one switched off, nor one not found.
func TestRules_IncludesNoteOnlyWhenLoaded(t *testing.T) {
	home := userHome(t)
	ws := t.TempDir()
	writeRule(t, filepath.Join(home, "AGENTS.md"), "the user's rules @gone.md")
	writeRule(t, filepath.Join(home, "CLAUDE.md"), "@AGENTS.md @gone.md")
	h := rulesHarness(t, ws)
	h.cfg.SessionFile = filepath.Join(t.TempDir(), "s.jsonl")
	row := func() string {
		v, _ := h.SwitchView(rulesKind)
		for _, it := range v.Items {
			if it.Name == "~/CLAUDE.md" {
				return it.Source
			}
		}
		return "no row"
	}
	if got := row(); got != "user · includes ~/AGENTS.md (already loaded)" {
		t.Errorf("a missing include named as loaded: %q", got)
	}
	off := false
	if _, err := h.setSwitch(h.rulesSwitchKind(), "~/AGENTS.md", mcpserver.ScopeSession, &off); err != nil {
		t.Fatal(err)
	}
	if got := row(); got != "user" {
		t.Errorf("an include switched off named as loaded: %q", got)
	}
	if s := h.RulesStatus(); strings.Contains(s, "already loaded") {
		t.Errorf("status:\n%s", s)
	}
}

// DUN_USER_RULES=0 leaves the global adds out with the user files; a global
// add is not refused because this project imports the file; an add with the
// same bytes as a root file goes in once; remove matches the real path.
func TestRulesAdd_GlobalUserOffDupBytesRealPath(t *testing.T) {
	home := userHome(t)
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "root @shared.md")
	writeRule(t, filepath.Join(home, "shared.md"), "shared rule")
	if err := os.Symlink(filepath.Join(home, "shared.md"), filepath.Join(ws, "shared.md")); err != nil {
		t.Fatal(err)
	}
	writeRule(t, filepath.Join(ws, "copy.md"), "root @shared.md")
	h := rulesHarness(t, ws)
	h.cfg.SessionFile = filepath.Join(t.TempDir(), "s.jsonl")
	if msg, err := h.AddRule("~/shared.md", mcpserver.ScopeGlobal); err != nil || !strings.Contains(msg, "added for") {
		t.Fatalf("global add of a file the project imports: %q %v", msg, err)
	}
	if set := h.loadRules(); len(set.Roots) != 2 || set.Roots[0].Name != "~/shared.md" || !set.Roots[0].User {
		t.Errorf("global add: %+v", set.Roots)
	}
	h.cfg.NoUserRules = true
	for _, d := range h.loadRules().Roots {
		if d.Added == mcpserver.ScopeGlobal {
			t.Errorf("DUN_USER_RULES=0 read a global add: %+v", d)
		}
	}
	h.cfg.NoUserRules = false
	// The symlink in the workspace is the same file.
	if _, err := h.RemoveRule("shared.md", mcpserver.ScopeGlobal); err != nil {
		t.Fatalf("remove by another path to the same file: %v", err)
	}
	if _, err := h.AddRule("copy.md", mcpserver.ScopeSession); err != nil {
		t.Fatal(err)
	}
	if set := h.loadRules(); len(set.Roots) != 1 {
		t.Errorf("same bytes as a root, in twice: %+v", set.Roots)
	}
}

// /rules add ./path at global scope (USER 2026-11-20) is kept as typed and
// names each project's own file; a missing one is a row with a note; a path
// without ./ is still absolute; remove takes the ./ entry back.
func TestRulesAdd_DotSlashNamesEachProjectsFile(t *testing.T) {
	userHome(t)
	wsA, wsB := t.TempDir(), t.TempDir()
	writeRule(t, filepath.Join(wsA, "AGENTS.local.md"), "rule of A")
	h := rulesHarness(t, wsA)
	h.cfg.SessionFile = filepath.Join(t.TempDir(), "s.jsonl")
	msg, err := h.AddRule("./AGENTS.local.md", mcpserver.ScopeGlobal)
	if err != nil || !strings.Contains(msg, "each project's") {
		t.Fatalf("add: %q %v", msg, err)
	}
	if g := loadSwitchFile(h.switchFile(rulesKind, mcpserver.ScopeGlobal)); len(g.Add) != 1 || g.Add[0] != "./AGENTS.local.md" {
		t.Errorf("global file: %+v", g)
	}
	find := func() rulesDoc {
		for _, d := range h.loadRules().Roots {
			if d.Added == mcpserver.ScopeGlobal {
				return d
			}
		}
		t.Fatal("no global add in the block")
		return rulesDoc{}
	}
	if d := find(); !strings.Contains(d.Text, "rule of A") || !d.PerWorkspace || d.Path != filepath.Join(wsA, "AGENTS.local.md") {
		t.Errorf("A: %+v", d)
	}
	// Another workspace: its own file, or a note where it has none.
	h.cfg.Workspace = wsB
	if d := find(); d.Note != "not found" || d.Path != filepath.Join(wsB, "AGENTS.local.md") {
		t.Errorf("B without the file: %+v", d)
	}
	writeRule(t, filepath.Join(wsB, "AGENTS.local.md"), "rule of B")
	if d := find(); !strings.Contains(d.Text, "rule of B") || strings.Contains(d.Text, "rule of A") {
		t.Errorf("B: %+v", d)
	}
	v, _ := h.SwitchView(rulesKind)
	for _, it := range v.Items {
		if it.Name == "AGENTS.local.md" && !strings.Contains(it.Source, "(each project's)") {
			t.Errorf("row source: %q", it.Source)
		}
	}
	// A ./ file that is a root file already is "already loaded", per workspace.
	writeRule(t, filepath.Join(wsB, "AGENTS.md"), "root B")
	if msg, err := h.AddRule("./AGENTS.md", mcpserver.ScopeProject); err != nil || !strings.Contains(msg, "already loaded") {
		t.Errorf("dup: %q %v", msg, err)
	}
	// No ./: absolute, as before.
	writeRule(t, filepath.Join(wsB, "plain.md"), "plain")
	if _, err := h.AddRule("plain.md", mcpserver.ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	if g := loadSwitchFile(h.switchFile(rulesKind, mcpserver.ScopeGlobal)); len(g.Add) != 2 || g.Add[1] != filepath.Join(wsB, "plain.md") {
		t.Errorf("plain relative path: %+v", g)
	}
	if _, err := h.RemoveRule("./AGENTS.local.md", mcpserver.ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	if g := loadSwitchFile(h.switchFile(rulesKind, mcpserver.ScopeGlobal)); len(g.Add) != 1 || g.Add[0] != filepath.Join(wsB, "plain.md") {
		t.Errorf("after remove: %+v", g)
	}
}
