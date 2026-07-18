package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func setupKey(m setupModel, k tea.KeyMsg) setupModel {
	nm, _ := m.Update(k)
	return nm.(setupModel)
}

// URL → key → model(list), preselecting the current model, then pick one.
func TestSetupWizard_pickFromList(t *testing.T) {
	m := newSetupModel(dunConfig{URL: "http://x:1", Model: "b", Key: "sk-1"})
	if m.step != stepURL {
		t.Fatal("wizard should start on the URL step")
	}
	m = setupKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.step != stepKey {
		t.Fatal("enter should advance URL → key")
	}
	m = setupKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.step != stepModel || !m.fetching {
		t.Fatalf("enter should advance key → model and start fetching (step=%d fetching=%v)", m.step, m.fetching)
	}
	// Models arrive; the current model ("b") is preselected.
	nm, _ := m.Update(modelsMsg{"a", "b", "c"})
	m = nm.(setupModel)
	if m.fetching {
		t.Fatal("modelsMsg should end fetching")
	}
	if m.sel != 1 {
		t.Fatalf("current model 'b' should be preselected (idx 1), got %d", m.sel)
	}
	// View renders without panic and shows the list.
	if v := stripANSI(m.View()); !strings.Contains(v, "dun setup") || !strings.Contains(v, "c") {
		t.Fatalf("view missing content: %q", v)
	}
	// ↓ to 'c', enter → chosen + saved.
	m = setupKey(m, tea.KeyMsg{Type: tea.KeyDown})
	m = setupKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.saved || m.chosenModel != "c" {
		t.Fatalf("expected chosen=c saved=true, got chosen=%q saved=%v", m.chosenModel, m.saved)
	}
}

// No model list (offline / endpoint lacks /models) → type a name.
func TestSetupWizard_customModel(t *testing.T) {
	m := newSetupModel(dunConfig{})
	m.step = stepModel
	nm, _ := m.Update(modelsMsg{}) // empty → custom row
	m = nm.(setupModel)
	if !m.customRow() {
		t.Fatal("empty model list should land on the custom row")
	}
	m.custom.SetValue("qwen3-next")
	m = setupKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.saved || m.chosenModel != "qwen3-next" {
		t.Fatalf("expected chosen=qwen3-next saved=true, got chosen=%q saved=%v", m.chosenModel, m.saved)
	}
}

// esc cancels without saving.
func TestSetupWizard_cancel(t *testing.T) {
	m := setupKey(newSetupModel(dunConfig{}), tea.KeyMsg{Type: tea.KeyEsc})
	if m.saved {
		t.Fatal("esc must not save")
	}
}
