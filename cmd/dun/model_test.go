package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// A config from before providers is one provider; adding one keeps it.
func TestProviders_LegacyConfigIsOneProvider(t *testing.T) {
	c := dunConfig{URL: "https://llm.example.com", Key: "sk-1", Model: "m"}
	if got := c.active(); got.Name != "llm.example.com" || got.Key != "sk-1" {
		t.Fatalf("legacy active = %+v", got)
	}
	c.putProvider(providerConfig{Name: "openrouter", URL: "https://openrouter.ai/api/v1", Key: "sk-or"})
	if len(c.Providers) != 2 || c.active().Name != "llm.example.com" {
		t.Fatalf("add must keep the legacy provider active: %+v", c.Providers)
	}
	if p, ok := c.providerAt("https://openrouter.ai/api/v1/"); !ok || p.Key != "sk-or" {
		t.Errorf("providerAt ignores a trailing slash; got %+v %v", p, ok)
	}
	c.use(c.Providers[1], "anthropic/x")
	if c.active().Name != "openrouter" || c.URL != "https://openrouter.ai/api/v1" || c.Key != "sk-or" || c.Model != "anthropic/x" {
		t.Errorf("use must set the default and the top-level fields: %+v", c)
	}
	if !c.removeProvider("llm.example.com") || len(c.Providers) != 1 {
		t.Errorf("remove: %+v", c.Providers)
	}
}

func pickerWith(rows ...modelRow) tuiModel {
	m := newTUIModel(nil, ".")
	m.openModelPicker("")
	m.modelRows = rows
	return m
}

func pick(m tuiModel, keys ...string) tuiModel {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "down", "up", "tab", "enter", "esc":
			msg = map[string]tea.KeyMsg{"down": {Type: tea.KeyDown}, "up": {Type: tea.KeyUp},
				"tab": {Type: tea.KeyTab}, "enter": {Type: tea.KeyEnter}, "esc": {Type: tea.KeyEsc}}[k]
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		nm, _ := m.updateModelPicking(msg)
		m = nm.(tuiModel)
	}
	return m
}

// Every typed word must appear, in any order, in "provider model".
func TestModelPicker_SearchMatchesEveryWord(t *testing.T) {
	m := pickerWith(
		modelRow{"local", "qwen3-coder-30b"},
		modelRow{"openrouter", "qwen/qwen3-coder"},
		modelRow{"openrouter", "anthropic/claude-sonnet-5-5"},
		modelRow{"corp", "claude-sonnet-5-5"},
	)
	m = pick(m, "s", "o", "n", "n", "e", "t", " ", "o", "p", "e", "n")
	got := m.filteredModels()
	if len(got) != 1 || got[0].id != "anthropic/claude-sonnet-5-5" {
		t.Fatalf("'sonnet open' → %v", got)
	}
	m.modelQuery.SetValue("QWEN")
	if n := len(m.filteredModels()); n != 2 {
		t.Errorf("search ignores case: %d rows", n)
	}
	if v := stripANSI(m.modelPanel()); !strings.Contains(v, "openrouter") || !strings.Contains(v, "qwen/qwen3-coder") {
		t.Errorf("panel lacks the provider column:\n%s", v)
	}
}

// Rows from a provider that answers later merge in, provider order first,
// and the cursor stays on the row it was on.
func TestModelPicker_LateProviderKeepsTheSelection(t *testing.T) {
	m := pickerWith()
	m.modelPending = 2
	m.addModelRows(modelFetchMsg{provider: "b", ids: []string{"y", "x"}})
	m = pick(m, "down")
	if r, _ := m.selectedModel(); r.id != "y" {
		t.Fatalf("selected %v", r)
	}
	m.addModelRows(modelFetchMsg{provider: "a", ids: []string{"z"}})
	if r, _ := m.selectedModel(); r.id != "y" {
		t.Errorf("a late answer moved the cursor to %v", r)
	}
	m.addModelRows(modelFetchMsg{provider: "c", err: "no model list"})
	if m.modelPending != 0 || len(m.modelErrs) != 1 {
		t.Errorf("pending %d, errs %v", m.modelPending, m.modelErrs)
	}
}

// The form validates, saves the provider and opens the picker on it.
func TestProviderForm_SavesAndSearchesTheNewProvider(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	m := newTUIModel(nil, ".")
	m.openProviderForm()
	m = pick(m, "open router", "enter")
	if m.providerErr == "" || m.providerStep != 0 {
		t.Fatal("a name with a space must be refused")
	}
	m.providerFields[0].SetValue("")
	m = pick(m, "openrouter", "enter", "openrouter.ai", "enter")
	if m.providerStep != 1 || m.providerErr == "" {
		t.Fatal("a URL without a scheme must be refused")
	}
	m.providerFields[1].SetValue("")
	m = pick(m, "https://openrouter.ai/api/v1/", "enter", "sk-or-1", "enter")
	if m.providerForm || !m.modelPicking || m.modelQuery.Value() != "openrouter " {
		t.Fatalf("want the picker filtered to the new provider; form=%v picking=%v q=%q",
			m.providerForm, m.modelPicking, m.modelQuery.Value())
	}
	p, ok := loadConfig().providerAt("https://openrouter.ai/api/v1")
	if !ok || p.Name != "openrouter" || p.Key != "sk-or-1" {
		t.Errorf("saved %+v %v", p, ok)
	}
	if len(loadConfig().Providers) != 2 {
		t.Errorf("the default provider must be kept: %+v", loadConfig().Providers)
	}
}
