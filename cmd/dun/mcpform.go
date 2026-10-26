package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun"
)

// /mcp add [preset] — a form for a new MCP server. It asks one thing: what
// the person was given — a command (resolved on PATH: notes-mcp), an address
// (a remote server over Streamable HTTP), or a preset's name — and enter
// adds it. The name (the server's id) is filled in from it (mcpNameFrom:
// notes-mcp → notes, https://mcp.linear.app/sse → linear, a preset → its
// id) on the row below, which tab reaches to change it. The optional rows
// follow the name, also by tab: for a URL, a bearer token and, with no
// token, an OAuth login (client id, scopes, issuer, which token to send);
// for a command, environment variables. The engine saves it to
// $DUN_HOME/mcp.json, there in every workspace, and starts it standalone;
// the result comes back as a server event. (Run 5: Lenny's sticky note said
// "it's called notes-mcp"; a name asked first, then "a command or an
// address", stopped him.)
//
// Scripted (hosted in mcpshell) is not asked here: it is a later choice —
// /mcp mode <id> scripted, or m in /mcp — and a first server's user cannot
// answer it.
//
// A preset (dun.Presets()) fills the name and the server row; typing a
// preset's name and leaving the row does the same when the server row is
// empty.
//
// The OAuth rows are the common ones. The rest of the oauth block
// (client_secret, audience, resource, redirect_port, auth_params) is set in
// mcp.json by hand. A command has no working-directory row: ServerSpec has
// no such field.
//
// The token and the env are typed here rather than given as /mcp add
// arguments, and masked: a slash command's arguments are in the input
// history and the scrollback.

// The form's rows. Which apply depends on the server row (mcpRowShown).
const (
	mcpServer = iota
	mcpID
	mcpBearer
	mcpEnv
	mcpClient
	mcpScopes
	mcpIssuer
	mcpSend
	mcpRows // how many
)

func (m *tuiModel) openMCPForm(preset ...string) tea.Cmd {
	mk := func(text string) textinput.Model {
		t := textinput.New()
		t.Placeholder = placeholder(text)
		t.SetWidth(formFieldW(m.w))
		return t
	}
	// Each placeholder fits its field (formFieldW): the label says what the
	// field is, the placeholder only what to put in it.
	given := mcpGivenHint
	if len(placeholder(given)) > formFieldW(m.w) {
		given = "what you were given"
	}
	m.mcpFields = [mcpRows]textinput.Model{
		mk(given),
		mk("filled in from what you were given"),
		mk("if you were given one"),
		mk("NAME=value, if it needs any"),
		mk("oauth client id (empty: none)"),
		mk("scopes (empty: what it asks for)"),
		mk("issuer (empty: the server's)"),
		mk("access_token or id_token"),
	}
	for _, i := range []int{mcpBearer, mcpEnv} {
		m.mcpFields[i].EchoMode = textinput.EchoPassword
		m.mcpFields[i].EchoCharacter = '•'
	}
	m.mcpStep, m.mcpErr, m.mcpForm, m.mcpAuto = mcpServer, "", true, ""
	if len(preset) > 0 && preset[0] != "" {
		if !m.fillPreset(preset[0]) {
			m.mcpErr = "no preset " + strconv.Quote(preset[0]) + " — presets: " + presetIDs()
		}
	}
	m.mcpFields[m.mcpStep].Focus()
	m.input.Blur()
	m.refresh()
	return textinput.Blink
}

// presetIDs lists the presets for a hint.
func presetIDs() string {
	ids := make([]string, len(dun.Presets()))
	for i, p := range dun.Presets() {
		ids[i] = p.ID
	}
	return strings.Join(ids, ", ")
}

// fillPreset fills the id and server rows from a preset.
func (m *tuiModel) fillPreset(id string) bool {
	p, ok := dun.Preset(id)
	if !ok {
		return false
	}
	target := p.URL
	if target == "" {
		target = shellJoin(append([]string{p.Command}, p.Args...))
	}
	m.mcpFields[mcpServer].SetValue(target)
	m.mcpFields[mcpServer].CursorEnd()
	m.mcpFields[mcpID].SetValue(p.ID)
	m.mcpAuto = p.ID
	return true
}

// mcpGivenHint is the server row's plain hint: what to put in it.
const mcpGivenHint = "what you were given — a command, an address or a preset"

// mcpLookPath resolves a command on PATH; a test replaces it.
var mcpLookPath = exec.LookPath

// mcpNameFrom is a server's name from what the person was given, or "":
// an address's host's main label (nameFromURL), a command's base name — the
// package's, for a runner such as npx — without "mcp" and "server" words.
//
//	notes-mcp                         → notes
//	npx -y @modelcontextprotocol/server-filesystem /tmp → filesystem
//	https://mcp.linear.app/sse        → linear
func mcpNameFrom(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		return mcpIDClean(nameFromURL(target))
	}
	words, err := splitShell(target)
	if err != nil || len(words) == 0 {
		return ""
	}
	word := words[0]
	if mcpRunners[filepath.Base(word)] {
		word = ""
		for _, a := range words[1:] {
			if !strings.HasPrefix(a, "-") {
				word = a
				break
			}
		}
	}
	base := filepath.Base(word)
	if i := strings.LastIndex(base, "@"); i > 0 {
		base = base[:i] // a version: @playwright/mcp@latest
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))
	name := strings.ToLower(base)
	for changed := true; changed; {
		changed = false
		for _, w := range []string{"mcp", "server"} {
			for _, sep := range []string{"-", "_"} {
				if s, ok := strings.CutPrefix(name, w+sep); ok {
					name, changed = s, true
				}
				if s, ok := strings.CutSuffix(name, sep+w); ok {
					name, changed = s, true
				}
			}
		}
	}
	if n := mcpIDClean(name); n != "" && n != "mcp" {
		return n
	}
	// @playwright/mcp: the package is "mcp"; its scope is the name.
	if scope, _, ok := strings.Cut(strings.TrimPrefix(word, "@"), "/"); ok && strings.HasPrefix(word, "@") {
		return mcpIDClean(scope)
	}
	return mcpIDClean(name)
}

// mcpRunners are commands that run a package named by their first argument.
var mcpRunners = map[string]bool{"npx": true, "bunx": true, "pnpx": true, "uvx": true, "pipx": true,
	"node": true, "python": true, "python3": true, "deno": true, "bun": true, "uv": true}

// mcpIDClean is s as a server id (mcpIDOK): lowercase letters, digits, - and
// _, no leading - or _, at most 32; "" when nothing is left.
func mcpIDClean(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 32 {
		out = strings.TrimRight(out[:32], "-_")
	}
	return out
}

// mcpResolve reads the server row when the cursor leaves it: a preset's
// name becomes its command or address, and the name row, while it is the
// one filled in, follows.
// A preset's name that is also a command here is not guessed at: it is said.
func (m *tuiModel) mcpResolve() string {
	v := strings.TrimSpace(m.mcpFields[mcpServer].Value())
	if _, ok := dun.Preset(v); ok {
		if _, err := mcpLookPath(v); err == nil {
			return v + " is a preset and a command here — /mcp add " + v + " for the preset, or the command's full path"
		}
		m.fillPreset(v)
		return ""
	}
	m.mcpFollowName()
	return ""
}

// mcpFollowName fills the name row from the server row, unless the name
// was changed by hand.
func (m *tuiModel) mcpFollowName() {
	name := &m.mcpFields[mcpID]
	if name.Value() != m.mcpAuto {
		return
	}
	m.mcpAuto = mcpNameFrom(m.mcpFields[mcpServer].Value())
	for _, p := range dun.Presets() {
		target := p.URL
		if target == "" {
			target = shellJoin(append([]string{p.Command}, p.Args...))
		}
		if strings.TrimSpace(m.mcpFields[mcpServer].Value()) == target {
			m.mcpAuto = p.ID // a preset's own command or address: its name
		}
	}
	name.SetValue(m.mcpAuto)
	name.CursorEnd()
}

func (m *tuiModel) closeMCPForm() {
	m.mcpForm = false
	m.input.Focus()
	m.refresh()
}

// mcpIsURL reports whether the server row is a URL; mcpIsCommand whether it
// is a command line.
func (m *tuiModel) mcpIsURL() bool {
	t := strings.TrimSpace(m.mcpFields[mcpServer].Value())
	return strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://")
}

func (m *tuiModel) mcpIsCommand() bool {
	return !m.mcpIsURL() && strings.TrimSpace(m.mcpFields[mcpServer].Value()) != ""
}

// mcpOAuthRows reports whether the oauth rows apply: a URL with no bearer.
func (m *tuiModel) mcpOAuthRows() bool {
	return m.mcpIsURL() && strings.TrimSpace(m.mcpFields[mcpBearer].Value()) == ""
}

// mcpRowShown reports whether a row applies to what the form holds now.
func (m *tuiModel) mcpRowShown(step int) bool {
	switch step {
	case mcpBearer:
		return m.mcpIsURL()
	case mcpEnv:
		return m.mcpIsCommand()
	case mcpClient:
		return m.mcpOAuthRows()
	case mcpScopes, mcpIssuer, mcpSend:
		return m.mcpOAuthRows() && strings.TrimSpace(m.mcpFields[mcpClient].Value()) != ""
	}
	return true
}

// mcpNext is the next row that applies after step, or step when none does.
func (m *tuiModel) mcpNext(step int) int {
	for next := step + 1; next < mcpRows; next++ {
		if m.mcpRowShown(next) {
			return next
		}
	}
	return step
}

// mcpPrev is the row before step that applies.
func (m *tuiModel) mcpPrev(step int) int {
	for prev := step - 1; prev > 0; prev-- {
		if m.mcpRowShown(prev) {
			return prev
		}
	}
	return 0
}

// mcpRowErr checks the row the cursor is leaving; "" when it is fine.
func (m *tuiModel) mcpRowErr() string {
	v := strings.TrimSpace(m.mcpFields[m.mcpStep].Value())
	switch m.mcpStep {
	case mcpID:
		if !mcpIDOK(v) {
			return "a name is lowercase letters, digits, - and _ — it prefixes the server's tools"
		}
	case mcpServer:
		if v == "" {
			return mcpGivenHint
		}
		if m.mcpIsCommand() {
			words, err := splitShell(v)
			if err != nil {
				return "command: " + err.Error()
			}
			if _, err := mcpLookPath(words[0]); err != nil {
				return words[0] + " isn't a command here — paste the full command or address"
			}
		}
	case mcpEnv:
		if _, err := parseEnv(v); err != nil {
			return "env: " + err.Error()
		}
	case mcpSend:
		if v != "" && v != dun.SendAccessToken && v != dun.SendIDToken {
			return "send is access_token or id_token"
		}
	}
	return ""
}

func (m tuiModel) updateMCPForm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.closeMCPForm()
		return m, nil
	case "shift+tab", "up":
		if m.mcpStep > 0 {
			m.mcpFields[m.mcpStep].Blur()
			m.mcpStep = m.mcpPrev(m.mcpStep)
			m.mcpFields[m.mcpStep].Focus()
		}
		m.refresh()
		return m, textinput.Blink
	case "enter", "tab", "down":
		// Leaving the server row: a preset's name becomes what it runs,
		// and the name follows.
		if m.mcpStep == mcpServer {
			if m.mcpErr = m.mcpResolve(); m.mcpErr != "" {
				m.refresh()
				return m, textinput.Blink
			}
		}
		if m.mcpErr = m.mcpRowErr(); m.mcpErr != "" {
			m.refresh()
			return m, textinput.Blink
		}
		// enter adds — from the server row on; tab goes on to the name and
		// the optional rows.
		if msg.String() == "enter" {
			if m.mcpErr = m.mcpNameErr(); m.mcpErr != "" {
				m.mcpFields[m.mcpStep].Blur()
				m.mcpStep = mcpID // the name filled in is not one: change it there
				m.mcpFields[m.mcpStep].Focus()
				m.refresh()
				return m, textinput.Blink
			}
			if m.addMCPServer() {
				return m, nil
			}
			m.refresh()
			return m, textinput.Blink
		}
		m.mcpFields[m.mcpStep].Blur()
		m.mcpStep = m.mcpNext(m.mcpStep)
		m.mcpFields[m.mcpStep].Focus()
		m.refresh()
		return m, textinput.Blink
	}
	return m.typeMCPField(msg)
}

// addMCPServer sends the form's server to the engine and closes the form;
// false, with mcpErr set, when it cannot.
func (m *tuiModel) addMCPServer() bool {
	spec, err := mcpFormSpec(m.mcpFields[mcpID].Value(), m.mcpFields[mcpServer].Value(), m.mcpFields[mcpBearer].Value())
	if err != nil {
		m.mcpErr = "command: " + err.Error()
		return false
	}
	if spec.Command != "" {
		if spec.Env, err = parseEnv(m.mcpFields[mcpEnv].Value()); err != nil {
			m.mcpErr = "env: " + err.Error()
			return false
		}
	}
	if m.mcpOAuthRows() {
		spec.OAuth = mcpFormOAuth(m.mcpFields[mcpClient].Value(), m.mcpFields[mcpScopes].Value(),
			m.mcpFields[mcpIssuer].Value(), m.mcpFields[mcpSend].Value())
	}
	b, _ := json.Marshal(spec)
	if !m.proc.controlCmd("mcp", "add "+string(b)) {
		m.mcpErr = "no engine right now — /reconnect first"
		return false
	}
	m.closeMCPForm()
	// The result replaces this line (noteMCPAdded): progress, then news,
	// in one place.
	m.appendNotice(convoEntry{collapsed: stDim.Render("adding " + spec.ID + "…"), adding: spec.ID})
	m.refresh()
	return true
}

// typeMCPField is a key or a paste in the add-server form's field.
func (m tuiModel) typeMCPField(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.mcpFields[m.mcpStep], cmd = m.mcpFields[m.mcpStep].Update(msg)
	if m.mcpStep == mcpServer {
		m.mcpErr = "" // said of what was there before
		m.mcpFollowName()
	}
	m.refresh()
	return m, cmd
}

// mcpNameErr checks the name row, wherever the cursor is.
func (m *tuiModel) mcpNameErr() string {
	if !mcpIDOK(strings.TrimSpace(m.mcpFields[mcpID].Value())) {
		return "a name is lowercase letters, digits, - and _ — it prefixes the server's tools"
	}
	return ""
}

func mcpIDOK(id string) bool {
	if id == "" || len(id) > 32 || id[0] == '-' || id[0] == '_' {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// mcpFormSpec turns the form into a server spec: an http(s) address is a
// remote server, anything else a command line split with shell quoting.
func mcpFormSpec(id, target, bearer string) (dun.ServerSpec, error) {
	spec := dun.ServerSpec{ID: strings.TrimSpace(id), Bearer: strings.TrimSpace(bearer)}
	target = strings.TrimSpace(target)
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		spec.URL = target
		return spec, nil
	}
	f, err := splitShell(target)
	if err != nil {
		return spec, err
	}
	if len(f) > 0 {
		spec.Command, spec.Args = f[0], f[1:]
	}
	spec.Bearer = "" // a stdio server takes its credentials in its env or args
	return spec, nil
}

// splitShell is dun.SplitShell.
func splitShell(s string) ([]string, error) { return dun.SplitShell(s) }

// shellJoin is splitShell's inverse, quoting a word only when it needs it.
func shellJoin(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		if w != "" && !strings.ContainsAny(w, " \t\n'\"\\$`") {
			out[i] = w
			continue
		}
		out[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

// parseEnv reads the env row: NAME=value words, shell-quoted.
func parseEnv(s string) ([]string, error) {
	words, err := splitShell(s)
	if err != nil {
		return nil, err
	}
	for _, w := range words {
		k, _, ok := strings.Cut(w, "=")
		if !ok || !envName(k) {
			return nil, fmt.Errorf("%q is not NAME=value", w)
		}
	}
	if len(words) == 0 {
		return nil, nil
	}
	return words, nil
}

func envName(k string) bool {
	if k == "" || k[0] >= '0' && k[0] <= '9' {
		return false
	}
	for _, r := range k {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// mcpFormOAuth is the oauth block from the form's rows; nil without a client
// id. Scopes split on spaces or commas.
func mcpFormOAuth(clientID, scopes, issuer, send string) *dun.OAuthSpec {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return nil
	}
	o := &dun.OAuthSpec{ClientID: clientID, Issuer: strings.TrimSpace(issuer), Send: strings.TrimSpace(send)}
	o.Scopes = strings.FieldsFunc(scopes, func(r rune) bool { return r == ' ' || r == ',' })
	if len(o.Scopes) == 0 {
		o.Scopes = nil
	}
	return o
}

func (m tuiModel) mcpFormPanel() string {
	labels := [mcpRows]string{"server", "name", "token", "env", "client", "scopes", "issuer", "send"}
	rows := []string{stHeader.Render("add MCP server")}
	for i, f := range m.mcpFields {
		if !m.mcpRowShown(i) {
			continue
		}
		if i == mcpClient {
			rows = append(rows, "  "+stDim.Render("oauth login (optional): a client id registered with the identity provider, redirect URI"),
				"  "+stDim.Render("http://127.0.0.1:"+strconv.Itoa(dun.DefaultOAuthRedirectPort)+"/oauth/callback; secret, audience, resource and the rest go in mcp.json"))
		}
		row := formRow(labels[i], i == m.mcpStep, f.View())
		if i == mcpServer {
			switch {
			case m.mcpIsURL():
				row += stDim.Render("  a web address")
			case m.mcpIsCommand():
				row += stDim.Render("  runs as a command")
			}
		}
		rows = append(rows, row)
	}
	if m.mcpErr != "" {
		rows = append(rows, "  "+stErr.Render(m.mcpErr))
	}
	notes := []string{"saved to " + tildePath(dun.UserServersFile()) + " for every project", "starts now"}
	if id := strings.TrimSpace(m.mcpFields[mcpID].Value()); id != "" && slices.Contains(m.serverIDs, id) {
		notes[1] += " · replaces the " + id + " there now"
	}
	rows = formNotes(rows, notes...)
	return strings.Join(rows, "\n")
}

// mcpHint is the form's status line: enter adds from the first row; tab
// goes to the name, then the optional rows when there are any.
func (m tuiModel) mcpHint() string {
	var extra []string
	switch {
	case m.mcpStep == mcpServer:
		extra = append(extra, "tab name")
	case m.mcpNext(m.mcpStep) != m.mcpStep:
		extra = append(extra, "tab more options")
	}
	return formKeys("add server", "enter add", extra...)
}
