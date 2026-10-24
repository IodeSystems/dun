package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// A config from before providers is one provider; adding one keeps it.
func TestProviders_LegacyConfigIsOneProvider(t *testing.T) {
	c := dunConfig{URL: "https://llm.example.com", Key: "sk-1", Model: "m"}
	if got, ok := c.active(); !ok || got.Name != "llm.example.com" || got.Key != "sk-1" {
		t.Fatalf("legacy active = %+v", got)
	}
	c.putProvider(providerConfig{Name: "openrouter", URL: "https://openrouter.ai/api/v1", Key: "sk-or"})
	if len(c.Providers) != 2 || c.activeName() != "llm.example.com" {
		t.Fatalf("add must keep the legacy provider active: %+v", c.Providers)
	}
	if p, ok := c.providerAt("https://openrouter.ai/api/v1/"); !ok || p.Key != "sk-or" {
		t.Errorf("providerAt ignores a trailing slash; got %+v %v", p, ok)
	}
	c.use(c.Providers[1], "anthropic/x")
	if c.activeName() != "openrouter" || c.URL != "https://openrouter.ai/api/v1" || c.Key != "sk-or" || c.Model != "anthropic/x" {
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
		var msg tea.KeyPressMsg
		switch k {
		case "down", "up", "tab", "enter", "esc":
			msg = map[string]tea.KeyPressMsg{"down": {Code: tea.KeyDown}, "up": {Code: tea.KeyUp},
				"tab": {Code: tea.KeyTab}, "enter": {Code: tea.KeyEnter}, "esc": {Code: tea.KeyEscape}}[k]
		default:
			msg = runesKey([]rune(k))
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
	m.models.query.SetValue("QWEN")
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
	m = pick(m, "openrouter.ai", "enter")
	if m.providerStep != fieldAddr || m.providerErr == "" {
		t.Fatal("a URL without a scheme must be refused")
	}
	m.providerFields[fieldAddr].SetValue("")
	m = pick(m, "https://openrouter.ai/api/v1/", "enter")
	if got := m.providerFields[fieldName].Value(); got != "openrouter" || m.providerStep != fieldKey {
		t.Fatalf("the name is filled in from the address and not asked: %q (step %d)", got, m.providerStep)
	}
	m = pick(m, "sk-or-1", "enter")
	m.loginChecked(loginCheckedMsg{models: modelFetchMsg{provider: m.providerChecking, ids: []string{"x"}}})
	if m.providerForm || !m.modelPicking || m.models.query.Value() != "" { // the only service: no filter
		t.Fatalf("want the picker filtered to the new provider; form=%v picking=%v q=%q",
			m.providerForm, m.modelPicking, m.models.query.Value())
	}
	p, ok := loadConfig().providerAt("https://openrouter.ai/api/v1")
	if !ok || p.Name != "openrouter" || p.Key != "sk-or-1" {
		t.Errorf("saved %+v %v", p, ok)
	}
	if c := loadConfig(); len(c.Providers) != 1 || c.activeName() != "openrouter" {
		t.Errorf("the first login is the only provider and the default: %+v", c.Providers)
	}
}

// No built-in provider: an empty config is logged out, and the provider
// operations every client uses log in, switch and log out.
func TestApplyProviderOp(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if ps := loadConfig().providerList(); len(ps) != 0 {
		t.Fatalf("an empty config must have no provider (no built-in fallback): %+v", ps)
	}
	if _, ok := loadConfig().active(); ok {
		t.Fatal("an empty config must be logged out")
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
	// The first login is the default, bearer optional, model not yet chosen.
	do(providerOp{Op: "login", Name: "local", URL: "http://127.0.0.1:8080/"})
	if c := loadConfig(); c.activeName() != "local" || c.URL != "http://127.0.0.1:8080" || c.Key != "" || c.Model != "" {
		t.Fatalf("first login: %+v", c)
	}
	do(providerOp{Op: "login", Name: "corp", URL: "https://litellm.corp.example/v1/", Key: "sk-corp"})
	if c := loadConfig(); c.activeName() != "local" || c.Providers[1].Kind != providerOpenAI {
		t.Fatalf("a second login must not take the default: %+v", c)
	}
	do(providerOp{Op: "use", Name: "corp", Model: "gpt-x"})
	if c := loadConfig(); c.activeName() != "corp" || c.URL != "https://litellm.corp.example/v1" || c.Model != "gpt-x" {
		t.Fatalf("use: %+v", c)
	}
	// Logging in again with no key keeps it; the default follows the new URL.
	do(providerOp{Op: "login", Name: "corp", URL: "https://litellm2.corp.example"})
	if c := loadConfig(); c.URL != "https://litellm2.corp.example" || c.Key != "sk-corp" || c.Model != "gpt-x" {
		t.Fatalf("login again: %+v", c)
	}
	// Logging out of the default moves it, with no model until one is picked.
	if msg := do(providerOp{Op: "logout", Name: "corp"}); !strings.Contains(msg, "local") {
		t.Errorf("logout answer: %q", msg)
	}
	if c := loadConfig(); c.activeName() != "local" || c.Model != "" {
		t.Fatalf("after logging out of the default: %+v", c)
	}
	// Logging out of the last leaves no provider at all.
	do(providerOp{Op: "logout", Name: "local"})
	if c := loadConfig(); len(c.providerList()) != 0 || c.URL != "" || c.Key != "" || c.Model != "" {
		t.Fatalf("after the last logout: %+v", c)
	}
	for _, bad := range []providerOp{{Op: "logout", Name: "nope"}, {Op: "login", Name: "a b", URL: "https://x"},
		{Op: "login", Name: "x", URL: "x.example"}, {Op: "login", Name: "x", URL: "https://x", Kind: "anthropic"},
		{Op: "use", Name: "nope", Model: "m"}, {Op: "add", Name: "x", URL: "https://x"}} {
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

// /login under a saved name: the name is fixed, the URL is filled in, an
// empty key keeps the saved one.
func TestProviderForm_LoginAgainKeepsTheKey(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if err := saveConfig(dunConfig{URL: "https://old.example.com", Key: "sk-old", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(nil, ".")
	loginSlash(&m, []string{"old.example.com"})
	if !m.providerForm || m.providerEdit != "old.example.com" || m.providerStep != fieldAddr || m.providerFields[fieldAddr].Value() != "https://old.example.com" {
		t.Fatalf("edit form: form=%v edit=%q step=%d url=%q", m.providerForm, m.providerEdit, m.providerStep, m.providerFields[fieldAddr].Value())
	}
	m.providerFields[fieldAddr].SetValue("")
	m = pick(m, "https://new.example.com", "enter")
	if m.providerStep != fieldKey || m.providerFields[fieldName].Value() != "old.example.com" {
		t.Fatalf("the fixed name is stepped over and kept: step %d name %q", m.providerStep, m.providerFields[fieldName].Value())
	}
	m = pick(m, "enter")
	if m.providerChecking == "" { // asking the service for its models
		t.Fatalf("not checked: %q", m.providerErr)
	}
	if c := loadConfig(); c.URL != "https://old.example.com" {
		t.Fatalf("saved before the check: %q", c.URL)
	}
	m.loginChecked(loginCheckedMsg{models: modelFetchMsg{provider: m.providerChecking, ids: []string{"m"}}})
	if c := loadConfig(); c.URL != "https://new.example.com" || c.Key != "sk-old" || c.activeName() != "old.example.com" {
		t.Errorf("after login again: url=%q key kept=%v active=%q", c.URL, c.Key == "sk-old", c.activeName())
	}
}
