package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// skillVtui is /skill open on a skill with a link, a broken link and an
// @include (that includes itself back: the cycle guard), at w×h.
func skillVtui(t *testing.T, w, h int) (*vtui, *recWC) {
	t.Helper()
	v := newVtui(w, h)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	rec := &recWC{}
	v.m.proc = &dunProc{stdin: rec}
	items := []any{
		map[string]any{"name": "deploy", "description": "ship it", "source": "user", "tokens": float64(20), "on": true},
		map[string]any{"name": "pdf-forms", "description": "fill PDF forms", "source": "user", "tokens": float64(40), "on": true},
	}
	v.m.openSwitches("skills")
	v.event(map[string]any{"type": "switches", "kind": "skills", "mode": "listed", "items": items, "order": []any{"listed", "surfaced"}})
	if v.m.switchesPaging() == nil {
		t.Fatal("the list did not open")
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyDown})
	rec.Reset()
	v.m = key(v.m, kEnter)
	if !strings.Contains(rec.String(), `"skillfile"`) || !strings.Contains(rec.String(), `pdf-forms`) {
		t.Fatalf("enter did not ask for the skill: %s", rec.String())
	}
	v.event(skillFileEv("SKILL.md", "Fill a PDF form.\n\n## Steps\n\n1. Read references/forms.md for the field names.\n"+
		"2. Run `scripts/fill.py` on the form.\n3. See [the old guide](old.md) too.\n\n@shared/rules.md\n\nDone.\n"))
	return v, rec
}

func skillFileEv(file, text string) map[string]any {
	ev := map[string]any{"type": "skill_file", "skill": "pdf-forms", "file": file, "text": text,
		"files":    []any{"SKILL.md", "references/forms.md", "scripts/fill.py", "shared/rules.md"},
		"includes": map[string]any{"shared/rules.md": "Shared rule: never sign for the user. @SKILL.md and @shared/rules.md"}}
	if file == "SKILL.md" {
		ev["name"], ev["description"] = "pdf-forms", "fill PDF forms"
	}
	return ev
}

func TestSkillPane_Reader(t *testing.T) {
	v, rec := skillVtui(t, 110, 40)
	got := v.view()
	for _, want := range []string{"pdf-forms", "fill PDF forms", "Steps", "references/forms.md", "Shared rule: never sign",
		"included: shared/rules.md", "end of shared/rules.md", "included above: SKILL.md", "included above: shared/rules.md",
		"the old guide ✗", "3 links", "deploy"} {
		if !strings.Contains(got, want) {
			t.Errorf("the pane lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "@shared/rules.md") || strings.Contains(got, "description:") {
		t.Errorf("an include or the front matter shows raw:\n%s", got)
	}
	p := v.m.scr.skillPane()
	if p == nil || len(p.links) != 3 || p.links[0].target != "references/forms.md" || p.links[1].target != "scripts/fill.py" || !p.links[2].broken {
		t.Fatalf("links: %+v", p)
	}
	// n and p (and tab) move between the links.
	v.m = key(v.m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyTab})
	if p.at.sel != 1 || !strings.Contains(v.view(), "link 2/3") {
		t.Errorf("n, tab: sel %d\n%s", p.at.sel, v.view())
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	if p.at.sel != 0 {
		t.Errorf("p: sel %d", p.at.sel)
	}
	// The selected link is in reverse video; the others are underlined.
	if raw := v.m.View().Content; !strings.Contains(stripANSI(raw), "references/forms.md") ||
		!strings.Contains(raw, sgrSel) || !strings.Contains(raw, sgrLink) || !strings.Contains(raw, sgrBroken) {
		t.Errorf("the links' styles are missing: %q", raw)
	}
	// enter follows it; backspace comes back to the same place.
	rec.Reset()
	v.m = key(v.m, kEnter)
	if !strings.Contains(rec.String(), `references/forms.md`) {
		t.Fatalf("enter did not ask for the link: %s", rec.String())
	}
	v.event(skillFileEv("references/forms.md", "# Fields\n\nname, date, signature. Back to SKILL.md.\n"))
	if got := v.view(); !strings.Contains(got, "pdf-forms › references/forms.md") || !strings.Contains(got, "signature") {
		t.Errorf("the link's file:\n%s", got)
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := v.view(); p.at.file != "SKILL.md" || p.at.sel != 0 || !strings.Contains(got, "Steps") {
		t.Errorf("back: %+v\n%s", p.at, got)
	}
	// enter on a broken link says so and stays.
	v.m = key(v.m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	rec.Reset()
	v.m = key(v.m, kEnter)
	if p.at.sel != 2 || rec.String() != "" || !strings.Contains(lastRow(v.m), "no such file in the skill: old.md") {
		t.Errorf("broken: sel %d sent %q status %q", p.at.sel, rec.String(), lastRow(v.m))
	}
	// esc on the skill itself closes the pane; the list has the keys again.
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if v.m.scr == nil || v.m.scr.skillPane() != nil || v.m.switchesPaging() == nil || strings.Contains(v.view(), "Steps") {
		t.Errorf("esc did not close the pane:\n%s", v.view())
	}
}

func TestSkillPane_Search(t *testing.T) {
	v, _ := skillVtui(t, 110, 40)
	p := v.m.scr.skillPane()
	v.m = key(v.m, tea.KeyPressMsg{Code: '/', Text: "/"})
	v.m = typeStr(v.m, "never sign")
	if len(p.matches) != 1 || !strings.Contains(v.view(), "/never sign█") || !strings.Contains(v.view(), "1/1") {
		t.Errorf("text search: %+v\n%s", p.matches, v.view())
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEscape})
	// A link by its target: [the old guide](old.md) shows no "old.md" but its text.
	v.m = key(v.m, tea.KeyPressMsg{Code: '/', Text: "/"})
	v.m = typeStr(v.m, "fill.py")
	v.m = key(v.m, kEnter)
	if p.searchOn || p.at.sel != 1 {
		t.Errorf("search for a link: sel %d on %v", p.at.sel, p.searchOn)
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: '/', Text: "/"})
	for range len("fill.py") {
		v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	v.m = typeStr(v.m, "old.md")
	if p.at.sel != 2 {
		t.Errorf("a link's target: sel %d, matches %+v", p.at.sel, p.matches)
	}
}

// A phone: the pane takes the screen; the list is back when it closes.
func TestSkillPane_Narrow(t *testing.T) {
	v, _ := skillVtui(t, 48, 30)
	got := v.view()
	if strings.Contains(got, "deploy") || !strings.Contains(got, "pdf-forms") || !strings.Contains(got, "n/p link") {
		t.Errorf("narrow:\n%s", got)
	}
	for _, l := range strings.Split(got, "\n") {
		if w := len([]rune(l)); w > 48 {
			t.Errorf("a row of %d cells: %q", w, l)
		}
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := v.view(); !strings.Contains(got, "deploy") {
		t.Errorf("the list is not back:\n%s", got)
	}
}

// TestSkillPane_Capture prints the pane (go test -run Capture -v).
func TestSkillPane_Capture(t *testing.T) {
	for _, w := range []int{110, 48} {
		v, _ := skillVtui(t, w, 34)
		t.Logf("at %d columns:\n%s", w, v.view())
	}
}
