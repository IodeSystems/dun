package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// /model — pick a model from every configured provider, by typing part of its
// name. Each provider is an OpenAI-compatible endpoint with its own key
// (config.go, providerConfig); the picker asks all of them for /v1/models at
// once and lists what has come back so far, so a slow or dead provider does
// not hold up the others. Typing filters: every word must appear in
// "provider model", in any order. Choosing a model restarts the engine on
// that provider's URL and key; tab first makes it the saved default.
//
// /model add opens a form for a new provider (name, URL, key) and, once it is
// saved, the picker filtered to it.

// modelRow is one model one provider serves.
type modelRow struct{ provider, id string }

// modelFetchMsg carries one provider's model ids, or why there are none.
type modelFetchMsg struct {
	provider string
	ids      []string
	err      string
}

// pickerProviders returns the providers /model lists: the saved ones, plus
// the running one when it came from a flag or env var and is not saved.
func (m tuiModel) pickerProviders() []providerConfig {
	cfg := loadConfig()
	ps := append([]providerConfig(nil), cfg.providerList()...)
	if _, ok := cfg.providerAt(m.url); !ok && m.url != "" {
		ps = append(ps, providerConfig{Name: hostName(m.url), URL: m.url, Key: m.opts.key})
	}
	return ps
}

func (m tuiModel) providerNamed(name string) (providerConfig, bool) {
	for _, p := range m.pickerProviders() {
		if p.Name == name {
			return p, true
		}
	}
	return providerConfig{}, false
}

// modelSlash implements /model — picking among the logged-in providers'
// models (/login and /logout manage the providers):
//
//	/model                      the picker
//	/model <name>               switch to that model on this provider
//	/model <provider> <name>    switch to that model on that provider
//	/model use <provider> <id>  switch and save as default, by id — for a
//	                            provider that does not list its models
func modelSlash(m *tuiModel, args []string) tea.Cmd {
	if m.replaying {
		m.append(stDim.Render("this is a replay — there is no engine to resume with"))
		return nil
	}
	switch {
	case len(args) >= 1 && args[0] == "use":
		if len(args) != 3 {
			m.append(stErr.Render("usage: /model use <provider> <model-id>"))
			return nil
		}
		p, ok := loadConfig().named(args[1])
		if !ok {
			m.append(stErr.Render("not logged in to " + args[1] + " — /login " + args[1]))
			return nil
		}
		return m.switchModel(p, args[2], true)
	case len(args) == 2:
		p, ok := m.providerNamed(args[0])
		if !ok {
			m.append(stErr.Render("not logged in to " + args[0] + " — /login " + args[0]))
			return nil
		}
		return m.switchModel(p, args[1], false)
	case len(args) == 1:
		p, ok := m.providerNamed(m.provider)
		if !ok && m.url == "" {
			m.append(stAccent.Render(errNoLogin.Error()))
			return nil
		}
		if p.URL == "" {
			p = providerConfig{Name: m.provider, URL: m.url, Key: m.opts.key}
		}
		return m.switchModel(p, args[0], false)
	}
	return m.openModelPicker("")
}

// loginSlash implements /login [name]: the form (a key typed in the composer
// would be kept in the sent history), then the picker on that provider. A
// name already logged in opens with its URL and keeps its key unless a new
// one is typed. Changes go through applyProviderOp, as they do from the web.
func loginSlash(m *tuiModel, args []string) tea.Cmd {
	if len(args) > 1 {
		m.append(stErr.Render("usage: /login [name] — the URL and key go in the form"))
		return nil
	}
	if len(args) == 1 {
		if p, ok := loadConfig().named(args[0]); ok {
			return m.openProviderForm(&p)
		}
		cmd := m.openProviderForm(nil)
		m.providerFields[0].SetValue(args[0])
		return cmd
	}
	return m.openProviderForm(nil)
}

// logoutSlash implements /logout <name>.
func logoutSlash(m *tuiModel, args []string) tea.Cmd {
	if len(args) != 1 {
		m.append(stErr.Render("usage: /logout <provider>"))
		return nil
	}
	msg, err := applyProviderOp(providerOp{Op: "logout", Name: args[0]})
	if err != nil {
		m.append(stErr.Render(err.Error()))
		return nil
	}
	m.append(stDim.Render(msg))
	return nil
}

// openModelPicker opens the picker with query typed in, and asks every
// provider for its models.
func (m *tuiModel) openModelPicker(query string) tea.Cmd {
	q := textinput.New()
	q.Placeholder = "type to search"
	q.SetWidth(40) // without a width the placeholder shows as its first letter
	q.Prompt = "/ "
	q.SetValue(query)
	q.CursorEnd()
	q.Focus()
	m.modelQuery = q
	m.modelPicking, m.modelSel = true, 0
	m.modelRows, m.modelErrs = nil, nil
	m.input.Blur()
	ps := m.pickerProviders()
	m.modelPending = len(ps)
	m.refresh()
	cmds := []tea.Cmd{textinput.Blink}
	for _, p := range ps {
		cmds = append(cmds, func() tea.Msg {
			ids, why := listModels(p.URL, p.Key)
			if len(ids) == 0 {
				return modelFetchMsg{provider: p.Name, err: why + " — /model use " + p.Name + " <model-id>, or /login " + p.Name + " again"}
			}
			return modelFetchMsg{provider: p.Name, ids: ids}
		})
	}
	return tea.Batch(cmds...)
}

// addModelRows takes one provider's answer into the picker, keeping the
// selection on the row it was on.
func (m *tuiModel) addModelRows(msg modelFetchMsg) {
	if m.modelPending > 0 {
		m.modelPending--
	}
	if msg.err != "" {
		m.modelErrs = append(m.modelErrs, msg.provider+": "+msg.err)
		return
	}
	cur, had := m.selectedModel()
	order := map[string]int{}
	for i, p := range m.pickerProviders() {
		order[p.Name] = i
	}
	for _, id := range msg.ids {
		m.modelRows = append(m.modelRows, modelRow{msg.provider, id})
	}
	sort.SliceStable(m.modelRows, func(a, b int) bool {
		ra, rb := m.modelRows[a], m.modelRows[b]
		if ra.provider != rb.provider {
			return order[ra.provider] < order[rb.provider]
		}
		return ra.id < rb.id
	})
	// Before the user has moved, the cursor rests on the running model.
	if !had {
		cur = modelRow{m.provider, m.model}
	}
	for i, r := range m.filteredModels() {
		if r == cur {
			m.modelSel = i
		}
	}
}

// filteredModels returns the rows matching every word of the query.
func (m tuiModel) filteredModels() []modelRow {
	words := strings.Fields(strings.ToLower(m.modelQuery.Value()))
	if len(words) == 0 {
		return m.modelRows
	}
	var out []modelRow
	for _, r := range m.modelRows {
		hay := strings.ToLower(r.provider + " " + r.id)
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

func (m tuiModel) selectedModel() (modelRow, bool) {
	rows := m.filteredModels()
	if m.modelSel < 0 || m.modelSel >= len(rows) {
		return modelRow{}, false
	}
	return rows[m.modelSel], true
}

func (m *tuiModel) closeModelPicker() {
	m.modelPicking = false
	m.modelQuery.Blur()
	m.input.Focus()
	m.refresh()
}

// updateModelPicking owns the keys while the model list is open. Printable
// keys go to the search box, so moving is on the arrows and ctrl+p/ctrl+n.
func (m tuiModel) updateModelPicking(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.providerForm {
		return m.updateProviderForm(msg)
	}
	switch msg.String() {
	case "esc", "ctrl+c":
		m.closeModelPicker()
		return m, nil
	case "up", "ctrl+p":
		if m.modelSel > 0 {
			m.modelSel--
		}
		m.refresh()
		return m, nil
	case "down", "ctrl+n":
		if m.modelSel < len(m.filteredModels())-1 {
			m.modelSel++
		}
		m.refresh()
		return m, nil
	case "pgup":
		m.modelSel = max(0, m.modelSel-modelWindow)
		m.refresh()
		return m, nil
	case "pgdown":
		m.modelSel = max(0, min(len(m.filteredModels())-1, m.modelSel+modelWindow))
		m.refresh()
		return m, nil
	case "ctrl+a":
		return m, m.openProviderForm(nil)
	case "enter", "alt+enter":
		// The pick is the default for later sessions too: what a person picks
		// is what they expect tomorrow. alt+enter is for this session only.
		r, ok := m.selectedModel()
		if !ok {
			return m, nil
		}
		p, found := m.providerNamed(r.provider)
		if !found {
			return m, nil
		}
		m.closeModelPicker()
		return m, m.switchModel(p, r.id, msg.String() == "enter")
	}
	return m.typeModelQuery(msg)
}

// typeModelQuery is a key or a paste in the model search box.
func (m tuiModel) typeModelQuery(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	before := m.modelQuery.Value()
	m.modelQuery, cmd = m.modelQuery.Update(msg)
	if m.modelQuery.Value() != before {
		m.modelSel = 0
	}
	m.refresh()
	return m, cmd
}

// switchModel restarts the engine on provider p with model. If persist is
// true, p and model become the saved defaults.
func (m *tuiModel) switchModel(p providerConfig, model string, persist bool) tea.Cmd {
	if model == "" {
		return nil
	}
	if persist {
		cfg := loadConfig()
		cfg.use(p, model)
		if err := saveConfig(cfg); err != nil {
			m.append(stErr.Render("failed to save config: " + err.Error()))
			return nil
		}
	}
	note := "using " + model + " on " + p.Name + " — this session only"
	if persist {
		note = "using " + model + " on " + p.Name + " — saved as default"
	}
	if model == m.model && p.URL == m.url { // nothing to restart
		if persist {
			m.append(stOK.Render(note))
		} else {
			m.append(stDim.Render("already using model " + model))
		}
		return nil
	}
	// Said once the new engine is ready (afterSwitch): a line written now
	// would go with the conversation it is about to replace.
	m.switchNote = note

	// The service restarts the engine on the new endpoint, on this
	// conversation; its history comes back and is rendered afresh, with no
	// "resumed" marker — to the person nothing was resumed.
	m.quietReplay = true
	m.convo = nil
	m.cur, m.pendingTool = "", -1
	m.sel = -1
	m.busy, m.starting = false, true
	m.dropAsk()
	m.convoHold = 0 // the conversation is about to be replaced
	m.startingStart = time.Now()
	m.fatalErr, m.exitAnnounced = "", false
	m.restarts, m.restartStart = 0, time.Now()
	m.model, m.url, m.provider, m.keySet = model, p.URL, p.Name, p.Key != ""
	m.noLogin = p.URL == ""
	m.skipHistory = false
	m.refresh()

	m.opts.model, m.opts.url, m.opts.key = model, p.URL, p.Key
	if !m.proc.restartWith(m.opts) {
		m.append(stErr.Render("no session service right now — /reconnect, then switch again"))
		m.refresh()
	}
	return nil
}

const modelWindow = 10

// modelPanel renders the model picker in the lower pane.
func (m tuiModel) modelPanel() string {
	if m.providerForm {
		return m.providerFormPanel()
	}
	// The keys are on the status line (modelHint), like every other list's.
	rows := []string{stHeader.Render("pick a model"), m.modelQuery.View()}
	// One service: its name on every row says nothing.
	several := len(m.pickerProviders()) > 1 || slices.ContainsFunc(m.modelRows, func(r modelRow) bool { return r.provider != m.modelRows[0].provider })

	shown := m.filteredModels()
	lo := m.modelSel - modelWindow/2
	if lo < 0 {
		lo = 0
	}
	hi := lo + modelWindow
	if hi > len(shown) {
		hi = len(shown)
		if lo = hi - modelWindow; lo < 0 {
			lo = 0
		}
	}
	width := 0
	for _, r := range shown[lo:hi] {
		width = max(width, len(r.provider))
	}
	for i := lo; i < hi; i++ {
		r := shown[i]
		mark := "  "
		if r.id == m.model && r.provider == m.provider {
			mark = stTool.Render("• ") // running now
		}
		line := mark
		if several {
			line += stDim.Render(fmt.Sprintf("%-*s  ", width, r.provider))
		}
		rows = append(rows, listRow(line, r.id, "", i == m.modelSel))
	}

	var status []string
	switch {
	case len(shown) == 0 && m.modelPending == 0 && len(m.modelRows) > 0:
		status = append(status, "no model matches")
	case len(shown) > 0:
		status = append(status, fmt.Sprintf("%d-%d of %d", lo+1, hi, len(shown)))
	}
	if len(m.pickerProviders()) == 0 {
		status = append(status, "not logged in — ctrl+a to log in")
	}
	if m.modelPending > 0 {
		status = append(status, fmt.Sprintf("asking %s…", plural(m.modelPending, "service")))
	}
	rows = append(rows, "  "+stDim.Render(strings.Join(status, " · ")))
	for _, e := range m.modelErrs {
		rows = append(rows, "  "+stErr.Render(e))
	}
	return strings.Join(rows, "\n")
}

// modelHint is the picker's status line.
func modelHint() string {
	return "model  ·  ↑/↓ choose · enter use · esc cancel · alt+enter this session only · ctrl+a log in"
}

// ── the add-provider form ──

// openProviderForm opens the login form, or with edit the form for a provider
// already logged in: its name fixed, its URL filled in, the key left empty to
// keep it.
func (m *tuiModel) openProviderForm(edit *providerConfig) tea.Cmd {
	mk := func(placeholder string) textinput.Model {
		t := textinput.New()
		t.Placeholder = placeholder
		t.SetWidth(56)
		return t
	}
	m.providerFields = [3]textinput.Model{
		mk("what to call it, e.g. work"),
		mk("e.g. https://openrouter.ai/api/v1"),
		mk("if you were given one"),
	}
	m.providerFields[2].EchoMode = textinput.EchoPassword
	m.providerFields[2].EchoCharacter = '•'
	m.providerStep, m.providerErr, m.providerEdit = 0, "", ""
	if edit != nil {
		m.providerEdit = edit.Name
		m.providerFields[0].SetValue(edit.Name)
		m.providerFields[1].SetValue(edit.URL)
		m.providerFields[2].Placeholder = "a new key (empty keeps the saved one)"
		m.providerStep = 1
	}
	m.providerFields[m.providerStep].Focus()
	m.providerForm, m.modelPicking = true, true
	m.input.Blur()
	m.refresh()
	return textinput.Blink
}

func (m tuiModel) updateProviderForm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.providerForm = false
		m.closeModelPicker()
		return m, nil
	case "shift+tab", "up":
		if m.providerStep > 0 && !(m.providerEdit != "" && m.providerStep == 1) {
			m.providerFields[m.providerStep].Blur()
			m.providerStep--
			m.providerFields[m.providerStep].Focus()
		}
		m.refresh()
		return m, textinput.Blink
	case "enter", "tab", "down":
		v := strings.TrimSpace(m.providerFields[m.providerStep].Value())
		switch {
		case m.providerStep == 0 && (v == "" || strings.ContainsAny(v, " \t")):
			m.providerErr = "a name is one word"
		case m.providerStep == 1 && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://"):
			m.providerErr = "the address starts with http:// or https://"
		case m.providerStep < 2:
			m.providerErr = ""
			m.providerFields[m.providerStep].Blur()
			m.providerStep++
			m.providerFields[m.providerStep].Focus()
		case msg.String() == "enter":
			return m, m.saveProvider()
		}
		m.refresh()
		return m, textinput.Blink
	}
	return m.typeProviderField(msg)
}

// typeProviderField is a key or a paste in the provider form's field.
func (m tuiModel) typeProviderField(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.providerFields[m.providerStep], cmd = m.providerFields[m.providerStep].Update(msg)
	m.refresh()
	return m, cmd
}

// saveProvider writes the form's provider to the config and opens the picker
// filtered to it, which is also the check that the URL and key work.
func (m *tuiModel) saveProvider() tea.Cmd {
	op := providerOp{Op: "login",
		Name: strings.TrimSpace(m.providerFields[0].Value()),
		URL:  m.providerFields[1].Value(),
		Key:  m.providerFields[2].Value(),
	}
	if m.providerEdit != "" {
		op.Name = m.providerEdit
	}
	msg, err := applyProviderOp(op)
	if err != nil {
		m.providerErr = err.Error()
		m.refresh()
		return nil
	}
	m.providerForm = false
	m.dropNeedLogin() // no longer true
	m.append(stOK.Render(msg))
	// Filtered to the new service — unless it is the only one, when the
	// filter would only repeat its name.
	query := op.Name + " "
	if len(m.pickerProviders()) <= 1 {
		query = ""
	}
	return m.openModelPicker(query)
}

func (m tuiModel) providerFormPanel() string {
	labels := [3]string{"name", "address", "key"}
	title := "log in to an AI service"
	if m.providerEdit != "" {
		title = "log in to " + m.providerEdit + " again"
	}
	rows := []string{stHeader.Render(title)}
	for i, f := range m.providerFields {
		rows = append(rows, formRow(labels[i], i == m.providerStep, f.View()))
	}
	if m.providerErr != "" {
		rows = append(rows, "  "+stErr.Render(m.providerErr))
	}
	rows = append(rows, "  "+stDim.Render("saved to "+tildePath(configPath())+" · the same name again replaces it"))
	return strings.Join(rows, "\n")
}

// providerHint is the login form's status line.
func (m tuiModel) providerHint() string {
	enter := "enter next"
	if m.providerStep == len(m.providerFields)-1 {
		enter = "enter log in"
	}
	return "log in  ·  " + enter + " · shift+tab back · esc cancel"
}
