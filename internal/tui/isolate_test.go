package tui

import (
	"slices"
	"strings"
	"testing"
)

// /isolate is listed, with its choices described; /docker and /worktree new
// are not.
func TestIsolate_Palette(t *testing.T) {
	cmds := engineCmdsNow(t)
	var iso, wt *engineCmd
	for i := range cmds {
		switch cmds[i].name {
		case "isolate":
			iso = &cmds[i]
		case "worktree":
			wt = &cmds[i]
		case "docker":
			t.Error("/docker is listed")
		}
	}
	if iso == nil || wt == nil {
		t.Fatalf("isolate %v, worktree %v", iso, wt)
	}
	var vals []string
	for _, c := range iso.choices {
		vals = append(vals, c.value)
		if c.desc == "" || strings.Contains(c.desc, "\n") {
			t.Errorf("%s: %q", c.value, c.desc)
		}
	}
	if !slices.Equal(vals, []string{"none", "worktree", "docker", "off"}) {
		t.Errorf("choices %v", vals)
	}
	if hasChoice(*wt, "new") {
		t.Error("/worktree new is in the palette")
	}

	m, _ := skillModel(t, 0)
	m.engineCmds = cmds
	m = typeStr(m, "/isolate d")
	if _, l, ok := m.paletteArgs(); !ok || len(l.choices) == 0 || l.choices[0].value != "docker" {
		t.Fatalf("/isolate d: %+v", l)
	}
	m.input.Reset()
	m = typeStr(m, "/dock")
	for _, c := range m.paletteMatches() {
		if c.Name == "docker" {
			t.Error("/dock lists /docker")
		}
	}
	m.input.Reset()
	m = typeStr(m, "/isol")
	if ms := m.paletteMatches(); len(ms) == 0 || ms[0].Name != "isolate" {
		t.Errorf("/isol: %v", ms)
	}
	m.input.Reset()
	m.showHelp(false)
	if help := stripANSI(m.convo[len(m.convo)-1].collapsed); !strings.Contains(help, "/isolate [none|worktree|docker|off]") {
		t.Errorf("/help lacks /isolate:\n%s", help)
	}
}

// The TUI says nothing for an empty branch (back in place) and drops the ⎇.
func TestIsolate_TUIWorkspaceEvent(t *testing.T) {
	m, _ := skillModel(t, 0)
	m = m.handleEvent(evMsg{"type": "workspace", "path": "/wt", "branch": "dun/1-x"})
	n := len(m.convo)
	if m.branch != "dun/1-x" || !strings.Contains(stripANSI(m.convo[n-1].collapsed), "worktree branch: dun/1-x") {
		t.Fatalf("branch %q", m.branch)
	}
	m = m.handleEvent(evMsg{"type": "workspace", "path": "/ws", "branch": ""})
	if m.branch != "" || len(m.convo) != n {
		t.Errorf("back in place: branch %q, %d lines (was %d)", m.branch, len(m.convo), n)
	}
}
