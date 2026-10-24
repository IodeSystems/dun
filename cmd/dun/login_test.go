package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// loginWith is the service picker, opened at w×h with no saved login and no
// key variable set.
func loginWith(t *testing.T, w, h int) tuiModel {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	for _, tm := range loginTemplates {
		for _, e := range append(tm.keyEnv, tm.hostEnv) {
			if e != "" {
				t.Setenv(e, "")
			}
		}
	}
	m := newTUIModel(nil, ".")
	m.w, m.h = w, h
	m.openLogin("")
	return m
}

// The table's order is the owner's: the services in use first, then the
// big two, the open models, the rest, the local servers.
func TestLogin_TemplateOrder(t *testing.T) {
	var ids []string
	for _, tm := range loginTemplates {
		ids = append(ids, tm.id)
	}
	if got := strings.Join(ids[:6], " "); got != "openrouter fireworks openai anthropic qwen deepseek" {
		t.Errorf("first six: %s", got)
	}
	if got := strings.Join(ids[len(ids)-3:], " "); got != "ollama lmstudio llamacpp" {
		t.Errorf("last three: %s", got)
	}
}

// Custom is the first row and never filtered away; typing narrows the
// templates (any case, name or address) and puts the cursor on the first
// match, so enter picks what was typed; nothing matching leaves custom.
func TestLogin_PickerFiltersCustomFirst(t *testing.T) {
	m := loginWith(t, 100, 30)
	if got := m.filteredLogins(); got[0].id != "custom" || len(got) != 1+len(loginTemplates) || m.logins.sel != 0 {
		t.Fatalf("unfiltered: %d rows, first %q, at %d", len(got), got[0].id, m.logins.sel)
	}
	m = pick(m, "GROQ")
	if got := m.filteredLogins(); len(got) != 2 || got[0].id != "custom" || got[1].id != "groq" || m.logins.sel != 1 {
		t.Fatalf("groq: %+v at %d", got, m.logins.sel)
	}
	m = loginWith(t, 100, 30)
	m = pick(m, "localhost")
	if got := m.filteredLogins(); len(got) != 4 || got[1].id != "ollama" {
		t.Errorf("by address: %+v", got)
	}
	m = loginWith(t, 100, 30)
	m = pick(m, "zzz")
	if got := m.filteredLogins(); len(got) != 1 || m.logins.sel != 0 {
		t.Errorf("nothing matches: %+v at %d", got, m.logins.sel)
	}
	if p := stripANSI(m.loginPanel()); !strings.Contains(p, "custom takes any address") {
		t.Errorf("no word for an empty match:\n%s", p)
	}
	m = pick(m, "enter")
	if !m.providerForm || m.providerTmpl != nil || m.providerStep != fieldAddr {
		t.Errorf("enter on custom: form %v tmpl %v step %d", m.providerForm, m.providerTmpl, m.providerStep)
	}
}

// Typing into either list keeps its height: blank rows for the missing
// matches, so the search line and the divider stay where they are.
func TestLogin_ListsKeepTheirHeight(t *testing.T) {
	for _, size := range [][2]int{{49, 21}, {100, 30}} {
		m := loginWith(t, size[0], size[1])
		n := len(strings.Split(m.modelPanel(), "\n"))
		for _, q := range []string{"o", "op", "openr", "zzz"} {
			mm := pick(m, q)
			if got := len(strings.Split(mm.modelPanel(), "\n")); got != n {
				t.Errorf("%v login %q: %d rows, want %d", size, q, got, n)
			}
		}
		pm := pickerWith(modelRow{"a", "chat-fast"}, modelRow{"a", "chat-large"}, modelRow{"a", "code-large"}, modelRow{"a", "embed-small"})
		pm.w, pm.h = size[0], size[1]
		n = len(strings.Split(pm.modelPanel(), "\n"))
		for _, q := range []string{"c", "chat", "chat-f", "zzz"} {
			mm := pick(pm, q)
			if got := len(strings.Split(mm.modelPanel(), "\n")); got != n {
				t.Errorf("%v models %q: %d rows, want %d", size, q, got, n)
			}
		}
	}
}

// At 49 columns every row of the picker fits, and one cut ends at a word
// with an ellipsis.
func TestLogin_PhoneRowsFit(t *testing.T) {
	m := loginWith(t, 49, 21)
	m.loginProbe["ollama"] = 4
	m = pick(m, "local")
	for _, r := range strings.Split(fitRows(m.modelPanel(), 49), "\n") {
		if w := ansi.StringWidth(r); w > 49 {
			t.Errorf("row %d wide: %q", w, stripANSI(r))
		}
	}
	if p := stripANSI(fitRows(m.modelPanel(), 49)); !strings.Contains(p, "Ollama      running · 4 models") {
		t.Errorf("the probe's answer is not on the row:\n%s", p)
	}
}

// A template with its key variable set offers it, by name: the key is
// never shown, and enter uses it.
func TestLogin_TemplateOffersTheKeyInTheEnv(t *testing.T) {
	m := loginWith(t, 100, 30)
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real")
	m = pick(m, "openai", "enter")
	if !m.providerForm || m.providerTmpl == nil || m.providerTmpl.id != "openai" || m.providerStep != fieldKey {
		t.Fatalf("form %v step %d", m.providerForm, m.providerStep)
	}
	p := stripANSI(m.providerFormPanel())
	for _, want := range []string{"log in to OpenAI", "https://api.openai.com/v1", "found in $OPENAI_API_KEY — enter to use it", "as openai"} {
		if !strings.Contains(p, want) {
			t.Errorf("no %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "sk-test") || strings.Contains(m.providerHint(), "sk-test") {
		t.Errorf("the key is on the screen:\n%s", p)
	}
	m.saveProvider()
	if m.providerPending.Key != "sk-test-not-real" || m.providerPending.Name != "openai" {
		t.Errorf("pending %+v", m.providerPending.Name)
	}
}

// With no key in the env, the form asks for one and says where to get it;
// esc goes back to the picker as it was left.
func TestLogin_TemplateAsksForTheKey(t *testing.T) {
	m := loginWith(t, 100, 30)
	m = pick(m, "router", "enter")
	p := stripANSI(m.providerFormPanel())
	if !strings.Contains(p, "paste your OpenRouter key") || !strings.Contains(p, "get a key at openrouter.ai/keys") {
		t.Errorf("form:\n%s", p)
	}
	if !strings.Contains(m.providerHint(), "esc back") {
		t.Errorf("hint %q", m.providerHint())
	}
	m = pick(m, "esc")
	if !m.loginPicking || m.providerForm || m.logins.query.Value() != "router" {
		t.Errorf("esc: picker %v form %v query %q", m.loginPicking, m.providerForm, m.logins.query.Value())
	}
}

// A local server's form is its address only: no key row, enter logs in.
func TestLogin_LocalHasNoKey(t *testing.T) {
	m := loginWith(t, 100, 30)
	m = pick(m, "studio", "enter")
	p := stripANSI(m.providerFormPanel())
	if strings.Contains(p, "key  ") || !strings.Contains(p, "http://localhost:1234/v1") || !strings.Contains(p, "no key") {
		t.Errorf("form:\n%s", p)
	}
	if m.lastStep() != fieldAddr || !strings.Contains(m.providerHint(), "enter log in") {
		t.Errorf("last step %d, hint %q", m.lastStep(), m.providerHint())
	}
}

// OLLAMA_HOST moves the Ollama template, in any of the forms Ollama reads.
func TestLogin_OllamaHost(t *testing.T) {
	def := "http://localhost:11434/v1"
	for in, want := range map[string]string{
		"":                    "",
		"0.0.0.0":             "http://localhost:11434/v1",
		"127.0.0.1:9999":      "http://127.0.0.1:9999/v1",
		"https://gpu.lan:443": "https://gpu.lan:443/v1",
		"http://gpu.lan":      "http://gpu.lan:11434/v1",
	} {
		if got := hostEnvURL(in, def); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	t.Setenv("OLLAMA_HOST", "127.0.0.1:9999")
	if tm, _ := templateNamed("ollama"); tm.addr() != "http://127.0.0.1:9999/v1" {
		t.Errorf("addr %q", tm.addr())
	}
}

// Nothing is saved before the service answers: a refused key and a server
// that is not running are said in the form, whole, which keeps what was
// typed.
func TestLogin_CheckBeforeSave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()
	m := loginWith(t, 49, 21)
	m = pick(m, "enter", srv.URL+"/v1", "enter", "sk-bad", "enter")
	cmd := m.saveProvider()
	m.loginChecked(cmd().(loginCheckedMsg))
	if !m.providerForm || !strings.HasPrefix(m.providerErr, "the key was refused") || m.providerStep != fieldKey {
		t.Fatalf("form %v err %q step %d", m.providerForm, m.providerErr, m.providerStep)
	}
	if _, ok := loadConfig().named("local"); ok || m.providerFields[fieldKey].Value() != "sk-bad" {
		t.Errorf("saved a refused login, or lost the key typed")
	}
	// Said whole at 49: every word of it is on the screen, no row too wide.
	p := fitRows(m.providerFormPanel(), 49)
	if !strings.Contains(strings.Join(strings.Fields(stripANSI(p)), " "), "invalid api key") {
		t.Errorf("the reason is cut:\n%s", stripANSI(p))
	}
	for _, r := range strings.Split(p, "\n") {
		if ansi.StringWidth(r) > 49 {
			t.Errorf("row %d wide: %q", ansi.StringWidth(r), stripANSI(r))
		}
	}

	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String() + "/v1"
	l.Close()
	m = loginWith(t, 100, 30)
	m = pick(m, "cpp", "enter")
	m.providerFields[fieldAddr].SetValue(dead)
	cmd = m.saveProvider()
	m.loginChecked(cmd().(loginCheckedMsg))
	if !strings.Contains(m.providerErr, "nothing is answering at "+hostPort(dead)+" — is llama.cpp running?") {
		t.Errorf("err %q", m.providerErr)
	}
	if len(loadConfig().providerList()) != 0 {
		t.Errorf("saved: %+v", loadConfig().providerList())
	}
}

// A service that answers is saved, under the template's name.
func TestLogin_TemplateSavesUnderItsName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3"}]}`))
	}))
	defer srv.Close()
	m := loginWith(t, 100, 30)
	m = pick(m, "ollama", "enter")
	m.providerFields[fieldAddr].SetValue(srv.URL + "/v1")
	cmd := m.saveProvider()
	m.loginChecked(cmd().(loginCheckedMsg))
	if p, ok := loadConfig().named("ollama"); !ok || p.URL != srv.URL+"/v1" || p.Key != "" {
		t.Fatalf("saved %+v %v (err %q)", p, ok, m.providerErr)
	}
	if m.providerForm || !m.modelPicking || len(m.modelRows) != 1 {
		t.Errorf("form %v picker %v rows %v", m.providerForm, m.modelPicking, m.modelRows)
	}
}

// /login <template> opens that template's form; esc then cancels.
func TestLogin_SlashTemplateName(t *testing.T) {
	m := loginWith(t, 100, 30)
	loginSlash(&m, []string{"deepseek"})
	if !m.providerForm || m.providerTmpl == nil || m.providerTmpl.id != "deepseek" || m.providerBack {
		t.Errorf("form %v tmpl %v back %v", m.providerForm, m.providerTmpl, m.providerBack)
	}
}
