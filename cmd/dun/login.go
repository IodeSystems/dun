package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// /login starts with a picker of services: custom (any OpenAI-compatible
// address and key, the form as it was), then the templates below. A
// template fills in the address and the name, and knows whether it needs a
// key and the variable that usually holds one: a key found there is offered,
// never shown. Then the form, the check (the service answers with its
// models), the save, and the model picker — the same for every service.

// loginTmpl is one service the login knows.
type loginTmpl struct {
	id      string   // the name it is saved under (openai, ollama…)
	label   string   // what the picker says
	url     string   // its OpenAI-compatible base
	keyEnv  []string // where a key usually is, in order; nil: a local server, no key
	keysAt  string   // where to get a key, in a few words
	hostEnv string   // a variable that moves a local server (OLLAMA_HOST)
}

// loginTemplates is the picker's list, in its order (the owner's: the
// services in use first, then the big two, then cheap open models, the
// rest, and the local servers). One row per service; add one here.
//
// Not here yet: Google Gemini. Its OpenAI-compatible base
// (generativelanguage.googleapis.com/v1beta/openai) does not end in /v1,
// and the engine's client adds /v1 to any base that does not, so its chat
// would go to …/openai/v1/chat/completions, which does not exist. It goes
// in once agentkit's chatURL takes such a base as it is.
var loginTemplates = []loginTmpl{
	{id: "openrouter", label: "OpenRouter", url: "https://openrouter.ai/api/v1", keyEnv: []string{"OPENROUTER_API_KEY"}, keysAt: "openrouter.ai/keys"},
	{id: "fireworks", label: "Fireworks", url: "https://api.fireworks.ai/inference/v1", keyEnv: []string{"FIREWORKS_API_KEY"}, keysAt: "fireworks.ai/account/api-keys"},
	{id: "openai", label: "OpenAI", url: "https://api.openai.com/v1", keyEnv: []string{"OPENAI_API_KEY"}, keysAt: "platform.openai.com/api-keys"},
	{id: "anthropic", label: "Anthropic", url: "https://api.anthropic.com/v1", keyEnv: []string{"ANTHROPIC_API_KEY"}, keysAt: "console.anthropic.com/settings/keys"},
	{id: "qwen", label: "Qwen", url: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", keyEnv: []string{"DASHSCOPE_API_KEY"}, keysAt: "Alibaba Cloud Model Studio"},
	{id: "deepseek", label: "DeepSeek", url: "https://api.deepseek.com/v1", keyEnv: []string{"DEEPSEEK_API_KEY"}, keysAt: "platform.deepseek.com/api_keys"},
	{id: "groq", label: "Groq", url: "https://api.groq.com/openai/v1", keyEnv: []string{"GROQ_API_KEY"}, keysAt: "console.groq.com/keys"},
	{id: "mistral", label: "Mistral", url: "https://api.mistral.ai/v1", keyEnv: []string{"MISTRAL_API_KEY"}, keysAt: "console.mistral.ai/api-keys"},
	{id: "xai", label: "xAI", url: "https://api.x.ai/v1", keyEnv: []string{"XAI_API_KEY"}, keysAt: "console.x.ai"},
	{id: "together", label: "Together", url: "https://api.together.xyz/v1", keyEnv: []string{"TOGETHER_API_KEY"}, keysAt: "api.together.ai/settings/api-keys"},
	{id: "ollama", label: "Ollama", url: "http://localhost:11434/v1", hostEnv: "OLLAMA_HOST"},
	{id: "lmstudio", label: "LM Studio", url: "http://localhost:1234/v1"},
	{id: "llamacpp", label: "llama.cpp", url: "http://localhost:8080/v1"},
}

// loginCustom is the picker's first row, always: the address and key a
// person was given, for a service no template knows.
var loginCustom = loginTmpl{id: "custom", label: "custom"}

func (t loginTmpl) custom() bool { return t.id == loginCustom.id }

// local is a server on this computer: no key.
func (t loginTmpl) local() bool { return !t.custom() && t.keyEnv == nil }

// addr is the template's address: its hostEnv's server when that is set.
func (t loginTmpl) addr() string {
	if t.hostEnv != "" {
		if u := hostEnvURL(os.Getenv(t.hostEnv), t.url); u != "" {
			return u
		}
	}
	return t.url
}

// hostEnvURL is the base for a host variable's value, as Ollama reads its
// own: host, host:port or a URL; 0.0.0.0 (listen everywhere) is asked at
// localhost; the port and /v1 are def's. "" for an empty or bad value.
func hostEnvURL(v, def string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.Contains(v, "://") {
		v = "http://" + v
	}
	u, err := url.Parse(v)
	d, derr := url.Parse(def)
	if err != nil || derr != nil || u.Hostname() == "" {
		return ""
	}
	host, port := u.Hostname(), u.Port()
	if host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	if port == "" {
		port = d.Port()
	}
	return u.Scheme + "://" + net.JoinHostPort(host, port) + d.Path
}

// envKey is the first of the template's key variables that is set, "" for
// none. Its name, never its value, is what the form says.
func (t loginTmpl) envKey() string {
	for _, e := range t.keyEnv {
		if strings.TrimSpace(os.Getenv(e)) != "" {
			return e
		}
	}
	return ""
}

// templateNamed finds a template by its id or label (/login openai).
func templateNamed(name string) (loginTmpl, bool) {
	for _, t := range loginTemplates {
		if strings.EqualFold(t.id, name) || strings.EqualFold(t.label, name) {
			return t, true
		}
	}
	return loginTmpl{}, false
}

// ── the service picker ──

// openLogin opens the service picker with query typed in, and asks each
// local server whether it is running (a short probe: the answer marks its
// row, and none leaves the row as it was).
func (m *tuiModel) openLogin(query string) tea.Cmd {
	m.logins = newPickList(query, "type to filter")
	m.loginPicking, m.modelPicking, m.providerForm = true, true, false
	m.loginProbe = map[string]int{}
	m.logins.sel = m.firstLogin()
	// The kept question's line (or the not-logged-in one) said "type /login":
	// done. esc puts it back (loginCancelled).
	m.dropNeedLogin()
	m.input.Blur()
	m.refresh()
	cmds := []tea.Cmd{textinput.Blink}
	for _, t := range loginTemplates {
		if t.local() {
			cmds = append(cmds, probeLocal(t))
		}
	}
	return tea.Batch(cmds...)
}

// loginProbeMsg is a local server's answer: n models, or -1 for none.
type loginProbeMsg struct {
	id string
	n  int
}

func probeLocal(t loginTmpl) tea.Cmd {
	return func() tea.Msg {
		ids, _ := askModels(&http.Client{Timeout: 250 * time.Millisecond}, t.addr(), "")
		if len(ids) == 0 {
			return loginProbeMsg{t.id, -1}
		}
		return loginProbeMsg{t.id, len(ids)}
	}
}

// filteredLogins is custom, then the templates that match the query (in
// any case, every word, in the name or the address). Custom is never
// filtered away: a query that matches nothing still leaves the one row that
// takes any address.
func (m tuiModel) filteredLogins() []loginTmpl {
	out := []loginTmpl{loginCustom}
	words := m.logins.words()
	for _, t := range loginTemplates {
		if matchWords(words, t.id+" "+t.label+" "+t.addr()) {
			out = append(out, t)
		}
	}
	return out
}

// firstLogin is where the cursor goes for the query: custom with nothing
// typed (or nothing matching), else the first template that matches — a
// typed "groq" is picked with enter, not custom above it.
func (m tuiModel) firstLogin() int {
	if len(m.logins.words()) > 0 && len(m.filteredLogins()) > 1 {
		return 1
	}
	return 0
}

func (m tuiModel) updateLoginPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	shown := m.filteredLogins()
	if m.logins.move(msg.String(), len(shown), pickWindow(m.h)) {
		m.refresh()
		return m, nil
	}
	switch msg.String() {
	case "esc", "ctrl+c":
		m.loginPicking = false
		m.closeModelPicker()
		m.loginCancelled()
		return m, nil
	case "enter":
		t := shown[min(m.logins.sel, len(shown)-1)]
		m.loginPicking = false
		if t.custom() {
			cmd := m.openProviderForm(nil)
			m.providerBack = true
			return m, cmd
		}
		return m, m.openTemplateForm(t, true)
	}
	return m.typeLoginQuery(msg)
}

// typeLoginQuery is a key or a paste in the service picker's search box.
func (m tuiModel) typeLoginQuery(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd, changed := m.logins.typeKey(msg)
	if changed {
		m.logins.sel = m.firstLogin()
	}
	m.refresh()
	return m, cmd
}

// loginLabelW is the picker's name column: its longest label.
func loginLabelW() int {
	w := len(loginCustom.label)
	for _, t := range loginTemplates {
		w = max(w, len(t.label))
	}
	return w
}

// loginPanel renders the service picker: what each row is, after its name
// — custom says what it takes, a template its host, a local server that
// answered says it is running.
func (m tuiModel) loginPanel() string {
	rows := []string{stHeader.Render("log in to an AI service"), listIndent + m.logins.query.View()}
	shown := m.filteredLogins()
	window := pickWindow(m.h)
	lo, hi := m.logins.span(len(shown), window)
	var list []string
	for i := lo; i < hi; i++ {
		t := shown[i]
		what := "any OpenAI-compatible service"
		if !t.custom() {
			what = hostPort(t.addr())
			if n := m.loginProbe[t.id]; n > 0 {
				what = "running · " + plural(n, "model") + " · " + what
			}
		}
		name := fmt.Sprintf("%-*s", loginLabelW(), t.label)
		list = append(list, listRow("", name, "  "+stDim.Render(what), i == m.logins.sel))
	}
	rows = append(rows, padPick(list, 1+len(loginTemplates), window)...)
	// Always a row, said or blank (pickList: the rows do not move).
	status := ""
	switch {
	case len(shown) == 1 && len(m.logins.words()) > 0:
		status = "no service by that name — custom takes any address"
	case lo > 0 || hi < len(shown):
		status = fmt.Sprintf("%d-%d of %d", lo+1, hi, len(shown))
	}
	rows = append(rows, listIndent+stDim.Render(status))
	return strings.Join(rows, "\n")
}

// hostPort is an address's host (and port, when it has one).
func hostPort(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// loginHint is the service picker's status line.
func loginHint() string {
	return "log in  ·  enter pick · esc cancel · type to filter"
}

// ── the form, from a template ──

// openTemplateForm opens the login form filled in from t: its address and
// name; the key's row asks for a key, offers the one in t's variable, or is
// not there (a local server). back: esc returns to the picker.
func (m *tuiModel) openTemplateForm(t loginTmpl, back bool) tea.Cmd {
	cmd := m.openProviderForm(nil)
	m.providerTmpl, m.providerBack = &t, back
	addr := t.addr()
	m.providerFields[fieldAddr].SetValue(addr)
	m.providerFields[fieldAddr].CursorEnd()
	m.providerAuto = loadConfig().freeName(t.id, addr)
	m.providerFields[fieldName].SetValue(m.providerAuto)
	m.providerEnvKey = t.envKey()
	if m.providerEnvKey != "" {
		m.providerFields[fieldKey].Placeholder = placeholder("found in $" + m.providerEnvKey + " — enter to use it")
	} else {
		m.providerFields[fieldKey].Placeholder = placeholder("paste your " + t.label + " key")
	}
	if !t.local() { // the address is right already: the key is what is asked
		m.providerFields[fieldAddr].Blur()
		m.providerStep = fieldKey
		m.providerFields[fieldKey].Focus()
	}
	m.refresh()
	return cmd
}

// lastStep is the form's last field: the address for a local server (no
// key), else the key.
func (m tuiModel) lastStep() int {
	if m.providerTmpl != nil && m.providerTmpl.local() {
		return fieldAddr
	}
	return fieldKey
}

// loginFail says why the check of a login failed, in the form: the key was
// refused, or nothing answered — for a local server, whether it is running.
// "" when the service answered (even without a model list: it is saved, and
// the picker says what is missing).
func (m tuiModel) loginFail(u string, f modelsFail) string {
	switch {
	case f.refused:
		return "the key was refused — " + f.why
	case f.unreachable:
		what := "the service"
		if m.providerTmpl != nil {
			what = m.providerTmpl.label
		}
		if h := hostOnly(u); h == "localhost" || net.ParseIP(h) != nil {
			return "nothing is answering at " + hostPort(u) + " — is " + what + " running?"
		}
		return "could not reach " + hostPort(u) + " — " + f.why
	}
	return ""
}

func hostOnly(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Hostname()
	}
	return ""
}
