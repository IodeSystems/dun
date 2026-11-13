package tui

import (
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/ragd"
	"github.com/iodesystems/dun/internal/textfmt"
	shim "github.com/iodesystems/openai-claude-api-shim"
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
	gen      int // the openModelPicker that asked; an older one's reply is dropped
}

// pickerProviders returns the providers /model lists: the saved ones, plus
// the running one when it came from a flag or env var and is not saved.
func (m tuiModel) pickerProviders() []dunconfig.ProviderConfig {
	cfg := dunconfig.LoadConfig()
	ps := append([]dunconfig.ProviderConfig(nil), cfg.ProviderList()...)
	if _, ok := cfg.ProviderAt(m.url); !ok && m.url != "" {
		ps = append(ps, dunconfig.ProviderConfig{Name: dunconfig.HostName(m.url), URL: m.url, Key: m.opts.Key})
	}
	return ps
}

func (m tuiModel) providerNamed(name string) (dunconfig.ProviderConfig, bool) {
	for _, p := range m.pickerProviders() {
		if p.Name == name {
			return p, true
		}
	}
	return dunconfig.ProviderConfig{}, false
}

// modelSlash implements /model — picking among the logged-in providers'
// models (/login and /logout manage the providers), and the model of each
// role (dunconfig.FacetNames):
//
//	/model                      the roles window: every role and where it came
//	                            from; enter picks its model, x resets it
//	                            (removes facets.<role>), p probes it
//	/model <name>               switch to that model on this provider
//	/model <provider> <name>    switch to that model on that provider
//	/model use <provider> <id>  the same, by id — for a provider that does
//	                            not list its models
//	/model use <provider>/<id>  the same
//	/model use <facet> <provider>/<id>   set one role's model (facets.<facet>);
//	                            main is the switch above
//	/model probe [<facet>]      the capability probes for a role
//
// Every form saves the pick as the default, as enter in the picker does;
// --session (anywhere in the arguments) is for this session only, as
// alt+enter in the picker is — the scope flag /mcp and the server
// switches use. Only main has a session-only pick.
func modelSlash(m *tuiModel, args []string) tea.Cmd {
	if m.replaying {
		m.append(StDim.Render("this is a replay — there is no engine to resume with"))
		return nil
	}
	persist := !slices.Contains(args, "--session")
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--session" })
	switch {
	case len(args) == 1 && (args[0] == "caps" || args[0] == "probe"),
		len(args) == 2 && args[0] == "probe" && dunconfig.IsFacet(args[1]):
		// What the model is known to do, and the check: the engine's. With a
		// role, that role's probe (cmd/dun/capscmd.go, internal/ragd/probe.go).
		// A provider named "probe" is reached with /model use.
		return engineSlash("model")(m, args)
	case len(args) == 3 && args[0] == "use" && dunconfig.IsFacet(args[1]):
		return m.useFacet(args[1], args[2], persist)
	case len(args) == 2 && args[0] == "use":
		return m.useFacet(dunconfig.FacetMain, args[1], persist)
	case len(args) >= 1 && args[0] == "use":
		if len(args) != 3 {
			m.append(StErr.Render("usage: /model use [<facet>] <provider>/<model-id> [--session] · /model use <provider> <model-id> · facets: " + strings.Join(dunconfig.FacetNames, ", ")))
			return nil
		}
		p, ok := dunconfig.LoadConfig().Named(args[1])
		if !ok {
			m.append(StErr.Render("not logged in to " + args[1] + " — /login " + args[1]))
			return nil
		}
		return m.switchModel(p, args[2], persist)
	case len(args) == 2:
		p, ok := m.providerNamed(args[0])
		if !ok {
			m.append(StErr.Render("not logged in to " + args[0] + " — /login " + args[0]))
			return nil
		}
		return m.switchModel(p, args[1], persist)
	case len(args) == 1:
		p, ok := m.providerNamed(m.provider)
		if !ok && m.url == "" {
			m.append(warnLine(needLoginLine))
			return nil
		}
		if p.URL == "" {
			p = dunconfig.ProviderConfig{Name: m.provider, URL: m.url, Key: m.opts.Key}
		}
		return m.switchModel(p, args[0], persist)
	}
	m.openRoles(0)
	return textinput.Blink
}

// useFacet is /model use [<facet>] <provider>/<model>. Main switches the
// session (switchModel); another role is saved to facets.<facet> and is
// used by what starts next (a session; dund for the retrieval roles).
func (m *tuiModel) useFacet(facet, pick string, persist bool) tea.Cmd {
	cfg := dunconfig.LoadConfig()
	provider, model := cfg.ParsePick(pick)
	if model == "" || strings.ContainsAny(model, " \t") {
		m.append(StErr.Render("a model id is one word: /model use " + facet + " <provider>/<model-id>"))
		return nil
	}
	if facet == dunconfig.FacetMain {
		if provider == "" {
			m.append(StErr.Render("no saved provider in " + pick + " — /model use <provider>/<model-id>; /provider lists them"))
			return nil
		}
		p, _ := cfg.Named(provider)
		return m.switchModel(p, model, persist)
	}
	if !persist {
		m.append(StErr.Render("only main has a session-only pick: /model use " + facet + " " + pick + " saves it"))
		return nil
	}
	f := cfg.Facets[facet]
	f.Provider, f.Model = provider, model
	cfg.SetFacet(facet, f)
	if err := dunconfig.SaveConfig(cfg); err != nil {
		m.append(StErr.Render("failed to save config: " + err.Error()))
		return nil
	}
	// The retrieval roles' restart notice is in the roles window's footer.
	when := ""
	switch {
	case facet == dunconfig.FacetVision:
		when = " · sessions started from now describe images with it"
	case !slices.Contains(ragd.Facets, facet):
		when = " · a session started from now uses it"
	}
	r := cfg.Resolve(facet, m.opts.Flags, os.Getenv)
	m.appendNotice(convoEntry{collapsed: okNotice(facet + " model: " + roleWhat(r) + " · saved" + when)})
	return nil
}

// loginSlash implements /login [name]: the service picker, then the form (a
// key typed in the composer would be kept in the sent history), then the
// model picker on that provider. A name already logged in opens the form
// with its URL and keeps its key unless a new one is typed; a template's
// name (/login openai) opens its form. Changes go through applyProviderOp,
// as they do from the web.
func loginSlash(m *tuiModel, args []string) tea.Cmd {
	if len(args) == 3 && args[0] == "rename" {
		m.loginRename(args[1], args[2])
		return nil
	}
	if len(args) > 1 {
		m.append(StErr.Render("usage: /login [name] — the URL and key go in the form · /login rename <old> <new>"))
		return nil
	}
	if len(args) == 1 {
		if p, ok := dunconfig.LoadConfig().Named(args[0]); ok {
			return m.openProviderForm(&p)
		}
		if t, ok := templateNamed(args[0]); ok {
			return m.openTemplateForm(t, false)
		}
		cmd := m.openProviderForm(nil)
		m.providerFields[fieldName].SetValue(args[0])
		return cmd
	}
	return m.openLogin("")
}

// logoutSlash implements /logout <name>.
func logoutSlash(m *tuiModel, args []string) tea.Cmd {
	if len(args) != 1 {
		m.append(StErr.Render("usage: /logout <provider>"))
		return nil
	}
	msg, err := dunconfig.ApplyProviderOp(dunconfig.ProviderOp{Op: "logout", Name: args[0]})
	if err != nil {
		m.append(StErr.Render(err.Error()))
		return nil
	}
	m.append(StDim.Render(msg))
	return nil
}

// openModelPicker opens the picker with query typed in, and asks every
// provider for its models — but for those answered already (have: the
// login's own check), whose rows are in it from the start.
func (m *tuiModel) openModelPicker(query string, have ...modelFetchMsg) tea.Cmd {
	m.models = newPickList(query, "type to search")
	m.modelPicking, m.rolesPicking = true, false
	m.modelRows, m.modelErrs = nil, nil
	m.modelGen++
	gen := m.modelGen
	m.input.Blur()
	ps := m.pickerProviders()
	m.modelPending = len(ps)
	// Several services, hundreds of models: the list opens on the one in
	// use (USER 2026-11-04); tab moves to the next, then to all of them.
	m.modelScope = ""
	cur := m.pickCurrent()
	if len(ps) > 1 && slices.ContainsFunc(ps, func(p dunconfig.ProviderConfig) bool { return p.Name == cur.provider }) {
		m.modelScope = cur.provider
	}
	cmds := []tea.Cmd{textinput.Blink}
	for _, p := range ps {
		if i := slices.IndexFunc(have, func(h modelFetchMsg) bool { return h.provider == p.Name }); i >= 0 {
			m.addModelRows(have[i])
			continue
		}
		cmds = append(cmds, func() tea.Msg {
			ids, why := dunconfig.ListModels(p.URL, p.Key)
			if len(ids) == 0 {
				return modelFetchMsg{provider: p.Name, gen: gen, err: why + " — /model use " + p.Name + " <model-id>, or /login " + p.Name + " again"}
			}
			return modelFetchMsg{provider: p.Name, gen: gen, ids: ids}
		})
	}
	m.refresh()
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
	if m.anthropicProvider(msg.provider) {
		for _, alias := range anthropicAliases {
			m.modelRows = append(m.modelRows, modelRow{msg.provider, alias})
		}
	}
	// Claude's picks first, then chat models, then the rest (modelKind); within
	// each, by name.
	sort.SliceStable(m.modelRows, func(a, b int) bool {
		ra, rb := m.modelRows[a], m.modelRows[b]
		if ra.provider != rb.provider {
			return order[ra.provider] < order[rb.provider]
		}
		if pa, pb := claudeRank(ra.id), claudeRank(rb.id); pa != pb {
			return pa < pb
		}
		if ka, kb := modelKind(ra.id) != "", modelKind(rb.id) != ""; ka != kb {
			return !ka
		}
		return ra.id < rb.id
	})
	// Before the user has moved, the cursor rests on the running model, else
	// the saved default, else the first chat model (row 0 after the sort).
	if !had {
		cur = m.pickCurrent()
		if m.model == "" && !m.rolePick() {
			c := dunconfig.LoadConfig()
			cur = modelRow{c.Provider, c.Model}
		}
	}
	rows := m.filteredModels()
	if i := slices.Index(rows, cur); i >= 0 {
		m.models.sel = i
		return
	}
	if !had {
		m.models.sel = suggestedModel(rows)
	}
}

// suggestedModel is the row the picker starts on when nothing is running
// or saved: the first chat model whose name says it is the default, else
// the standard one, else a chat one; else the first row. A list of ids says
// nothing else (run 5: it started on chat-fast, the first by name).
func suggestedModel(rows []modelRow) int {
	if i := slices.IndexFunc(rows, func(r modelRow) bool { return r.id == claudeDefault }); i >= 0 {
		return i
	}
	for _, word := range []string{"default", "standard", "chat"} {
		for i, r := range rows {
			if modelKind(r.id) == "" && strings.Contains(strings.ToLower(r.id), word) {
				return i
			}
		}
	}
	return 0
}

// modelKind is "" for a model that chats, else what it is for. A model
// list says nothing about kinds (/v1/models is ids), so this is a guess
// from the name, and a conservative one: only a name that says it is not
// for chat is listed after the chat models. A wrong guess moves a row
// down the list; it never hides one.
func modelKind(id string) string {
	l := strings.ToLower(id)
	for _, k := range []struct{ word, kind string }{
		{"embed", "embeddings"}, {"rerank", "reranking"}, {"vision", "images"},
		{"whisper", "speech"}, {"tts", "speech"}, {"transcribe", "speech"},
		{"moderation", "moderation"}, {"dall-e", "images"},
	} {
		if strings.Contains(l, k.word) {
			return k.kind
		}
	}
	return ""
}

// filteredModels returns the rows of the provider in scope matching every
// word of the query. A word that names another provider widens the list to
// every provider, so typing "corp" finds corp's models from anywhere.
func (m tuiModel) filteredModels() []modelRow {
	words := m.models.words()
	scope := m.pickScope()
	if len(words) == 0 && scope == "" {
		return m.modelRows
	}
	var out []modelRow
	for _, r := range m.modelRows {
		if (scope == "" || r.provider == scope) && matchWords(words, r.provider+" "+r.id) {
			out = append(out, r)
		}
	}
	return out
}

// pickScope is the provider the list is limited to: modelScope, unless a
// word of the query names another provider.
func (m tuiModel) pickScope() string {
	if m.modelScope == "" {
		return ""
	}
	for _, w := range m.models.words() {
		for _, p := range m.pickerProviders() {
			if p.Name != m.modelScope && strings.HasPrefix(strings.ToLower(p.Name), w) {
				return ""
			}
		}
	}
	return m.modelScope
}

// nextScope moves the picker to the next provider, after the last one to
// every provider, and from there to the first; back goes the other way.
func (m *tuiModel) nextScope(back bool) {
	scopes := []string{""}
	for _, p := range m.pickerProviders() {
		scopes = append(scopes, p.Name)
	}
	i := max(0, slices.Index(scopes, m.modelScope))
	if back {
		i = (i + len(scopes) - 1) % len(scopes)
	} else {
		i = (i + 1) % len(scopes)
	}
	m.modelScope = scopes[i]
	m.models.sel = 0
	if i := slices.Index(m.filteredModels(), m.pickCurrent()); i >= 0 {
		m.models.sel = i
	}
}

// rolePick says the picker picks for a role other than main, which is
// saved to facets.<role> and not switched to.
func (m tuiModel) rolePick() bool {
	return m.pickFacet != "" && m.pickFacet != dunconfig.FacetMain
}

// pickCurrent is the model the picker marks "in use" and starts on: the
// running one for main, else what the role resolves to.
func (m tuiModel) pickCurrent() modelRow {
	if !m.rolePick() {
		return modelRow{m.provider, m.model}
	}
	r := dunconfig.LoadConfig().Resolve(m.pickFacet, m.opts.Flags, os.Getenv)
	if r.Model == "" {
		return modelRow{}
	}
	if r.Provider == "" {
		r.Provider = m.provider
	}
	return modelRow{r.Provider, r.Model}
}

func (m tuiModel) selectedModel() (modelRow, bool) {
	rows := m.filteredModels()
	if m.models.sel < 0 || m.models.sel >= len(rows) {
		return modelRow{}, false
	}
	return rows[m.models.sel], true
}

func (m *tuiModel) closeModelPicker() {
	m.modelPicking, m.rolesPicking, m.pickFacet = false, false, ""
	m.models.query.Blur()
	m.input.Focus()
	m.refresh()
}

// updateModelPicking owns the keys while the model list is open. Printable
// keys go to the search box, so moving is on the arrows and ctrl+p/ctrl+n.
func (m tuiModel) updateModelPicking(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.rolesPicking {
		return m.updateRoles(msg)
	}
	if m.loginPicking {
		return m.updateLoginPicker(msg)
	}
	if m.providerForm {
		return m.updateProviderForm(msg)
	}
	if m.models.move(msg.String(), len(m.filteredModels()), pickWindow(m.h)) {
		m.refresh()
		return m, nil
	}
	switch msg.String() {
	case "esc", "ctrl+c":
		if m.pickFacet != "" { // back to the roles window
			m.openRoles(slices.Index(dunconfig.FacetNames, m.pickFacet))
			return m, nil
		}
		m.closeModelPicker()
		return m, nil
	case "tab", "shift+tab":
		if len(m.pickerProviders()) > 1 {
			m.nextScope(msg.String() == "shift+tab")
			m.refresh()
		}
		return m, nil
	case "ctrl+a":
		return m, m.openLogin("")
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
		if m.rolePick() {
			if msg.String() != "enter" {
				m.append(StErr.Render("only main has a session-only pick: enter saves " + m.pickFacet + "'s model"))
				return m, nil
			}
			facet := m.pickFacet
			cmd := m.useFacet(facet, r.provider+"/"+r.id, true)
			m.openRoles(slices.Index(dunconfig.FacetNames, facet))
			return m, cmd
		}
		m.closeModelPicker()
		return m, m.switchModel(p, r.id, msg.String() == "enter")
	}
	return m.typeModelQuery(msg)
}

// typeModelQuery is a key or a paste in the model search box.
func (m tuiModel) typeModelQuery(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd, changed := m.models.typeKey(msg)
	if changed {
		m.models.sel = 0
	}
	m.refresh()
	return m, cmd
}

// switchModel restarts the engine on provider p with model. If persist is
// true, p and model become the saved defaults.
func (m *tuiModel) switchModel(p dunconfig.ProviderConfig, model string, persist bool) tea.Cmd {
	if model == "" {
		return nil
	}
	if persist {
		cfg := dunconfig.LoadConfig()
		cfg.Use(p, model)
		if err := dunconfig.SaveConfig(cfg); err != nil {
			m.append(StErr.Render("failed to save config: " + err.Error()))
			return nil
		}
		// Saved: main now comes from the config, not the flag it was started with.
		if f, ok := m.opts.Flags[dunconfig.FacetMain]; ok {
			m.opts.Flags = maps.Clone(m.opts.Flags)
			f.Model, f.URL = "", ""
			m.opts.Flags[dunconfig.FacetMain] = f
		}
	}
	// The words of a status notice (okNotice): what happened · its detail.
	note := "using " + model + " on " + p.Name + " · this session only"
	if persist {
		note = "using " + model + " on " + p.Name + " · saved as default"
	}
	if model == m.model && p.URL == m.url { // nothing to restart
		if persist {
			m.appendNotice(convoEntry{collapsed: okNotice(note)})
		} else {
			m.append(StDim.Render("already using model " + model))
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
	if m.liveSplashAt() >= 0 || (len(m.convo) > 0 && m.convo[0].splash) {
		m.splashWant, m.splashKeep = true, true // said again, with the new model
	}
	m.convo = nil
	m.cur, m.pendingTool = "", -1
	m.sel = -1
	m.busy, m.starting = false, true
	m.dropAsk()
	m.dropHold() // the conversation is about to be replaced
	m.startingStart = time.Now()
	m.fatalErr, m.exitAnnounced = "", false
	m.restarts, m.restartStart = 0, time.Now()
	m.model, m.url, m.provider, m.keySet = model, p.URL, p.Name, p.Key != ""
	m.noLogin = p.URL == ""
	m.skipHistory = false
	m.refresh()

	m.opts.Model, m.opts.Url, m.opts.Key = model, p.URL, p.Key
	if !m.proc.restartWith(m.opts) {
		m.append(StErr.Render("no session service right now — /reconnect, then switch again"))
		m.refresh()
	}
	return nil
}

// modelPanel renders the model picker in the lower pane.
func (m tuiModel) modelPanel() string {
	if m.rolesPicking {
		return m.rolesPanel()
	}
	if m.loginPicking {
		return m.loginPanel()
	}
	if m.providerForm {
		return m.providerFormPanel()
	}
	// The keys are on the status line (modelHint), like every other list's.
	// One indent (listIndent) for the search line, the rows and the count.
	head := stHeader.Render("pick a model")
	if m.rolePick() {
		head = stHeader.Render("pick a model for " + m.pickFacet)
	}
	cur := m.pickCurrent()
	if n := len(m.pickerProviders()); n > 1 {
		on := "every service"
		if s := m.pickScope(); s != "" {
			on = s
		}
		head += StDim.Render(fmt.Sprintf("  on %s · tab: %s", on, textfmt.Plural(n, "service")))
	}
	rows := []string{head, listIndent + m.models.query.View()}

	shown := m.filteredModels()
	// One service in the list: its name on every row says nothing.
	several := slices.ContainsFunc(shown, func(r modelRow) bool { return r.provider != shown[0].provider })
	window := pickWindow(m.h)
	lo, hi := m.models.span(len(shown), window)
	var list []string
	width, idW := 0, 0
	for _, r := range shown[lo:hi] {
		width = max(width, len(r.provider))
		idW = max(idW, len(r.id))
	}
	for i := lo; i < hi; i++ {
		r := shown[i]
		pre := ""
		if several {
			pre = StDim.Render(fmt.Sprintf("%-*s  ", width, r.provider))
		}
		var post []string
		if k := modelKind(r.id); k != "" {
			post = append(post, k)
		}
		if r == cur {
			post = append(post, "in use")
		}
		if n := claudeNote(r.id); n != "" {
			post = append(post, n)
		}
		tail := ""
		if len(post) > 0 {
			tail = strings.Repeat(" ", max(0, idW-len(r.id))) + "  " + StDim.Render(strings.Join(post, " · "))
		}
		name := r.id
		if modelKind(r.id) != "" && i != m.models.sel {
			name = StDim.Render(r.id)
		}
		list = append(list, listRow(pre, name, tail, i == m.models.sel))
	}
	rows = append(rows, padPick(list, len(m.modelRows), window)...)

	// A count only when the list does not all fit: "1-7 of 7" says nothing.
	var status []string
	switch {
	case len(shown) == 0 && m.modelPending == 0 && len(m.modelRows) > 0:
		status = append(status, "no model matches")
	case lo > 0 || hi < len(shown):
		status = append(status, fmt.Sprintf("%d-%d of %d", lo+1, hi, len(shown)))
	}
	if len(m.pickerProviders()) == 0 {
		status = append(status, "not logged in — ctrl+a to log in")
	}
	if m.modelPending > 0 {
		status = append(status, fmt.Sprintf("asking %s…", textfmt.Plural(m.modelPending, "service")))
	}
	// Always a row, said or blank, so a count that comes and goes as the
	// query narrows the list does not move the rows.
	rows = append(rows, listIndent+StDim.Render(strings.Join(status, " · ")))
	// A service's failure is said whole, wrapped, never cut at the edge.
	for _, e := range m.modelErrs {
		rows = append(rows, wholeRows(e, m.w, listIndent, StErr)...)
	}
	return strings.Join(rows, "\n")
}

// modelHint is the picker's status line.
func (m tuiModel) modelHint() string {
	if m.rolePick() {
		return "model for " + m.pickFacet + "  ·  enter save · esc back to roles · tab next service · ctrl+a log in"
	}
	if m.pickFacet != "" {
		return "model  ·  enter use · esc back to roles · alt+enter this session only · tab next service · ctrl+a log in"
	}
	return "model  ·  enter use · esc cancel · alt+enter this session only · tab next service · ctrl+a log in"
}

// ── the add-provider form ──

// The login form's fields. It asks for two: the address, then the key. The
// name is not asked (George, run 5: a step nobody needs on a first run): it
// is filled in from the address as it is typed (nameFromURL) or from the
// template, numbered when another address has it (freeName), shown in the
// form's note, and changed later with /login rename <old> <new>. /login
// <name> names it up front; a service logged in to again keeps its name.
// From a template the address is filled in and the key is asked first; a
// local server's form has no key row.
const (
	fieldAddr = iota
	fieldName
	fieldKey
)

// openProviderForm opens the login form, or with edit the form for a provider
// already logged in: its name fixed, its URL filled in, the key left empty to
// keep it.
func (m *tuiModel) openProviderForm(edit *dunconfig.ProviderConfig) tea.Cmd {
	mk := func(text string) textinput.Model {
		t := textinput.New()
		t.Placeholder = placeholder(text)
		t.SetWidth(formFieldW(m.w))
		return t
	}
	m.providerFields = [3]textinput.Model{
		fieldAddr: mk("e.g. https://openrouter.ai/api/v1"),
		fieldName: mk("filled in from the address"),
		fieldKey:  mk("if you were given one"),
	}
	m.providerFields[fieldKey].EchoMode = textinput.EchoPassword
	m.providerFields[fieldKey].EchoCharacter = '•'
	m.providerStep, m.providerErr, m.providerEdit, m.providerAuto = fieldAddr, "", "", ""
	m.providerChecking, m.providerTmpl, m.providerEnvKey, m.providerBack = "", nil, "", false
	m.providerKeysOpened = false
	// The kept question's line (or the not-logged-in one) said "type /login": done.
	// esc puts it back (loginCancelled).
	m.dropNeedLogin()
	if edit != nil {
		m.providerEdit = edit.Name
		m.providerFields[fieldName].SetValue(edit.Name)
		m.providerFields[fieldAddr].SetValue(edit.URL)
		m.providerFields[fieldKey].Placeholder = placeholder("a new key (empty: keep it)")
	}
	m.providerFields[m.providerStep].Focus()
	m.providerForm, m.modelPicking, m.loginPicking, m.rolesPicking = true, true, false, false
	m.input.Blur()
	m.refresh()
	return textinput.Blink
}

// providerMove moves the form's cursor by d fields; the name is never a
// stop (it is filled in), so it is stepped over.
func (m *tuiModel) providerMove(d int) {
	next := m.providerStep + d
	if next == fieldName {
		next += d
	}
	if next < 0 || next > m.lastStep() {
		return
	}
	m.providerFields[m.providerStep].Blur()
	m.providerStep = next
	m.providerFields[m.providerStep].Focus()
}

func (m tuiModel) updateProviderForm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+o":
		if t := m.providerTmpl; t != nil && t.keysURL != "" {
			m.providerKeysOpened = openKeysPage(t.keysURL)
			m.refresh()
		}
		return m, nil
	case "esc", "ctrl+c":
		// Nothing is saved before the check passes: esc while it runs
		// drops it (loginChecked finds the form gone).
		m.providerForm, m.providerChecking = false, ""
		if m.providerBack && msg.String() == "esc" {
			m.loginPicking = true // the picker as it was left
			m.refresh()
			return m, textinput.Blink
		}
		m.closeModelPicker()
		m.loginCancelled()
		return m, nil
	case "shift+tab", "up":
		m.providerMove(-1)
		m.refresh()
		return m, textinput.Blink
	case "enter", "tab", "down":
		if m.providerChecking != "" {
			return m, nil // the check is running
		}
		v := strings.TrimSpace(m.providerFields[m.providerStep].Value())
		name := strings.TrimSpace(m.providerFields[fieldName].Value())
		switch {
		case m.providerStep == fieldAddr && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://"):
			m.providerErr = "the address starts with http:// or https://"
		case m.providerStep == fieldAddr && (name == "" || strings.ContainsAny(name, " \t")):
			m.providerErr = "the address needs a host: https://host/…"
		case m.providerStep == fieldName && (v == "" || strings.ContainsAny(v, " \t")):
			m.providerErr = "a name is one word"
		case m.providerStep < m.lastStep():
			m.providerErr = ""
			m.providerMove(1)
		case msg.String() == "enter":
			return m, m.saveProvider()
		}
		m.refresh()
		return m, textinput.Blink
	}
	return m.typeProviderField(msg)
}

// typeProviderField is a key or a paste in the provider form's field. Typing
// the address fills in the name as it goes, until the name is edited: the
// template's, or one from the address, numbered when another address has
// it (freeName).
func (m tuiModel) typeProviderField(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	cmd = editField(&m.providerFields[m.providerStep], fmt.Sprintf("provider.%d", m.providerStep), msg)
	if m.providerStep == fieldAddr && m.providerEdit == "" {
		name := &m.providerFields[fieldName]
		if name.Value() == m.providerAuto {
			addr := strings.TrimRight(strings.TrimSpace(m.providerFields[fieldAddr].Value()), "/")
			base := nameFromURL(addr)
			if m.providerTmpl != nil {
				base = m.providerTmpl.id
			}
			m.providerAuto = dunconfig.LoadConfig().FreeName(base, addr)
			name.SetValue(m.providerAuto)
			// SetValue keeps the cursor where the first fill put it (after
			// one character); a person erasing the name starts at its end.
			name.CursorEnd()
		}
	}
	m.refresh()
	return m, cmd
}

// nameFromURL is a name for a service, from its address: the host's main
// label (api.example.com → example, llm.iodesystems.com → iodesystems), and
// "local" for an IP address or localhost. "" while the address has no host.
func nameFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host) != nil {
		return "local"
	}
	labels := strings.Split(strings.Trim(host, "."), ".")
	if len(labels) == 1 {
		return labels[0]
	}
	return labels[len(labels)-2]
}

// saveProvider checks the form's service before anything is saved: it is
// asked for its models with the address and key given. A refused key, or
// nothing answering, is said in the form, which stays with what was typed;
// an answer saves the login and opens the picker (loginChecked).
func (m *tuiModel) saveProvider() tea.Cmd {
	op := dunconfig.ProviderOp{Op: "login",
		Name: m.providerName(),
		URL:  strings.TrimRight(strings.TrimSpace(m.providerFields[fieldAddr].Value()), "/"),
		Key:  strings.TrimSpace(m.providerFields[fieldKey].Value()),
	}
	if op.Key == "" && m.providerEnvKey != "" && m.lastStep() == fieldKey {
		op.Key = strings.TrimSpace(os.Getenv(m.providerEnvKey))
	}
	key := op.Key
	if p, ok := dunconfig.LoadConfig().Named(op.Name); ok && key == "" {
		key = p.Key // again under a saved name: an empty key keeps it
	}
	if m.lastStep() == fieldAddr {
		op.Key, key = "", "" // a local server: no key
	}
	// The form stays while the service is asked for its models; the
	// answer closes it, says the login, and opens the picker already
	// filled. A picker that opened empty and grew when the list came
	// covered the login notice it had just pushed up (run 5, 49×21: "· key
	// set" under the picker).
	m.providerErr, m.providerChecking, m.providerPending = "", op.Name, op
	m.refresh()
	return func() tea.Msg {
		ids, f := dunconfig.AskModels(&http.Client{Timeout: 6 * time.Second}, op.URL, key)
		r := loginCheckedMsg{models: modelFetchMsg{provider: op.Name, ids: ids}}
		if len(ids) == 0 {
			r.models.err = f.Why + " — /model use " + op.Name + " <model-id>, or /login " + op.Name + " again"
			r.fail = f
		}
		return r
	}
}

// loginCheckedMsg is the answer to the login's check (saveProvider): the
// service's model list, or why there is none.
type loginCheckedMsg struct {
	models modelFetchMsg
	fail   dunconfig.ModelsFail
}

// loginChecked takes the check's answer. Refused or unreachable: said in
// the form, nothing saved. Else the login is saved, the form closes, the
// notice is said, and the picker opens with the service's models in it —
// filtered to the new service, unless it is the only one, when the filter
// would only repeat its name.
func (m *tuiModel) loginChecked(msg loginCheckedMsg) tea.Cmd {
	if !m.providerForm || m.providerChecking != msg.models.provider {
		return nil // cancelled while it was asked
	}
	m.providerChecking = ""
	if why := m.loginFail(m.providerPending.URL, msg.fail); why != "" {
		m.providerErr = why
		if msg.fail.Refused && m.providerStep != fieldKey && m.lastStep() == fieldKey {
			m.providerMove(fieldKey - m.providerStep)
		}
		m.refresh()
		return textinput.Blink
	}
	notice, err := dunconfig.ApplyProviderOp(m.providerPending)
	if err != nil {
		m.providerErr = err.Error()
		m.refresh()
		return nil
	}
	if m.providerEnvKey != "" && strings.TrimSpace(m.providerFields[fieldKey].Value()) == "" {
		notice += " from $" + m.providerEnvKey
	}
	m.providerForm = false
	m.dropNeedLogin() // no longer true
	if t := m.providerTmpl; t != nil && t.id == "anthropic" {
		notice += " · " + anthropicBilling
	}
	m.appendNotice(convoEntry{collapsed: okNotice(notice)})
	query := msg.models.provider + " "
	if len(m.pickerProviders()) <= 1 {
		query = ""
	}
	return m.openModelPicker(query, msg.models)
}

func (m tuiModel) providerFormPanel() string {
	labels := [3]string{fieldAddr: "address", fieldName: "name", fieldKey: "key"}
	title := "custom: an OpenAI-compatible service"
	switch {
	case m.providerEdit != "":
		title = "log in to " + m.providerEdit + " again"
	case m.providerTmpl != nil:
		title = "log in to " + m.providerTmpl.label
	}
	rows := []string{stHeader.Render(title)}
	for i := fieldAddr; i <= m.lastStep(); i++ {
		if i != fieldName {
			rows = append(rows, formRow(labels[i], i == m.providerStep, m.providerFields[i].View()))
		}
	}
	switch {
	case m.providerChecking != "":
		rows = append(rows, "  "+StDim.Render("checking "+hostPort(m.providerPending.URL)+"…"))
	case m.providerErr != "":
		// Said whole: the reason (a 401, a refused connection) is the fix.
		rows = append(rows, wholeRows(m.providerErr, m.w, "  ", StErr)...)
	}
	// "privately": saveConfig keeps the file 0600 (only you can read it).
	notes := []string{"saved privately to " + tildePath(dunconfig.ConfigPath())}
	if t := m.providerTmpl; t != nil && m.providerEdit == "" {
		switch {
		case t.local():
			notes = append(notes, t.label+" runs on this computer: no key")
		case m.providerKeysOpened:
			notes = append(notes, "opened "+t.keysAt+" in your browser · ctrl+o opens it again")
		case m.providerEnvKey == "" && t.keysAt != "":
			notes = append(notes, "get a key at "+t.keysAt)
		}
		if t.id == "anthropic" {
			if m.providerEnvKey == "" {
				notes = append(notes, anthropicNoSubscription)
			}
			notes = append(notes, anthropicBilling)
		}
	}
	// Always a row, so the form does not grow as the address is typed.
	if n := m.providerName(); m.providerEdit == "" {
		switch _, ok := dunconfig.LoadConfig().Named(n); {
		case n == "":
			notes = append(notes, "the name comes from the address")
		case ok:
			notes = append(notes, "as "+n+" — replaces the "+n+" saved before")
		default:
			notes = append(notes, "as "+n+" · /login rename changes it")
		}
	}
	rows = formNotes(rows, notes...)
	return strings.Join(rows, "\n")
}

// providerName is the name the login form saves under: the one filled in
// from the address, given with /login <name>, or kept from before.
func (m tuiModel) providerName() string {
	if m.providerEdit != "" {
		return m.providerEdit
	}
	return strings.TrimSpace(m.providerFields[fieldName].Value())
}

// providerHint is the login form's status line.
func (m tuiModel) providerHint() string {
	enter := "enter next"
	if m.providerStep == m.lastStep() {
		enter = "enter log in"
	}
	keys := formKeys("log in", enter)
	if m.providerBack {
		keys = strings.Replace(keys, "esc cancel", "esc back", 1)
	}
	return keys
}

// loginRename is /login rename <old> <new>: the saved service's name, the
// one the splash, the picker and the spend row say.
func (m *tuiModel) loginRename(old, name string) {
	msg, err := dunconfig.ApplyProviderOp(dunconfig.ProviderOp{Op: "rename", Name: old, NewName: name})
	if err != nil {
		m.append(StErr.Render(err.Error()))
		return
	}
	if m.provider == old {
		m.provider = name
	}
	m.appendNotice(convoEntry{collapsed: okNotice(msg)})
}

// loginCancelled puts back the line the login form took away when it
// opened: the kept question's, else the not-logged-in one.
func (m *tuiModel) loginCancelled() {
	if m.loggedIn() {
		return
	}
	if m.heldMsg != "" {
		m.sayHeld()
		return
	}
	m.sayNeedLogin()
}

// claudeDefault is what the picker starts on at Anthropic: Opus 5.5, at the effort
// it defaults to, medium (Carl, 2026-11-01). Change it with the model.
const claudeDefault = "claude-opus-5-5"

// claudePicks lead the list, and anthropicAliases follow them: an alias goes to the
// newest model in that line (agentkit resolves it from the model list).
var (
	claudePicks      = []string{claudeDefault, "claude-sonnet-5-5", "claude-haiku-5-5"}
	anthropicAliases = []string{"opus-latest", "sonnet-latest", "haiku-latest", "fable-latest"}
)

// claudeRank orders the picks and the aliases ahead of everything else.
func claudeRank(id string) int {
	if i := slices.Index(claudePicks, id); i >= 0 {
		return i
	}
	if i := slices.Index(anthropicAliases, id); i >= 0 {
		return len(claudePicks) + i
	}
	return len(claudePicks) + len(anthropicAliases)
}

// claudeNote is what the picker says beside a Claude model: its price, from the
// same list as the estimates.
func claudeNote(id string) string {
	if shim.IsAlias(id) && slices.Contains(anthropicAliases, id) {
		return "the newest " + strings.TrimSuffix(id, "-latest")
	}
	p, ok := shim.PriceOf(dunconfig.LoadPrices(), id)
	if !ok {
		return ""
	}
	note := fmt.Sprintf("~$%g / $%g per million tokens", p.Input, p.Output)
	if id == claudeDefault {
		note = "recommended · " + note
	}
	return note
}

// anthropicProvider reports whether name is Anthropic's own endpoint.
func (m tuiModel) anthropicProvider(name string) bool {
	if p, ok := m.providerNamed(name); ok {
		return strings.Contains(p.URL, "api.anthropic.com")
	}
	return false
}
