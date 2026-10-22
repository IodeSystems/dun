package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

// modelSlash implements /model:
//
//	/model                      the picker
//	/model <name>               switch to that model on this provider
//	/model <provider> <name>    switch to that model on that provider
//	/model add                  add a provider
//	/model edit <provider>      change its URL or key
//	/model remove <provider>    forget a provider (the default moves to the next)
//	/model use <provider> <id>  switch and save as default, by id — for a
//	                            provider that does not list its models
//
// Changes to providers go through applyProviderOp, as they do from the web.
func modelSlash(m *tuiModel, args []string) tea.Cmd {
	if m.replaying {
		m.append(stDim.Render("this is a replay — there is no engine to resume with"))
		return nil
	}
	switch {
	case len(args) == 1 && args[0] == "add":
		return m.openProviderForm(nil)
	case len(args) >= 1 && (args[0] == "remove" || args[0] == "rm"):
		if len(args) != 2 {
			m.append(stErr.Render("usage: /model remove <provider>"))
			return nil
		}
		msg, err := applyProviderOp(providerOp{Op: "remove", Name: args[1]})
		if err != nil {
			m.append(stErr.Render(err.Error()))
			return nil
		}
		m.append(stDim.Render(msg))
		return nil
	case len(args) >= 1 && args[0] == "edit":
		if len(args) != 2 {
			m.append(stErr.Render("usage: /model edit <provider>"))
			return nil
		}
		p, ok := loadConfig().named(args[1])
		if !ok {
			m.append(stErr.Render("no provider named " + args[1]))
			return nil
		}
		return m.openProviderForm(&p)
	case len(args) >= 1 && args[0] == "use":
		if len(args) != 3 {
			m.append(stErr.Render("usage: /model use <provider> <model-id>"))
			return nil
		}
		p, ok := loadConfig().named(args[1])
		if !ok {
			m.append(stErr.Render("no provider named " + args[1] + " — /model add"))
			return nil
		}
		return m.switchModel(p, args[2], true)
	case len(args) == 2:
		p, ok := m.providerNamed(args[0])
		if !ok {
			m.append(stErr.Render("no provider named " + args[0] + " — /model add"))
			return nil
		}
		return m.switchModel(p, args[1], false)
	case len(args) == 1:
		p, _ := m.providerNamed(m.provider)
		if p.URL == "" {
			p = providerConfig{Name: m.provider, URL: m.url, Key: m.opts.key}
		}
		return m.switchModel(p, args[0], false)
	}
	return m.openModelPicker("")
}

// openModelPicker opens the picker with query typed in, and asks every
// provider for its models.
func (m *tuiModel) openModelPicker(query string) tea.Cmd {
	q := textinput.New()
	q.Placeholder = "type to search"
	q.Prompt = "/ "
	q.SetValue(query)
	q.CursorEnd()
	q.Focus()
	m.modelQuery = q
	m.modelPicking, m.modelSel, m.modelPersist = true, 0, false
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
				return modelFetchMsg{provider: p.Name, err: why + " — /model use " + p.Name + " <model-id>, /model edit " + p.Name}
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
func (m tuiModel) updateModelPicking(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
	case "tab":
		m.modelPersist = !m.modelPersist
		m.refresh()
		return m, nil
	case "ctrl+a":
		return m, m.openProviderForm(nil)
	case "enter":
		r, ok := m.selectedModel()
		if !ok {
			return m, nil
		}
		p, found := m.providerNamed(r.provider)
		if !found {
			return m, nil
		}
		m.closeModelPicker()
		return m, m.switchModel(p, r.id, m.modelPersist)
	}
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
	if model == m.model && p.URL == m.url && !persist {
		m.append(stDim.Render("already using model " + model))
		m.refresh()
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

	desc := "this session"
	if persist {
		desc = "persistently"
	}
	m.append(stDim.Render(fmt.Sprintf("switching to %s on %s, %s…", model, p.Name, desc)))

	// The service restarts the engine on the new endpoint, on this
	// conversation; its history comes back and is rendered afresh.
	m.convo = nil
	m.cur, m.pendingTool, m.pendingArgs = "", -1, nil
	m.sel = -1
	m.busy, m.asking, m.starting = false, false, true
	m.startingStart = time.Now()
	m.fatalErr, m.exitAnnounced = "", false
	m.restarts, m.restartStart = 0, time.Now()
	m.model, m.url, m.provider, m.keySet = model, p.URL, p.Name, p.Key != ""
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
	persist := " "
	if m.modelPersist {
		persist = "✓"
	}
	head := stHeader.Render("models") + stDim.Render("  type to search · ↑/↓ choose · enter switch · tab save as default · ctrl+a add provider · esc cancel")
	rows := []string{head, m.modelQuery.View()}

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
		line := mark + stDim.Render(fmt.Sprintf("%-*s  ", width, r.provider)) + r.id
		if i == m.modelSel {
			rows = append(rows, addGutter(line, "▎ ", stSel))
		} else {
			rows = append(rows, addGutter(line, "  ", lipgloss.NewStyle()))
		}
	}

	var status []string
	switch {
	case len(shown) == 0 && m.modelPending == 0 && len(m.modelRows) > 0:
		status = append(status, "no model matches")
	case len(shown) > 0:
		status = append(status, fmt.Sprintf("%d-%d of %d", lo+1, hi, len(shown)))
	}
	if m.modelPending > 0 {
		status = append(status, fmt.Sprintf("fetching from %d provider(s)…", m.modelPending))
	}
	rows = append(rows, "  "+stDim.Render(strings.Join(status, " · ")))
	for _, e := range m.modelErrs {
		rows = append(rows, "  "+stErr.Render(e))
	}
	rows = append(rows, fmt.Sprintf("  [%s] save as default (future sessions)", persist))
	return strings.Join(rows, "\n")
}

// ── the add-provider form ──

// openProviderForm opens the add form, or with edit the edit form for that
// provider: its name fixed, its URL filled in, the key left empty to keep it.
func (m *tuiModel) openProviderForm(edit *providerConfig) tea.Cmd {
	mk := func(placeholder string) textinput.Model {
		t := textinput.New()
		t.Placeholder = placeholder
		t.Width = 56
		return t
	}
	m.providerFields = [3]textinput.Model{
		mk("e.g. openrouter"),
		mk("e.g. https://openrouter.ai/api/v1"),
		mk("API key (empty for none)"),
	}
	m.providerFields[2].EchoMode = textinput.EchoPassword
	m.providerFields[2].EchoCharacter = '•'
	m.providerStep, m.providerErr, m.providerEdit = 0, "", ""
	if edit != nil {
		m.providerEdit = edit.Name
		m.providerFields[0].SetValue(edit.Name)
		m.providerFields[1].SetValue(edit.URL)
		m.providerFields[2].Placeholder = "new API key (empty keeps the saved one)"
		m.providerStep = 1
	}
	m.providerFields[m.providerStep].Focus()
	m.providerForm, m.modelPicking = true, true
	m.input.Blur()
	m.refresh()
	return textinput.Blink
}

func (m tuiModel) updateProviderForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
			m.providerErr = "the URL starts with http:// or https://"
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
	var cmd tea.Cmd
	m.providerFields[m.providerStep], cmd = m.providerFields[m.providerStep].Update(msg)
	m.refresh()
	return m, cmd
}

// saveProvider writes the form's provider to the config and opens the picker
// filtered to it, which is also the check that the URL and key work.
func (m *tuiModel) saveProvider() tea.Cmd {
	op := providerOp{Op: "add",
		Name: strings.TrimSpace(m.providerFields[0].Value()),
		URL:  m.providerFields[1].Value(),
		Key:  m.providerFields[2].Value(),
	}
	if m.providerEdit != "" {
		op.Op, op.Name = "edit", m.providerEdit
	}
	msg, err := applyProviderOp(op)
	if err != nil {
		m.providerErr = err.Error()
		m.refresh()
		return nil
	}
	m.providerForm = false
	m.append(stDim.Render(msg))
	return m.openModelPicker(op.Name + " ")
}

func (m tuiModel) providerFormPanel() string {
	labels := [3]string{"name", "url ", "key "}
	title := "add provider"
	if m.providerEdit != "" {
		title = "edit provider " + m.providerEdit
	}
	rows := []string{stHeader.Render(title) + stDim.Render("  an OpenAI-compatible endpoint · enter next · shift+tab back · esc cancel")}
	for i, f := range m.providerFields {
		label := stDim.Render("  " + labels[i] + "  ")
		if i == m.providerStep {
			label = stSel.Render("▎ " + labels[i] + "  ")
		}
		rows = append(rows, label+f.View())
	}
	if m.providerErr != "" {
		rows = append(rows, "  "+stErr.Render(m.providerErr))
	}
	rows = append(rows, "  "+stDim.Render("saved to "+configPath()+" (0600); a name already saved is replaced"))
	return strings.Join(rows, "\n")
}
