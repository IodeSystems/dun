package tui

import (
	"bytes"
	"github.com/charmbracelet/x/ansi"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/dunconfig"
)

// rolesModel is a TUI with two saved providers and a config of facets,
// the roles window not yet open.
func rolesModel(t *testing.T, facets map[string]dunconfig.Facet) (tuiModel, *bytes.Buffer) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("DUN_MODEL", "")
	cfg := dunconfig.DunConfig{Provider: "work",
		Providers: []dunconfig.ProviderConfig{{Name: "work", URL: "http://127.0.0.1:1/w", Key: "sk-secret"}, {Name: "fast", URL: "http://127.0.0.1:1/f"}},
		Facets:    facets}
	if err := dunconfig.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	m = resized(m, 100, 40)
	m.starting = false
	m.provider, m.url, m.model = "work", "http://127.0.0.1:1/w", "chat"
	return m, &buf
}

// Bare /model opens the roles window: all six roles, each with its model
// and source, the purpose of the highlighted one, and the two notices.
func TestRoles_BareModelOpensWindow(t *testing.T) {
	m, _ := rolesModel(t, map[string]dunconfig.Facet{dunconfig.FacetMain: {Provider: "work", Model: "chat"}})
	modelSlash(&m, nil)
	if !m.rolesPicking || !m.modelPicking {
		t.Fatal("bare /model did not open the roles window")
	}
	p := stripANSI(m.rolesPanel())
	for _, role := range dunconfig.FacetNames {
		if !strings.Contains(p, role+" ") {
			t.Errorf("no row for %s:\n%s", role, p)
		}
	}
	for _, want := range []string{
		"main      chat on work · config",
		"embed     local-nomic-embed-text on work · default",
		"segment   off · default",
		"answers you in the conversation",
		"apply after dund restarts",
		"/rag setup",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("panel missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "sk-secret") {
		t.Error("a key in the roles window")
	}
	if h := stripANSI(m.View().Content); !strings.Contains(h, "p probe") || !strings.Contains(h, "x reset") {
		t.Errorf("hints missing from the screen:\n%s", h)
	}
	m = pick(m, "down", "down")
	if p := stripANSI(m.rolesPanel()); !strings.Contains(p, "vision: reads images and does OCR") {
		t.Errorf("purpose of the highlighted role:\n%s", p)
	}
	m = pick(m, "esc")
	if m.modelPicking || m.rolesPicking {
		t.Error("esc did not close the window")
	}
}

// enter on a non-main role opens the picker for it; enter there saves
// facets.<role> and returns to the window; esc returns without saving.
func TestRoles_PickForRole(t *testing.T) {
	m, _ := rolesModel(t, nil)
	modelSlash(&m, nil)
	m = pick(m, "down", "down", "enter") // vision
	if m.rolesPicking || !m.modelPicking || m.pickFacet != dunconfig.FacetVision {
		t.Fatalf("picker for vision: roles %v picking %v facet %q", m.rolesPicking, m.modelPicking, m.pickFacet)
	}
	m.modelRows = []modelRow{{"fast", "qwen-vl"}, {"work", "chat"}}
	if p := stripANSI(m.modelPanel()); !strings.Contains(p, "pick a model for vision") {
		t.Errorf("panel:\n%s", p)
	}
	if h := stripANSI(m.View().Content); strings.Contains(h, "alt+enter") {
		t.Errorf("a session-only pick offered for a role:\n%s", h)
	}
	m = pick(m, "esc")
	if !m.rolesPicking || m.rolesSel != 2 {
		t.Fatalf("esc must return to the roles window on vision: %v %d", m.rolesPicking, m.rolesSel)
	}
	if f := dunconfig.LoadConfig().Facets; len(f) != 0 {
		t.Fatalf("esc saved: %+v", f)
	}

	m = pick(m, "enter")
	m.modelRows = []modelRow{{"fast", "qwen-vl"}, {"work", "chat"}}
	m = pick(m, "enter")
	if f := dunconfig.LoadConfig().Facets["vision"]; f != (dunconfig.Facet{Provider: "fast", Model: "qwen-vl"}) {
		t.Fatalf("vision saved: %+v", f)
	}
	if !m.rolesPicking || m.rolesSel != 2 {
		t.Error("saving must return to the roles window")
	}
	if p := stripANSI(m.rolesPanel()); !strings.Contains(p, "vision    qwen-vl on fast · config") {
		t.Errorf("the row shows the new model:\n%s", p)
	}
	if m.model != "chat" {
		t.Errorf("a role pick switched main: %s", m.model)
	}
}

// The picker starts on, and marks, the role's current model.
func TestRoles_PickerMarksTheRolesModel(t *testing.T) {
	m, _ := rolesModel(t, map[string]dunconfig.Facet{dunconfig.FacetVision: {Provider: "fast", Model: "qwen-vl"}})
	modelSlash(&m, nil)
	m = pick(m, "down", "down", "enter")
	m.modelRows = []modelRow{{"work", "chat"}, {"fast", "other"}, {"fast", "qwen-vl"}}
	if got := m.pickCurrent(); got != (modelRow{"fast", "qwen-vl"}) {
		t.Fatalf("current %+v", got)
	}
	p := stripANSI(m.modelPanel())
	line := ""
	for _, l := range strings.Split(p, "\n") {
		if strings.Contains(l, "qwen-vl") {
			line = l
		}
	}
	if !strings.Contains(line, "in use") {
		t.Errorf("the role's model is not marked:\n%s", p)
	}
}

// The picker of main is as it was: enter switches the session and saves.
func TestRoles_MainUnchanged(t *testing.T) {
	m, _ := rolesModel(t, nil)
	modelSlash(&m, nil)
	m = pick(m, "enter") // main
	if m.pickFacet != dunconfig.FacetMain || m.rolePick() {
		t.Fatalf("facet %q", m.pickFacet)
	}
	m.modelRows = []modelRow{{"fast", "kid"}, {"work", "chat"}}
	m.modelScope, m.models.sel = "", 0
	m = pick(m, "enter")
	c := dunconfig.LoadConfig()
	if m.model != "kid" || c.Facets["main"] != (dunconfig.Facet{Provider: "fast", Model: "kid"}) {
		t.Fatalf("main: running %s, saved %+v", m.model, c.Facets)
	}
	if m.modelPicking || m.rolesPicking {
		t.Error("the picker stays open after switching main")
	}
}

// alt+enter is refused for a role, with a message, and nothing is saved.
func TestRoles_AltEnterRefusedForRole(t *testing.T) {
	m, _ := rolesModel(t, nil)
	modelSlash(&m, nil)
	m = pick(m, "down", "enter") // child
	m.modelRows = []modelRow{{"fast", "kid"}}
	m.modelScope, m.models.sel = "", 0
	nm, _ := m.updateModelPicking(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
	m = nm.(tuiModel)
	if !m.modelPicking || m.rolesPicking {
		t.Error("the picker must stay open")
	}
	if !strings.Contains(stripANSI(convoText(m)), "only main has a session-only pick") {
		t.Errorf("no message:\n%s", stripANSI(convoText(m)))
	}
	if f := dunconfig.LoadConfig().Facets; len(f) != 0 {
		t.Fatalf("saved: %+v", f)
	}
}

// x removes facets.<role>; the row then shows what it falls back to.
func TestRoles_ResetFallsBack(t *testing.T) {
	m, _ := rolesModel(t, map[string]dunconfig.Facet{dunconfig.FacetEmbed: {Provider: "fast", Model: "e5"}})
	modelSlash(&m, nil)
	m = pick(m, "down", "down", "down") // embed
	if p := stripANSI(m.rolesPanel()); !strings.Contains(p, "embed     e5 on fast · config") {
		t.Fatalf("before:\n%s", p)
	}
	m = pick(m, "x")
	if _, ok := dunconfig.LoadConfig().Facets[dunconfig.FacetEmbed]; ok {
		t.Fatal("facets.embed still saved")
	}
	if p := stripANSI(m.rolesPanel()); !strings.Contains(p, "embed     local-nomic-embed-text on work · default") {
		t.Errorf("after:\n%s", p)
	}
	if !m.rolesPicking {
		t.Error("reset closed the window")
	}
	if !strings.Contains(stripANSI(convoText(m)), "embed reset to its default") {
		t.Errorf("no notice:\n%s", stripANSI(convoText(m)))
	}
	m = pick(m, "x") // again: nothing to remove
	if !strings.Contains(stripANSI(convoText(m)), "embed already uses its default") {
		t.Errorf("no word on a second reset:\n%s", stripANSI(convoText(m)))
	}
}

// p sends the role's probe to the engine and closes the window.
func TestRoles_ProbeDispatches(t *testing.T) {
	m, buf := rolesModel(t, nil)
	modelSlash(&m, nil)
	m = pick(m, "down", "down", "p")
	if m.modelPicking || m.rolesPicking {
		t.Error("the window stays open")
	}
	if got := buf.String(); !strings.Contains(got, "probe vision") {
		t.Errorf("probe not sent: %q", got)
	}
}

// On a short, narrow screen the window and its hints still fit.
func TestRoles_SmallScreen(t *testing.T) {
	m, _ := rolesModel(t, nil)
	m = resized(m, 60, 20)
	modelSlash(&m, nil)
	v := stripANSI(m.View().Content)
	if !strings.Contains(v, "models by role") || !strings.Contains(v, "esc close") {
		t.Errorf("small screen:\n%s", v)
	}
}

// labelsOf are the labels of the argument choices at the input.
func labelsOf(t *testing.T, m tuiModel) []string {
	t.Helper()
	_, l, ok := m.paletteArgs()
	if !ok {
		return nil
	}
	var out []string
	for _, c := range l.choices {
		out = append(out, c.label)
	}
	return out
}

// /model completes its subcommands, then the roles and the provider/model
// after use, the roles after probe; no key is typed into the input.
func TestModelCompletion(t *testing.T) {
	m, _ := rolesModel(t, nil)
	m.modelRows = []modelRow{{"fast", "qwen-vl"}, {"work", "chat"}}

	m = typeStr(m, "/model ")
	if got := strings.Join(labelsOf(t, m), " "); got != "use probe caps" {
		t.Errorf("/model : %q", got)
	}
	m = typeStr(m, "use ")
	got := labelsOf(t, m)
	for _, want := range append([]string{"fast/", "work/", "fast/qwen-vl"}, dunconfig.FacetNames...) {
		if !slices.Contains(got, want) {
			t.Errorf("/model use : no %q in %v", want, got)
		}
	}
	m = typeStr(m, "fast/")
	if got := labelsOf(t, m); len(got) < 2 || got[0] != "fast/" || !slices.Contains(got, "fast/qwen-vl") || slices.Contains(got, "work/chat") {
		t.Errorf("/model use fast/ : %v", got)
	}
	// tab on a provider continues it with no space
	m = key(m, kTab)
	if v := m.input.Value(); v != "/model use fast/" {
		t.Errorf("tab on a provider: %q", v)
	}

	m, _ = rolesModel(t, nil)
	m.modelRows = []modelRow{{"fast", "qwen-vl"}}
	m = typeStr(m, "/model use vision ")
	if got := strings.Join(labelsOf(t, m), " "); got != "work/ fast/ fast/qwen-vl" {
		t.Errorf("/model use vision : %q", got)
	}
	if p := stripANSI(m.palettePanel()); strings.Contains(p, "tea.Key") || strings.Contains(p, "{") {
		t.Errorf("a key string leaks:\n%s", p)
	}

	m, _ = rolesModel(t, nil)
	m = typeStr(m, "/model probe ")
	if got := strings.Join(labelsOf(t, m), " "); got != strings.Join(dunconfig.FacetNames, " ") {
		t.Errorf("/model probe : %q", got)
	}
	m = typeStr(m, "vis")
	if got := labelsOf(t, m); len(got) == 0 || got[0] != "vision" {
		t.Errorf("/model probe vis: %v", got)
	}
	m, _ = rolesModel(t, nil)
	m = typeStr(m, "/model probe vision ")
	if got := labelsOf(t, m); got != nil {
		t.Errorf("nothing follows a role: %v", got)
	}
}

// Rows left by a picker are offered only for providers still saved: one
// logged out since is not.
func TestModelCompletion_SkipsLoggedOutProviders(t *testing.T) {
	m, _ := rolesModel(t, nil)
	m.modelRows = []modelRow{{"fast", "qwen-vl"}, {"gone", "ghost-1"}}
	m = typeStr(m, "/model use ")
	got := labelsOf(t, m)
	if !slices.Contains(got, "fast/qwen-vl") {
		t.Errorf("saved provider's model missing: %v", got)
	}
	if slices.Contains(got, "gone/ghost-1") {
		t.Errorf("logged-out provider offered: %v", got)
	}
}

// At small heights the window fits in half the rows, the highlighted role
// stays in view, and notes go before roles do. The hint at 48 columns keeps
// enter and esc.
func TestRoles_FitsSmallHeights(t *testing.T) {
	for _, h := range []int{10, 12, 14, 24} {
		for sel := range dunconfig.FacetNames {
			m, _ := rolesModel(t, nil)
			m = resized(m, 48, h)
			m.openRoles(sel)
			p := stripANSI(m.rolesPanel())
			if n := strings.Count(p, "\n") + 1; n > max(5, h/2) {
				t.Errorf("h=%d sel=%d: %d rows > %d:\n%s", h, sel, n, max(5, h/2), p)
			}
			want := "\u258e " + dunconfig.FacetNames[sel]
			if !strings.Contains(p, want) {
				t.Errorf("h=%d sel=%d: highlighted row not shown:\n%s", h, sel, p)
			}
		}
	}
	for _, w := range []int{48, 40} {
		if h := rolesHint(w); !strings.Contains(h, "enter") || !strings.Contains(h, "esc") || ansi.StringWidth(h) > w {
			t.Errorf("hint at %d: %q", w, h)
		}
	}
	if h := rolesHint(48); strings.Contains(h, "move") {
		t.Errorf("move should go first at 48: %q", h)
	}
	if h := rolesHint(100); !strings.Contains(h, "move") || !strings.Contains(h, "probe") {
		t.Errorf("wide hint: %q", h)
	}
}
