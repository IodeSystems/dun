package main

import (
	"net/http"
	"net/http/httptest"
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
	m.openProviderForm(nil)
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

// The provider operations every client uses: a bad default can be edited,
// replaced or removed, and a model used by id when there is no list.
func TestApplyProviderOp(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if err := saveConfig(dunConfig{URL: "https://home.example.com", Key: "sk-home", Model: "gone"}); err != nil {
		t.Fatal(err)
	}
	do := func(op providerOp) string {
		t.Helper()
		msg, err := applyProviderOp(op)
		if err != nil {
			t.Fatalf("%+v: %v", op, err)
		}
		if strings.Contains(msg, "sk-") {
			t.Fatalf("a key in the answer: %q", msg)
		}
		return msg
	}
	if _, err := applyProviderOp(providerOp{Op: "remove", Name: "home.example.com"}); err == nil {
		t.Fatal("the only provider must not be removable")
	}
	do(providerOp{Op: "add", Name: "corp", URL: "https://litellm.corp.example/v1/", Key: "sk-corp"})
	do(providerOp{Op: "use", Name: "corp", Model: "gpt-x"})
	if c := loadConfig(); c.active().Name != "corp" || c.URL != "https://litellm.corp.example/v1" || c.Model != "gpt-x" {
		t.Fatalf("use: %+v", c)
	}
	// Edit with no key keeps it, and the default follows the new URL.
	do(providerOp{Op: "edit", Name: "corp", URL: "https://litellm2.corp.example"})
	if c := loadConfig(); c.URL != "https://litellm2.corp.example" || c.Key != "sk-corp" {
		t.Fatalf("edit: %+v", c)
	}
	// Removing the default moves it to the next provider.
	if msg := do(providerOp{Op: "remove", Name: "corp"}); !strings.Contains(msg, "home.example.com") {
		t.Errorf("remove answer: %q", msg)
	}
	if c := loadConfig(); c.active().Name != "home.example.com" || c.Key != "sk-home" {
		t.Fatalf("after removing the default: %+v", c)
	}
	for _, bad := range []providerOp{{Op: "edit", Name: "nope", URL: "https://x"}, {Op: "add", Name: "a b", URL: "https://x"},
		{Op: "add", Name: "x", URL: "x.example"}, {Op: "use", Name: "home.example.com", Model: ""}, {Op: "frob", Name: "x"}} {
		if _, err := applyProviderOp(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// A failed listing says why; a base ending in /v1 (LiteLLM) still lists.
func TestListModelsSaysWhy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, `{"error":"invalid key"}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-x"}]}`))
	}))
	defer srv.Close()
	if ids, why := listModels(srv.URL+"/v1", "good"); len(ids) != 1 || why != "" {
		t.Errorf("a /v1 base: %v %q", ids, why)
	}
	if _, why := listModels(srv.URL, "bad"); !strings.Contains(why, "401") || !strings.Contains(why, "invalid key") {
		t.Errorf("why = %q", why)
	}
}
