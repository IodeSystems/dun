package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun"
)

// /rag setup — a model for each of raglit's roles, from the person's own
// providers (USER 2026-10-24: raglit "as pleasant to install as poly-lsp").
//
// One screen per role (embed, summaries, vision, segment, audio), each the
// type-to-filter list (pickList) over every logged-in provider's models
// with /model's labels (modelKind: embeddings, images, speech), a "keep"
// row for the role's current model and a "none" row; then one screen of
// what changes, and enter saves it. What each role adds, and what it is
// set to, is raglit's own report (`raglit doctor --json`).
//
// Where it is saved: raglit's daemon (its URL from doctor --json) by PUT
// /api/settings/models, which reloads it when no job runs; with no daemon
// answering, the config.json the shared daemon reads when it starts
// ($RAGLIT_ROOT, else ~/.raglit — the daemon dun's raglit serve talks to).
//
// Keys are never copied into raglit's config. A provider with a key gets
// api_key_file: a 0600 file dun writes under $DUN_HOME/keys/, which raglit
// reads at each use. Not api_key_env: the daemon is usually started by
// systemd or by another session, and neither sees this shell's variables.
//
// Undo: /rag setup again, and none for the role.

// ragSetupHelp is setup's line in the palette and in bare /rag.
var ragSetupHelp = []string{"setup", "pick a model for each role (embeddings, summaries, vision, segment, audio) from your providers; none turns one off"}

// ragSetupOrder is the order of the screens: the one that matters most
// first (semantic search), then what reads documents.
var ragSetupOrder = []string{"embed", "identity", "vision", "segment", "audio"}

// ragRoleKind is the modelKind a role's models usually have, listed first;
// "" is a chat model.
var ragRoleKind = map[string]string{"embed": "embeddings", "identity": "", "vision": "images", "segment": "", "audio": "speech"}

// raglitBin finds the raglit the engine runs. A variable for tests.
var raglitBin = func() (string, error) { return dun.FindCommand("raglit") }

// raglitRoot is where the shared daemon keeps its config.json, as raglit
// resolves it (raglit.DefaultRoot).
func raglitRoot() string {
	if r := os.Getenv("RAGLIT_ROOT"); r != "" {
		return r
	}
	if u, err := os.UserHomeDir(); err == nil {
		return filepath.Join(u, ".raglit")
	}
	return ".raglit-root"
}

// raglitDoctor runs `raglit doctor --json` (args after it) and reads it;
// an error for a raglit without it.
func raglitDoctor(timeout time.Duration, args ...string) (dun.RaglitDoctor, []byte, error) {
	bin, err := raglitBin()
	if err != nil {
		return dun.RaglitDoctor{}, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"doctor", "--json"}, args...)...)
	cmd.Env = append(os.Environ(), dun.RaglitTokenEnv()...) // a raglit with keys (dun/raglitauth.go)
	out, _ := cmd.Output()
	d, ok := dun.ParseRaglitDoctor(out)
	if !ok {
		if ctx.Err() != nil {
			return d, out, fmt.Errorf("raglit doctor --json did not answer in %s", timeout)
		}
		return d, out, errors.New("this raglit has no doctor --json (an older build) — update it")
	}
	return d, out, nil
}

// ── what raglit has ──

// ragProvider is one of raglit's providers, as GET /api/settings/models
// shows it: never the key, only whether there is one and where it is read
// from.
type ragProvider struct {
	Name    string `json:"name"`
	Kind    string `json:"kind,omitempty"`
	BaseURL string `json:"base_url"`
	HasKey  bool   `json:"has_key"`
	KeyEnv  string `json:"api_key_env,omitempty"`
	KeyFile string `json:"api_key_file,omitempty"`
}

// ragRole is a role's provider and model; both empty is off.
type ragRole struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ragSettings is raglit's providers and roles.
type ragSettings struct {
	Providers []ragProvider      `json:"providers"`
	Roles     map[string]ragRole `json:"roles"`
}

func (s ragSettings) provider(name string) (ragProvider, bool) {
	for _, p := range s.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return ragProvider{}, false
}

var ragHTTP = &http.Client{Timeout: 10 * time.Second}

// daemonSettings is GET <daemon>/api/settings/models.
func daemonSettings(daemon string) (ragSettings, error) {
	var s ragSettings
	resp, err := ragHTTP.Get(strings.TrimRight(daemon, "/") + "/api/settings/models")
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if err := ragHTTPErr(resp); err != nil {
		return s, err
	}
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

// ragHTTPErr is a daemon's refusal in words: admin-only with auth on, or
// its own detail.
func ragHTTPErr(resp *http.Response) error {
	if resp.StatusCode/100 == 2 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var e struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(b, &e)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("the daemon has auth on and model settings are admin only (HTTP %d) — set them in its web UI at %s", resp.StatusCode, resp.Request.URL.Scheme+"://"+resp.Request.URL.Host)
	case e.Detail != "":
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Detail)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

// ragConfigKeys are the config.json keys of a role.
func ragConfigKeys(role string) (provider, model string) { return role + "_provider", role + "_model" }

// fileSettings reads the providers and roles out of a config.json; a
// missing file is none.
func fileSettings(path string) (ragSettings, error) {
	s := ragSettings{Roles: map[string]ragRole{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return s, fmt.Errorf("%s does not parse: %w", path, err)
	}
	var provs map[string]struct {
		Kind    string `json:"kind"`
		BaseURL string `json:"base_url"`
		Key     string `json:"api_key"`
		KeyEnv  string `json:"api_key_env"`
		KeyFile string `json:"api_key_file"`
	}
	_ = json.Unmarshal(raw["providers"], &provs)
	for n, p := range provs {
		s.Providers = append(s.Providers, ragProvider{Name: n, Kind: p.Kind, BaseURL: p.BaseURL,
			HasKey: p.Key != "" || p.KeyEnv != "" || p.KeyFile != "", KeyEnv: p.KeyEnv, KeyFile: p.KeyFile})
	}
	sort.Slice(s.Providers, func(a, b int) bool { return s.Providers[a].Name < s.Providers[b].Name })
	str := func(k string) string {
		var v string
		_ = json.Unmarshal(raw[k], &v)
		return v
	}
	for _, r := range ragSetupOrder {
		pk, mk := ragConfigKeys(r)
		if m := str(mk); m != "" {
			s.Roles[r] = ragRole{Provider: str(pk), Model: m}
		}
	}
	return s, nil
}

// ── the change ──

// ragPick is what a screen chose for a role: keep it, none, or a model of
// one of dun's providers.
type ragPick struct {
	keep     bool
	provider string // dun's provider name
	model    string
}

// ragTarget is where a change is saved: the daemon, else a config.json.
type ragTarget struct {
	daemon string // URL; "" when none answered
	config string // the config.json written without a daemon
}

var ragNameRe = regexp.MustCompile(`[^a-z0-9-]+`)

// ragProviderName is a dun provider's name as raglit takes one: lowercase
// letters, digits and dashes, starting with a letter or digit, ≤ 40.
func ragProviderName(name string) string {
	n := strings.Trim(ragNameRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(n) > 40 {
		n = strings.TrimRight(n[:40], "-")
	}
	if n == "" {
		n = "provider"
	}
	return n
}

func sameBase(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }

// keyFile writes key to $DUN_HOME/keys/<name>.key, 0600 in a 0700
// directory, and returns its path. raglit reads it at each use, so a key
// changed by /login again reaches raglit at the next setup.
func keyFile(name, key string) (string, error) {
	dir := filepath.Join(dunHome(), "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name+".key")
	if b, err := os.ReadFile(p); err == nil && string(b) == key {
		return p, os.Chmod(p, 0o600)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(key), 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// ragPlan is the change: raglit's providers after it (cur's, and one per
// dun provider a pick uses), and every role.
type ragPlan struct {
	providers []ragProvider
	roles     map[string]ragRole
	added     []string // raglit providers added or given a key file, "name (key in path)"
}

// planRagSetup turns picks into raglit's providers and roles. A dun
// provider whose base URL raglit already has reuses that provider (and
// gets a key file only when it has no key); another gets a new one, named
// after it. Key files are written here.
func planRagSetup(cur ragSettings, picks map[string]ragPick, dunProvs []providerConfig) (ragPlan, error) {
	pl := ragPlan{providers: slices.Clone(cur.Providers), roles: map[string]ragRole{}}
	for r, v := range cur.Roles {
		pl.roles[r] = v
	}
	named := map[string]string{} // dun provider → raglit provider
	for _, role := range ragSetupOrder {
		pk, ok := picks[role]
		if !ok || pk.keep {
			continue
		}
		if pk.model == "" {
			delete(pl.roles, role)
			continue
		}
		name, done := named[pk.provider]
		if !done {
			dp, found := findProvider(dunProvs, pk.provider)
			if !found {
				return pl, fmt.Errorf("%s: dun has no provider %q", dun.RagRoleName(role), pk.provider)
			}
			var err error
			if name, err = pl.provide(dp); err != nil {
				return pl, err
			}
			named[pk.provider] = name
		}
		pl.roles[role] = ragRole{Provider: name, Model: pk.model}
	}
	return pl, nil
}

func findProvider(ps []providerConfig, name string) (providerConfig, bool) {
	for _, p := range ps {
		if p.Name == name {
			return p, true
		}
	}
	return providerConfig{}, false
}

// provide is the raglit provider for dun's dp, added when raglit has none
// at its address.
func (pl *ragPlan) provide(dp providerConfig) (string, error) {
	for i, p := range pl.providers {
		if !sameBase(p.BaseURL, dp.URL) {
			continue
		}
		if !p.HasKey && dp.Key != "" {
			f, err := keyFile(p.Name, dp.Key)
			if err != nil {
				return "", err
			}
			pl.providers[i].KeyFile, pl.providers[i].HasKey = f, true
			pl.added = append(pl.added, p.Name+" (key read from "+f+")")
		}
		return p.Name, nil
	}
	base := ragProviderName(dp.Name)
	name := base
	taken := func(n string) bool {
		return slices.ContainsFunc(pl.providers, func(p ragProvider) bool { return p.Name == n })
	}
	for i := 2; taken(name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	np := ragProvider{Name: name, Kind: "custom", BaseURL: strings.TrimRight(dp.URL, "/")}
	what := name + " (no key)"
	if dp.Key != "" {
		f, err := keyFile(name, dp.Key)
		if err != nil {
			return "", err
		}
		np.KeyFile, np.HasKey = f, true
		what = name + " (key read from " + f + ")"
	}
	pl.providers = append(pl.providers, np)
	pl.added = append(pl.added, what)
	return name, nil
}

// apply saves pl to t: PUT to the daemon, else into the config.json.
func (pl ragPlan) apply(t ragTarget) error {
	if t.daemon != "" {
		type provIn struct {
			Name    string `json:"name"`
			Kind    string `json:"kind,omitempty"`
			BaseURL string `json:"base_url"`
			KeyEnv  string `json:"api_key_env,omitempty"`
			KeyFile string `json:"api_key_file,omitempty"`
		}
		body := struct {
			Providers []provIn           `json:"providers"`
			Roles     map[string]ragRole `json:"roles"`
		}{Providers: []provIn{}, Roles: map[string]ragRole{}}
		// The daemon replaces its provider list with this one; a stored key
		// is kept for a provider listed by name (it is never sent back).
		for _, p := range pl.providers {
			body.Providers = append(body.Providers, provIn{p.Name, p.Kind, p.BaseURL, p.KeyEnv, p.KeyFile})
		}
		// Every role: one left out is turned off.
		for _, r := range ragSetupOrder {
			body.Roles[r] = pl.roles[r]
		}
		b, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPut, strings.TrimRight(t.daemon, "/")+"/api/settings/models", bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := ragHTTP.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return ragHTTPErr(resp)
	}
	return pl.patchConfig(t.config)
}

// patchConfig writes the providers and roles into path, keeping every other
// key (and a provider's inline key) as it is; temp file and rename, 0600.
func (pl ragPlan) patchConfig(path string) error {
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &raw); err != nil {
			return fmt.Errorf("%s does not parse: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	provs := map[string]map[string]any{}
	_ = json.Unmarshal(raw["providers"], &provs)
	for _, p := range pl.providers {
		e := provs[p.Name]
		if e == nil {
			e = map[string]any{}
		}
		e["base_url"] = p.BaseURL
		if p.Kind != "" {
			e["kind"] = p.Kind
		}
		if p.KeyFile != "" {
			e["api_key_file"] = p.KeyFile
		}
		provs[p.Name] = e
	}
	if b, err := json.Marshal(provs); err == nil && len(provs) > 0 {
		raw["providers"] = b
	}
	for _, r := range ragSetupOrder {
		pk, mk := ragConfigKeys(r)
		v, on := pl.roles[r]
		if !on || v.Model == "" {
			delete(raw, pk)
			delete(raw, mk)
			continue
		}
		raw[pk], _ = json.Marshal(v.Provider)
		raw[mk], _ = json.Marshal(v.Model)
	}
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ragOnNow is what the report after a save says: the capabilities on now
// that were off before, and the ones still off.
func ragOnNow(before, after dun.SetupReport) string {
	was := map[string]bool{}
	for _, c := range before.Capabilities {
		was[c.Name] = c.On
	}
	var on, off []string
	for _, c := range after.Capabilities {
		switch {
		case c.On && !was[c.Name]:
			s := c.Name
			if c.Via != "" {
				s += " (" + c.Via + ")"
			}
			on = append(on, s)
		case !c.On:
			off = append(off, c.Name)
		}
	}
	var parts []string
	if len(on) > 0 {
		parts = append(parts, "on now: "+strings.Join(on, ", "))
	}
	if len(off) > 0 {
		parts = append(parts, "still off: "+strings.Join(off, ", "))
	}
	if len(parts) == 0 {
		return "everything it reports is on"
	}
	return strings.Join(parts, " · ")
}

// ── the screens ──

// ragSetup is the open /rag setup.
type ragSetup struct {
	loading bool // asking raglit; the screens open when it answers
	doc     dun.RaglitDoctor
	cur     ragSettings
	target  ragTarget
	roles   []dun.SetupRole // the screens, in ragSetupOrder
	step    int             // the role on screen; len(roles) is the summary
	picks   map[string]ragPick
	provs   []providerConfig // dun's, as /model lists them
	list    pickList
	rows    []modelRow // every provider's models so far
	errs    []string
	pending int
	saving  bool
	moved   bool // the person moved or typed on this screen
}

// ragLoadedMsg is raglit's report and settings, or why there are none.
type ragLoadedMsg struct {
	doc    dun.RaglitDoctor
	cur    ragSettings
	target ragTarget
	err    error
}

// ragModelsMsg is one provider's models for the setup.
type ragModelsMsg struct{ modelFetchMsg }

// ragSavedMsg is the save's outcome, in words.
type ragSavedMsg struct {
	line string
	err  error
}

// loadRagSetup asks raglit for its report, then for its settings where the
// report says they are.
func loadRagSetup() tea.Msg {
	d, _, err := raglitDoctor(10 * time.Second)
	if err != nil {
		return ragLoadedMsg{err: err}
	}
	msg := ragLoadedMsg{doc: d}
	if d.Daemon != "" {
		msg.target.daemon = d.Daemon
		msg.cur, msg.err = daemonSettings(d.Daemon)
		if msg.err != nil {
			msg.err = fmt.Errorf("raglit's daemon at %s: %w", d.Daemon, msg.err)
		}
		return msg
	}
	msg.target.config = filepath.Join(raglitRoot(), "config.json")
	msg.cur, msg.err = fileSettings(msg.target.config)
	return msg
}

// openRagSetup is /rag setup: the panel opens at once, saying it is
// asking, and every provider is asked for its models while raglit answers.
func (m *tuiModel) openRagSetup() tea.Cmd {
	if _, err := raglitBin(); err != nil {
		m.append(stErr.Render("raglit setup: raglit is not installed — dun doctor says how to get it"))
		return nil
	}
	ps := m.pickerProviders()
	m.rag = &ragSetup{loading: true, picks: map[string]ragPick{}, provs: ps, pending: len(ps), list: newPickList("", "type to filter")}
	m.input.Blur()
	cmds := []tea.Cmd{loadRagSetup, textinput.Blink}
	for _, p := range ps {
		cmds = append(cmds, func() tea.Msg {
			ids, why := listModels(p.URL, p.Key)
			if len(ids) == 0 {
				return ragModelsMsg{modelFetchMsg{provider: p.Name, err: why}}
			}
			return ragModelsMsg{modelFetchMsg{provider: p.Name, ids: ids}}
		})
	}
	m.refresh()
	return tea.Batch(cmds...)
}

func (m *tuiModel) closeRagSetup() {
	m.rag = nil
	m.input.Focus()
	m.refresh()
}

// ragLoaded opens the first screen, or closes with why it cannot.
func (m *tuiModel) ragLoaded(msg ragLoadedMsg) {
	if m.rag == nil {
		return
	}
	if msg.err != nil {
		m.closeRagSetup()
		m.append(stErr.Render("raglit setup: " + oneLine(msg.err.Error())))
		return
	}
	r := m.rag
	r.loading, r.doc, r.cur, r.target = false, msg.doc, msg.cur, msg.target
	if r.cur.Roles == nil {
		r.cur.Roles = map[string]ragRole{}
	}
	for _, name := range ragSetupOrder {
		for _, ro := range msg.doc.Roles {
			if ro.Role == name {
				r.roles = append(r.roles, ro)
			}
		}
	}
	if len(r.roles) == 0 {
		m.closeRagSetup()
		m.append(stErr.Render("raglit setup: raglit reported no model roles"))
		return
	}
	m.ragScreen()
}

// ragChoice is one row of a role's screen.
type ragChoice struct {
	pick  ragPick
	label string // keep and none rows
	row   modelRow
	now   bool // the role's model now
}

// ragChoices are a role's rows: keep (when it has a model), none, then the
// models that match the query, the role's usual kind first.
func (r *ragSetup) ragChoices() []ragChoice {
	role := r.roles[r.step]
	cur := r.cur.Roles[role.Role]
	var out []ragChoice
	if cur.Model != "" {
		out = append(out, ragChoice{pick: ragPick{keep: true}, label: "keep " + strings.TrimSpace(cur.Provider+" "+cur.Model), now: true})
	}
	none := "none — off"
	if role.FallsBackTo != "" {
		none = "none — use " + dun.RagRoleName(role.FallsBackTo) + "'s model"
	}
	out = append(out, ragChoice{pick: ragPick{}, label: none, now: cur.Model == ""})
	want := ragRoleKind[role.Role]
	rows := slices.Clone(r.rows)
	sort.SliceStable(rows, func(a, b int) bool {
		return (modelKind(rows[a].id) == want) && (modelKind(rows[b].id) != want)
	})
	curBase := ""
	if p, ok := r.cur.provider(cur.Provider); ok {
		curBase = p.BaseURL
	}
	words := r.list.words()
	for _, row := range rows {
		if !matchWords(words, row.provider+" "+row.id) {
			continue
		}
		c := ragChoice{pick: ragPick{provider: row.provider, model: row.id}, row: row}
		if dp, ok := r.dunProvider(row.provider); ok && row.id == cur.Model && sameBase(dp.URL, curBase) {
			c.now = true
		}
		out = append(out, c)
	}
	return out
}

func (r *ragSetup) dunProvider(name string) (providerConfig, bool) {
	return findProvider(r.provs, name)
}

// ragScreen puts the cursor where a role's screen starts: on its pick when
// it was made, else on its model now (a matching model row before keep),
// else for embed on the first embedding model, else on none.
func (m *tuiModel) ragScreen() {
	r := m.rag
	r.list, r.moved = newPickList("", "type to filter"), false
	if r.step >= len(r.roles) {
		m.refresh()
		return
	}
	cs := r.ragChoices()
	role := r.roles[r.step].Role
	r.list.sel = 0
	if pk, ok := r.picks[role]; ok {
		if i := slices.IndexFunc(cs, func(c ragChoice) bool { return c.pick == pk }); i >= 0 {
			r.list.sel = i
		}
	} else if i := slices.IndexFunc(cs, func(c ragChoice) bool { return c.now && c.row.id != "" }); i >= 0 {
		r.list.sel = i
	} else if i := slices.IndexFunc(cs, func(c ragChoice) bool { return c.now }); i >= 0 {
		r.list.sel = i
		if role == "embed" && !cs[i].pick.keep {
			if j := slices.IndexFunc(cs, func(c ragChoice) bool { return modelKind(c.row.id) == "embeddings" && c.row.id != "" }); j >= 0 {
				r.list.sel = j
			}
		}
	}
	m.refresh()
}

// updateRagSetup owns the keys while the setup is open.
func (m tuiModel) updateRagSetup(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	r := m.rag
	key := msg.String()
	if key == "esc" || key == "ctrl+c" {
		m.closeRagSetup()
		m.append(stDim.Render("raglit setup: cancelled, nothing changed"))
		return m, nil
	}
	if r.loading || r.saving {
		return m, nil
	}
	if key == "shift+tab" {
		if r.step > 0 {
			r.step--
			m.ragScreen()
		}
		return m, nil
	}
	if r.step >= len(r.roles) { // the summary
		if key != "enter" {
			return m, nil
		}
		if len(r.changes()) == 0 {
			m.closeRagSetup()
			m.append(stDim.Render("raglit setup: nothing changed"))
			return m, nil
		}
		r.saving = true
		m.refresh()
		return m, r.save()
	}
	cs := r.ragChoices()
	if r.list.move(key, len(cs), pickWindow(m.h)) {
		r.moved = true
		m.refresh()
		return m, nil
	}
	if key == "enter" {
		if len(cs) == 0 {
			return m, nil
		}
		r.picks[r.roles[r.step].Role] = cs[min(r.list.sel, len(cs)-1)].pick
		r.step++
		m.ragScreen()
		return m, nil
	}
	return m.typeRagQuery(msg)
}

// typeRagQuery is a key or a paste in a role's search box.
func (m tuiModel) typeRagQuery(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd, changed := m.rag.list.typeKey(msg)
	if changed {
		m.rag.moved = true
		// The first model row: keep and none are always there above it.
		m.rag.list.sel = 0
		if cs := m.rag.ragChoices(); len(m.rag.list.words()) > 0 {
			if i := slices.IndexFunc(cs, func(c ragChoice) bool { return c.row.id != "" }); i >= 0 {
				m.rag.list.sel = i
			}
		}
	}
	m.refresh()
	return m, cmd
}

// changes are the summary's rows: each role whose pick changes it.
func (r *ragSetup) changes() []string {
	var out []string
	for _, ro := range r.roles {
		pk, ok := r.picks[ro.Role]
		if !ok || pk.keep {
			continue
		}
		cur := r.cur.Roles[ro.Role]
		was := "none"
		if cur.Model != "" {
			was = strings.TrimSpace(cur.Provider + " " + cur.Model)
		}
		to := "none"
		if pk.model != "" {
			to = pk.provider + " " + pk.model
		}
		if pk.model == cur.Model && (pk.model == "" || r.sameProvider(pk.provider, cur.Provider)) {
			continue
		}
		out = append(out, fmt.Sprintf("%s: %s → %s", dun.RagRoleName(ro.Role), was, to))
	}
	return out
}

// sameProvider: dun's provider dunName is raglit's ragName (same address).
func (r *ragSetup) sameProvider(dunName, ragName string) bool {
	dp, ok := r.dunProvider(dunName)
	rp, ok2 := r.cur.provider(ragName)
	return ok && ok2 && sameBase(dp.URL, rp.BaseURL)
}

// save applies the picks and asks raglit again what it can do now.
func (r *ragSetup) save() tea.Cmd {
	cur, picks, target, before, provs := r.cur, maps.Clone(r.picks), r.target, r.doc.SetupReport, r.provs
	return func() tea.Msg {
		pl, err := planRagSetup(cur, picks, provs)
		if err != nil {
			return ragSavedMsg{err: err}
		}
		if err := pl.apply(target); err != nil {
			return ragSavedMsg{err: err}
		}
		where := "saved to " + target.config + " (the daemon reads it when it starts)"
		if target.daemon != "" {
			where = "saved to the daemon at " + target.daemon + " (it reloads when no job is running)"
		}
		line := "raglit: " + where
		if len(pl.added) > 0 {
			line += "\nproviders: " + strings.Join(pl.added, ", ") + " — raglit's config holds the file's path, never the key"
		}
		if d, _, err := raglitDoctor(10 * time.Second); err == nil {
			line += "\n" + ragOnNow(before, d.SetupReport)
		}
		return ragSavedMsg{line: line + "\n/rag restart refreshes /rag's table · /rag setup again, with none, to undo"}
	}
}

// ragSaved closes the setup with the save's outcome.
func (m *tuiModel) ragSaved(msg ragSavedMsg) {
	if m.rag == nil {
		return
	}
	m.closeRagSetup()
	if msg.err != nil {
		m.append(stErr.Render("raglit setup: not saved — " + oneLine(msg.err.Error())))
		return
	}
	m.append(stDim.Render(msg.line))
}

// ragModels takes one provider's answer into the setup.
func (m *tuiModel) ragModels(msg ragModelsMsg) {
	r := m.rag
	if r == nil {
		return
	}
	r.pending = max(0, r.pending-1)
	if msg.err != "" {
		r.errs = append(r.errs, msg.provider+": "+msg.err)
	} else {
		for _, id := range msg.ids {
			r.rows = append(r.rows, modelRow{msg.provider, id})
		}
		sort.SliceStable(r.rows, func(a, b int) bool {
			if r.rows[a].provider != r.rows[b].provider {
				return r.rows[a].provider < r.rows[b].provider
			}
			return r.rows[a].id < r.rows[b].id
		})
	}
	// Until the person moves or types, the cursor follows where the screen
	// starts (an embedding model may be in this answer).
	if !r.loading && r.step < len(r.roles) && !r.moved {
		m.ragScreen()
	}
	m.refresh()
}

// ragSetupPanel renders the open setup.
func (m tuiModel) ragSetupPanel() string {
	r := m.rag
	if r.loading {
		return strings.Join([]string{stHeader.Render("raglit setup"), listIndent + stDim.Render("asking raglit what it has…")}, "\n")
	}
	if r.step >= len(r.roles) {
		rows := []string{stHeader.Render("raglit setup · save these?")}
		ch := r.changes()
		if len(ch) == 0 {
			rows = append(rows, listIndent+"nothing changes")
		}
		for _, c := range ch {
			rows = append(rows, listIndent+c)
		}
		// Changing a set embedding model (not setting the first one) makes
		// raglit re-embed what it has.
		if r.cur.Roles["embed"].Model != "" && slices.ContainsFunc(ch, func(s string) bool { return strings.HasPrefix(s, "embed:") }) {
			rows = append(rows, listIndent+stDim.Render("another embedding model re-embeds every index"))
		}
		where := "into " + r.target.config
		if r.target.daemon != "" {
			where = "to the daemon at " + r.target.daemon
		}
		if r.saving {
			rows = append(rows, listIndent+stDim.Render("saving "+where+"…"))
		} else {
			rows = append(rows, listIndent+stDim.Render("enter saves it "+where+"; a key stays in a file of dun's, named by its path"))
		}
		return strings.Join(rows, "\n")
	}
	role := r.roles[r.step]
	head := fmt.Sprintf("raglit setup · %s (%d of %d)", dun.RagRoleName(role.Role), r.step+1, len(r.roles))
	rows := []string{stHeader.Render(head)}
	if role.Adds != "" {
		rows = append(rows, listIndent+stDim.Render(role.Adds))
	}
	rows = append(rows, listIndent+r.list.query.View())
	cs := r.ragChoices()
	window := pickWindow(m.h)
	lo, hi := r.list.span(len(cs), window)
	idW, provW := 0, 0
	for _, c := range cs[lo:hi] {
		idW, provW = max(idW, len(c.row.id)), max(provW, len(c.row.provider))
	}
	var list []string
	for i := lo; i < hi; i++ {
		c := cs[i]
		if c.row.id == "" {
			tail := ""
			if c.now && !c.pick.keep {
				tail = "  " + stDim.Render("now")
			}
			list = append(list, listRow("", c.label, tail, i == r.list.sel))
			continue
		}
		pre := stDim.Render(fmt.Sprintf("%-*s  ", provW, c.row.provider))
		var post []string
		if k := modelKind(c.row.id); k != "" {
			post = append(post, k)
		}
		if c.now {
			post = append(post, "now")
		}
		tail := ""
		if len(post) > 0 {
			tail = strings.Repeat(" ", idW-len(c.row.id)) + "  " + stDim.Render(strings.Join(post, " · "))
		}
		list = append(list, listRow(pre, c.row.id, tail, i == r.list.sel))
	}
	rows = append(rows, padPick(list, len(r.rows)+2, window)...)
	var status []string
	if lo > 0 || hi < len(cs) {
		status = append(status, fmt.Sprintf("%d-%d of %d", lo+1, hi, len(cs)))
	}
	if r.pending > 0 {
		status = append(status, fmt.Sprintf("asking %s…", plural(r.pending, "service")))
	}
	if len(r.provs) == 0 {
		status = append(status, "not logged in — /login, then /rag setup")
	}
	rows = append(rows, listIndent+stDim.Render(strings.Join(status, " · ")))
	for _, e := range r.errs {
		rows = append(rows, wholeRows(e, m.w, listIndent, stErr)...)
	}
	return strings.Join(rows, "\n")
}

// ragSetupHint is the setup's status line.
func (m tuiModel) ragSetupHint() string {
	switch {
	case m.rag.loading:
		return "raglit setup  ·  esc cancel"
	case m.rag.step >= len(m.rag.roles):
		return "raglit setup  ·  enter save · shift+tab back · esc cancel"
	}
	return "raglit setup  ·  enter pick · shift+tab back · esc cancel · type to filter"
}
