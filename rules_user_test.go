package dun

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// userHome points userHomeDir at a new temp dir for the test and returns it.
func userHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	old := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = old })
	return home
}

// before fails the test unless every string is in b, each after the one
// before it.
func before(t *testing.T, b string, in ...string) {
	t.Helper()
	at := -1
	for _, s := range in {
		i := strings.Index(b, s)
		if i < 0 {
			t.Fatalf("%q not in the block:\n%s", s, b)
		}
		if i <= at {
			t.Fatalf("%q is not after %q:\n%s", s, in[0], b)
		}
		at = i
	}
}

// The user's files alone: ~/AGENTS.md, ~/CLAUDE.md, ~/.claude/CLAUDE.md, in
// that order, each under a header with its path; no project section.
func TestUserRules_UserOnly(t *testing.T) {
	home := userHome(t)
	writeRule(t, filepath.Join(home, "agents.md"), "u-agents")
	writeRule(t, filepath.Join(home, "CLAUDE.md"), "u-claude")
	writeRule(t, filepath.Join(home, ".claude", "CLAUDE.md"), "u-dotclaude")
	b := rootRules(t.TempDir())
	before(t, b, "## User rules (~/agents.md, ~/CLAUDE.md, ~/.claude/CLAUDE.md)",
		"### ~/agents.md", "u-agents", "### ~/CLAUDE.md", "u-claude", "### ~/.claude/CLAUDE.md", "u-dotclaude")
	if strings.Contains(b, "## Project rules") {
		t.Errorf("a project section with no project file:\n%s", b)
	}
}

func TestUserRules_ProjectOnly(t *testing.T) {
	userHome(t)
	ws := t.TempDir()
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "p-agents")
	b := rootRules(ws)
	if strings.Contains(b, "User rules") || !strings.Contains(b, "## Project rules (AGENTS.md)") {
		t.Errorf("project only:\n%s", b)
	}
}

// Both: the user's section first, so the project's, later, refines it.
func TestUserRules_BothUserFirst(t *testing.T) {
	home := userHome(t)
	ws := t.TempDir()
	writeRule(t, filepath.Join(home, "CLAUDE.md"), "u-claude")
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "p-agents")
	writeRule(t, filepath.Join(ws, "CLAUDE.md"), "p-claude")
	b := rootRules(ws)
	before(t, b, "## User rules (~/CLAUDE.md)", "follow the project's", "### ~/CLAUDE.md", "u-claude",
		"## Project rules (AGENTS.md, CLAUDE.md)", "### AGENTS.md", "p-agents", "### CLAUDE.md", "p-claude")
	// The same bytes in a user file and a project file: once, under the
	// user's header, named as both.
	writeRule(t, filepath.Join(ws, "CLAUDE.md"), "u-claude")
	b = rootRules(ws)
	if strings.Count(b, "u-claude") != 1 || !strings.Contains(b, "### ~/CLAUDE.md (also CLAUDE.md)") {
		t.Errorf("same bytes:\n%s", b)
	}
}

// The workspace is the home directory: its AGENTS.md and CLAUDE.md are the
// project's and are read once; ~/.claude/CLAUDE.md is still the user's.
func TestUserRules_WorkspaceIsHome(t *testing.T) {
	home := userHome(t)
	writeRule(t, filepath.Join(home, "AGENTS.md"), "h-agents")
	writeRule(t, filepath.Join(home, "CLAUDE.md"), "h-claude")
	writeRule(t, filepath.Join(home, ".claude", "CLAUDE.md"), "h-dotclaude")
	set := loadRules(home, true)
	b := set.render(RulesFull, "")
	for _, s := range []string{"h-agents", "h-claude", "h-dotclaude"} {
		if strings.Count(b, s) != 1 {
			t.Errorf("%q %d times:\n%s", s, strings.Count(b, s), b)
		}
	}
	before(t, b, "## User rules (~/.claude/CLAUDE.md)", "h-dotclaude", "## Project rules (AGENTS.md, CLAUDE.md)", "h-agents", "h-claude")
	if len(set.Roots) != 3 {
		t.Errorf("roots: %+v", set.Roots)
	}
	search := strings.Join(rulesSearch(home, true), "\n")
	if strings.Contains(search, "~/AGENTS.md") || !strings.Contains(search, "the workspace is the home directory") {
		t.Errorf("search:\n%s", search)
	}
}

// A user file's @imports resolve relative to that file, and sit in the
// user's section, before the project's.
func TestUserRules_Imports(t *testing.T) {
	home := userHome(t)
	ws := t.TempDir()
	writeRule(t, filepath.Join(home, "CLAUDE.md"), "u-root @notes/style.md")
	writeRule(t, filepath.Join(home, "notes", "style.md"), "u-style")
	writeRule(t, filepath.Join(home, ".claude", "CLAUDE.md"), "u-dot @extra.md")
	writeRule(t, filepath.Join(home, ".claude", "extra.md"), "u-extra")
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "p-root @p.md")
	writeRule(t, filepath.Join(ws, "p.md"), "p-import")
	set := loadRules(ws, true)
	if got := strings.Join(importNames(set), " "); got != "~/notes/style.md ~/.claude/extra.md p.md" {
		t.Errorf("imports: %s", got)
	}
	for _, d := range set.Imports {
		if d.User != strings.HasPrefix(d.Name, "~/") {
			t.Errorf("%s: user %v", d.Name, d.User)
		}
	}
	before(t, set.render(RulesFull, ""), "u-root", "u-dot", "### ~/notes/style.md (imported by ~/CLAUDE.md)", "u-style",
		"### ~/.claude/extra.md (imported by ~/.claude/CLAUDE.md)", "u-extra", "## Project rules", "p-root", "p-import")
	// In summary mode the imports, the user's too, are the one section after.
	before(t, set.render(RulesSummary, "condensed"), "## User rules", "## Project rules", "- ~/notes/style.md", "- p.md", "words.\n\ncondensed")
}

// The user files have their own budget: past it they are cut, and the
// project's files still get theirs whole.
func TestUserRules_Caps(t *testing.T) {
	home := userHome(t)
	ws := t.TempDir()
	big := strings.Repeat("u", 40<<10)
	writeRule(t, filepath.Join(home, "AGENTS.md"), big+" @more.md")
	writeRule(t, filepath.Join(home, "CLAUDE.md"), strings.Repeat("v", 40<<10))
	writeRule(t, filepath.Join(home, "more.md"), "more")
	writeRule(t, filepath.Join(ws, "AGENTS.md"), strings.Repeat("p", 60<<10))
	set := loadRules(ws, true)
	user := 0
	for _, d := range append(append([]rulesDoc{}, set.Roots...), set.Imports...) {
		if d.User {
			user += len(d.Text)
		}
	}
	if user > rulesUserMax {
		t.Errorf("user files %d bytes, cap %d", user, rulesUserMax)
	}
	if n := set.Roots[1].Note; n != "cut: the user rules reached their 64 KiB total" {
		t.Errorf("second user file: %q", n)
	}
	if got := strings.Join(importNames(set), " "); got != "~/more.md [not read: the user rules reached their 64 KiB total]" {
		t.Errorf("import past the cap: %s", got)
	}
	if p := set.Roots[2]; p.User || len(p.Text) != 60<<10 || p.Note != "" {
		t.Errorf("the project file was crowded: %d bytes, note %q", len(p.Text), p.Note)
	}
}

// The off switch: no user files read, and /rules says so.
func TestUserRules_Off(t *testing.T) {
	home := userHome(t)
	ws := t.TempDir()
	writeRule(t, filepath.Join(home, "CLAUDE.md"), "u-claude")
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "p-agents")
	if b := loadRules(ws, false).render(RulesFull, ""); strings.Contains(b, "u-claude") || !strings.Contains(b, "p-agents") {
		t.Errorf("off:\n%s", b)
	}
	h := rulesHarness(t, ws)
	if st := h.RulesStatus(); !strings.Contains(st, "~/CLAUDE.md") || !strings.Contains(st, "~/AGENTS.md not there") {
		t.Errorf("on, status:\n%s", st)
	}
	h.cfg.NoUserRules = true
	if b, _, _ := h.rulesBlock(false); strings.Contains(b, "u-claude") {
		t.Errorf("off, block:\n%s", b)
	}
	if st := h.RulesStatus(); !strings.Contains(st, "user files: off (DUN_USER_RULES=0)") || strings.Contains(st, "~/CLAUDE.md") {
		t.Errorf("off, status:\n%s", st)
	}
	if s := strings.Join(rulesSearch(ws, false), "\n"); !strings.Contains(s, "user rules: off") {
		t.Errorf("off, search:\n%s", s)
	}
}

// A child gets its parent's rules, the user's with them, and the off switch
// with its Config.
func TestUserRules_Children(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	home := userHome(t)
	ws := t.TempDir()
	writeRule(t, filepath.Join(home, "CLAUDE.md"), "u-claude")
	writeRule(t, filepath.Join(ws, "AGENTS.md"), "p-agents")
	for _, off := range []bool{false, true} {
		parent, err := Start(context.Background(), Config{Workspace: ws, Servers: []Server{}, NoUserRules: off})
		if err != nil {
			t.Fatal(err)
		}
		ccfg := childConfig(parent, 1, childRun{}, isolation{})
		ccfg.SessionFile = ""
		child, err := Start(context.Background(), ccfg)
		if err != nil {
			parent.Close()
			t.Fatal(err)
		}
		if !child.isChild() {
			t.Fatal("not a child")
		}
		for name, sys := range map[string]string{"parent": parent.Session.System, "child": child.Session.System} {
			if strings.Contains(sys, "u-claude") == off || !strings.Contains(sys, "p-agents") {
				t.Errorf("off=%v, %s's system prompt: user rules %v", off, name, strings.Contains(sys, "u-claude"))
			}
		}
		child.Close()
		parent.Close()
	}
}
