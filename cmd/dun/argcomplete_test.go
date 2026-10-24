package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func manySkills(n int) []catalogSkill {
	out := make([]catalogSkill, 0, n+3)
	out = append(out,
		catalogSkill{Name: "code-review", Source: "user", Description: "review a diff"},
		catalogSkill{Name: "review", Source: "project", Description: "read the change"},
		catalogSkill{Name: "rails-everything", Source: "mcp:corp", Description: "a framework"},
	)
	for i := len(out); i < n; i++ {
		out = append(out, catalogSkill{Name: fmt.Sprintf("tool-%03d", i), Source: "user", Description: fmt.Sprintf("does thing %d", i)})
	}
	return out
}

func skillModel(t *testing.T, n int) (tuiModel, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	m = resized(m, 100, 40)
	m.starting = false
	m.skills = manySkills(n)
	return m, &buf
}

// Prefix beats substring beats a fuzzy subsequence beats the description.
func TestMatchRank_Order(t *testing.T) {
	cands := []argChoice{
		{label: "zzz", desc: "for a careful rev of things"}, // description
		{label: "rails-everything"},                         // r…e…v fuzzy
		{label: "code-review"},                              // substring
		{label: "review"},                                   // prefix
		{label: "unrelated"},
	}
	got := rankChoices("REV", cands)
	var names []string
	for _, c := range got {
		names = append(names, c.label)
	}
	if want := "review code-review rails-everything zzz"; strings.Join(names, " ") != want {
		t.Fatalf("order %v, want %s", names, want)
	}
	if h := got[1].hits; len(h) != 3 || h[0] != 5 {
		t.Errorf("substring hits %v", h)
	}
	if h := got[2].hits; len(h) != 3 || h[0] != 0 || h[1] != 6 || h[2] != 7 {
		t.Errorf("fuzzy hits %v", h)
	}
	if got[3].hits != nil {
		t.Errorf("a description match highlights nothing in the name: %v", got[3].hits)
	}
}

// "/" lists commands; "/skill " switches the palette to the skill names,
// with a count; free text after the name switches back.
func TestPalette_SwitchesToArguments(t *testing.T) {
	m, _ := skillModel(t, 200)
	m = typeStr(m, "/sk")
	if _, _, ok := m.paletteArgs(); ok {
		t.Fatal("a command name is still being typed")
	}
	m = typeStr(m, "ill rev")
	cmd, l, ok := m.paletteArgs()
	if !ok || cmd != "skill" {
		t.Fatalf("argument mode: ok=%v cmd=%q", ok, cmd)
	}
	if len(l.choices) != 3 || l.choices[0].value != "review" {
		t.Fatalf("choices %+v", l.choices)
	}
	view := stripANSI(m.palettePanel())
	if !strings.Contains(view, "3 of 200 skills") {
		t.Errorf("no count line:\n%s", view)
	}
	if !strings.Contains(view, "mcp:corp") || !strings.Contains(view, "read the change") {
		t.Errorf("source or description missing:\n%s", view)
	}
	m = typeStr(m, "iew fix it")
	if _, _, ok := m.paletteArgs(); ok {
		t.Error("free text after the name is not completed")
	}
}

// Tab completes the highlighted choice; enter runs it.
func TestPalette_TabAndEnterOnChoice(t *testing.T) {
	m, buf := skillModel(t, 200)
	m = typeStr(m, "/skill rev")
	m = key(m, kDown) // code-review
	m = key(m, kTab)
	if got := m.input.Value(); got != "/skill code-review " {
		t.Fatalf("tab: %q", got)
	}
	if _, _, ok := m.paletteArgs(); ok { // a space after the name: free text now
		t.Error("after tab the palette should stop offering names")
	}

	m.input.Reset()
	m = typeStr(m, "/skill rev")
	m = key(m, kDown)
	m = key(m, kDown) // rails-everything
	m = key(m, kEnter)
	if !strings.Contains(buf.String(), "Use the skill rails-everything") {
		t.Fatalf("enter did not run the highlighted skill: %s", buf.String())
	}
	if m.input.Value() != "" {
		t.Errorf("input left %q", m.input.Value())
	}
}

// 200 skills: the palette shows a window, and the selection scrolls in it.
func TestPalette_BoundedHeight(t *testing.T) {
	m, _ := skillModel(t, 200)
	m = typeStr(m, "/skill ")
	h := lipgloss.Height(m.palettePanel())
	if want := paletteWindow + 2; h != want { // rows + count + input
		t.Fatalf("palette height %d, want %d", h, want)
	}
	for i := 0; i < 50; i++ {
		m = key(m, kDown)
	}
	if m.paletteSel != 50 {
		t.Fatalf("sel %d", m.paletteSel)
	}
	view := stripANSI(m.palettePanel())
	if lipgloss.Height(view) != paletteWindow+2 || !strings.Contains(view, "▎ tool-050") {
		t.Errorf("the selection is not in the window:\n%s", view)
	}
	if !strings.Contains(view, "200 of 200 skills") {
		t.Errorf("count:\n%s", view)
	}
	// The command list follows the same cap.
	m.input.Reset()
	m.paletteSel = 0
	m = typeStr(m, "/")
	if n := len(m.paletteMatches()); n > paletteWindow {
		if h := lipgloss.Height(m.palettePanel()); h != paletteWindow+4 { // + the heading, the gap, the count
			t.Errorf("command palette height %d with %d commands", h, n)
		}
	}
}

func lastText(m tuiModel) string { return stripANSI(m.convo[len(m.convo)-1].view()) }

// Bare /skill: one by one for a few, a summary by source for many.
func TestSkillSlash_BareSummaryOrList(t *testing.T) {
	m, _ := skillModel(t, 5)
	skillSlash(&m, nil)
	if v := lastText(m); !strings.Contains(v, "code-review") || !strings.Contains(v, "tool-004") {
		t.Errorf("a few are listed:\n%s", v)
	}
	m, _ = skillModel(t, 200)
	skillSlash(&m, nil)
	v := lastText(m)
	if strings.Contains(v, "tool-004") || !strings.Contains(v, "200 skills") ||
		!strings.Contains(v, "mcp:corp") || !strings.Contains(v, "/skill <part of a name>") {
		t.Errorf("many are summarised:\n%s", v)
	}
	if !strings.Contains(v, "198") { // user: code-review + 197 tools
		t.Errorf("per-source count:\n%s", v)
	}
}

// /skill <part>: many matches are listed, one runs, none is an error.
func TestSkillSlash_Query(t *testing.T) {
	m, buf := skillModel(t, 200)
	skillSlash(&m, []string{"tool-1"})
	if v := lastText(m); !strings.Contains(v, "skills match") || !strings.Contains(v, "more") || buf.Len() != 0 {
		t.Errorf("many matches:\n%s", v)
	}
	skillSlash(&m, []string{"rails", "do", "it"})
	if !strings.Contains(buf.String(), "Use the skill rails-everything") || !strings.Contains(buf.String(), "do it") {
		t.Errorf("a unique match runs: %s", buf.String())
	}
	buf.Reset()
	skillSlash(&m, []string{"qqqq"})
	if v := lastText(m); !strings.Contains(v, "no skill matches qqqq") || buf.Len() != 0 {
		t.Errorf("no match:\n%s", v)
	}
}

// /mcp completes its subcommands from the engine's choices, then server ids.
func TestPalette_MCPCompletion(t *testing.T) {
	m, _ := skillModel(t, 0)
	m.engineCmds = engineCmdsFromEvent(evMsg{"items": []any{map[string]any{"name": "mcp",
		"choices": []any{map[string]any{"value": "tools", "description": "costs"}, map[string]any{"value": "add"}}}}})
	m.noteServers(evMsg{"servers": []any{map[string]any{"id": "github"}, map[string]any{"id": "shell"}}})
	m = typeStr(m, "/mcp to")
	if _, l, ok := m.paletteArgs(); !ok || len(l.choices) != 1 || l.choices[0].value != "tools" {
		t.Fatalf("subcommand: %+v", l)
	}
	m = key(m, kTab)
	m = typeStr(m, "gi")
	_, l, ok := m.paletteArgs()
	if !ok || len(l.choices) != 1 || l.choices[0].value != "tools github" {
		t.Fatalf("server id: %+v", l)
	}
	m = key(m, kTab)
	if got := m.input.Value(); got != "/mcp tools github " {
		t.Errorf("tab: %q", got)
	}
}
