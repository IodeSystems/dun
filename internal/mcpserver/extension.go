package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/iodesystems/dun/examples/extensions"
	"github.com/iodesystems/dun/internal/project"
)

// Extensions — an MCP server plus a manifest (extension.json) that says what
// dun may ask of it and what it wants to hear about (plan item 21). There is
// no dun-specific protocol: everything dun does with an extension is an MCP
// tool call, except where a role names a CLI fallback (below).
//
// raglit and poly-lsp-mcp are extensions bundled in the binary; their
// manifests are examples/extensions/*/extension.json, embedded. A user's
// live in $DUN_HOME/extensions/<id>/ and a project's in
// <repo>/.dun/extensions/<id>/; a later source replaces an earlier one of the
// same id (bundled < user < project).
//
// The manifest:
//
//	id           the extension's id, which is also its server's id, its
//	             directory's name and its tools' namespace ("<id>__<tool>")
//	description  one line, for /ext
//	server       the same spec as /mcp add (command or url, args, env, via
//	             standalone|scripted, autostart, disable …), switched on and
//	             off by the same global, project and session scopes
//	slash        {name, description, commands}: /<name> takes the server
//	             switch's actions (on, off, unset, restart, auto, manual), and
//	             /<name> <command> <text> runs commands[command]: {help,
//	             tool+args | cli | steps}, a call of the server's own tool
//	             or a run of roles.index.cli, with {{arg}}, {{paths}},
//	             {{path}}, {{index}}, {{project}} filled in (harness/extcmd.go)
//	roles        lookups dun makes on demand:
//	               search {tool, get, list}: tool takes {"query"} and answers
//	                 hits (raglit's shape); get takes {"path", "max_chars"}
//	                 and answers {"text", "truncated"}, to inline a short
//	                 hit; list takes {} and answers {"indexes": [{"name",
//	                 "meaning"}]}: which indexes search by meaning, so a
//	                 proactive hit is judged by its similarity or by the
//	                 query words it holds (docsPreparer).
//	                 When tool takes an "index", /<slash> attach|detach
//	                 adds other projects' indexes to it (harness/attach.go)
//	               index {tool, forget, cli}: where documents go — see
//	                 indexSink for the MCP/CLI choice. With cli,
//	                 /<slash> watch <dir> has the CLI watch a directory
//	                 into an index
//	hooks        events dun sends:
//	               turn.before      search the conversation, surface a clear
//	                                winner as an aside (roles.search)
//	               workspace.open   index the workspace when the server starts
//	               memory.save      accepted, unused: memory notes are
//	                                dund's since rag slice 5 (memory/recall.go)
//	               skills.changed   the local skills, SKILL.md and references
//	               rules.changed    the rules' imports, in /rules index mode
//	               mcp.docs         MCP servers' resources, on /mcp add|refresh
//	             the index events need roles.index
//	env          {KEY: value} for the server, "$NAME" read from dun's env
//	skills       a directory of skills, relative to the manifest, read like
//	             ~/.dun/skills
//
// Placeholders in server args and env: {{workspace}}, {{project}} (the
// repository's corpus name) and {{index}} (this directory on this branch);
// and in a user's or project's command, args and env, {{dir}}: the
// extension's own directory, for a server it ships.
//
// An extension with no roles or hooks is a packaged MCP server and shows in
// /mcp like any other. Not in v1: shell-command hooks.

// bundledFS is examples/extensions/<id>/extension.json, embedded there.
var bundledFS = extensions.FS

// bundledOrder is the order the bundled extensions are listed in (embed
// lists alphabetically).
var bundledOrder = []string{ServerDocs, ServerCode}

// Extension sources.
const (
	ExtBundled = "bundled"
	ExtUser    = "user"
	ExtProject = "project"
)

// ExtensionFile is the manifest's name in an extension's directory.

const ExtensionFile = "extension.json"

// Extension is one loaded manifest.
type Extension struct {
	ID          string            `json:"id"`
	Description string            `json:"description,omitempty"`
	Server      Spec              `json:"server"`
	Slash       *ExtSlash         `json:"slash,omitempty"`
	Roles       ExtRoles          `json:"roles,omitempty"`
	Hooks       []string          `json:"hooks,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Skills      string            `json:"skills,omitempty"`

	// Source is bundled, user or project; Path the manifest ("" bundled).
	Source string `json:"-"`
	Path   string `json:"-"`
}

// ExtSlash is an extension's slash command.
type ExtSlash struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Help        string                 `json:"help,omitempty"` // the command in full, for /help <name>
	Commands    map[string]ExtSlashCmd `json:"commands,omitempty"`
}

// ExtSlashCmd is one /<slash> <command> (harness/extcmd.go): one step written
// inline, or steps. Help is its line in bare /<slash> and the completion.
type ExtSlashCmd struct {
	Help string `json:"help,omitempty"`
	ExtStep
	Steps []ExtStep `json:"steps,omitempty"`
}

// ExtStep is one thing a command does: a call of the extension's own tool
// with args, or the index role's CLI with argv cli. If ("file", "dir") runs
// it with only those of the paths typed; unless ("--flag") skips it when the
// flag is typed, and with ("--flag") runs it only when the flag is typed.
type ExtStep struct {
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	CLI    []string       `json:"cli,omitempty"`
	If     string         `json:"if,omitempty"`
	Unless string         `json:"unless,omitempty"`
	With   string         `json:"with,omitempty"`
}

// ExtRoles are the lookups an extension serves.
type ExtRoles struct {
	Search *SearchRole `json:"search,omitempty"`
	Index  *IndexRole  `json:"index,omitempty"`
}

// SearchRole names the search tool and, optionally, the document reader
// and the index list.
type SearchRole struct {
	Tool string `json:"tool"`
	Get  string `json:"get,omitempty"`
	List string `json:"list,omitempty"`
}

// IndexRole is where documents go. Tool and Forget are MCP tools taking
// {"index", "paths"}; CLI is the fallback (see indexSink).
type IndexRole struct {
	Tool   string `json:"tool,omitempty"`
	Forget string `json:"forget,omitempty"`
	CLI    string `json:"cli,omitempty"`
}

// Hook events.
const (
	HookTurnBefore    = "turn.before"
	HookWorkspaceOpen = "workspace.open"
	HookMemorySave    = "memory.save"
	HookSkillsChanged = "skills.changed"
	HookRulesChanged  = "rules.changed"
	HookMCPDocs       = "mcp.docs"
)

// indexHooks are the events that send documents, so need roles.index.
var indexHooks = map[string]bool{HookWorkspaceOpen: true, HookMemorySave: true, HookSkillsChanged: true, HookRulesChanged: true, HookMCPDocs: true}

// slashActions are what every extension's slash command does to its server;
// a command of its own cannot take one of these names.
var slashActions = []string{"on", "off", "unset", "restart", "auto", "manual", "status"}

var (
	slashPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	envName      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Subscribes reports whether e takes hook.
func (e Extension) Subscribes(hook string) bool {
	for _, h := range e.Hooks {
		if h == hook {
			return true
		}
	}
	return false
}

// SlashName is e's slash command, or "".
func (e Extension) SlashName() string {
	if e.Slash == nil {
		return ""
	}
	return e.Slash.Name
}

// ParseExtension reads one manifest. An error names the field.
func ParseExtension(data []byte) (Extension, error) {
	var e Extension
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Extension{}, err
	}
	return e, e.validate()
}

// validate checks a manifest; the error starts with the field.
func (e *Extension) validate() error {
	if !IDPattern.MatchString(e.ID) {
		return fmt.Errorf("id: %q is not lowercase letters, digits, - and _ (at most 32)", e.ID)
	}
	if e.Server.ID != "" && e.Server.ID != e.ID {
		return fmt.Errorf("server.id: %q differs from id %q (leave it out)", e.Server.ID, e.ID)
	}
	e.Server.ID = e.ID
	if (e.Server.URL == "") == (e.Server.Command == "") {
		return fmt.Errorf("server: has a command or a url, not both and not neither")
	}
	via, err := NormalizeVia(e.Server.Via)
	if err != nil {
		return fmt.Errorf("server.via: %w", err)
	}
	if err := CheckOAuth(e.Server, via); err != nil {
		return fmt.Errorf("server.oauth: %w", err)
	}
	if _, err := NormalizeInstance(e.Server.Instance); err != nil {
		return fmt.Errorf("server.instance: %w", err)
	}
	if e.Server.Disabled {
		return fmt.Errorf("server.disabled: an extension is turned off with /mcp off, not disabled")
	}
	if s := e.Slash; s != nil {
		if !slashPattern.MatchString(s.Name) {
			return fmt.Errorf("slash.name: %q is not a lowercase word (letters, digits, -)", s.Name)
		}
		for name, c := range s.Commands {
			if !slashPattern.MatchString(name) {
				return fmt.Errorf("slash.commands.%s: not a lowercase word", name)
			}
			for _, a := range slashActions {
				if name == a {
					return fmt.Errorf("slash.commands.%s: is the server switch's action", name)
				}
			}
			if err := c.validate(e.Roles.Index); err != nil {
				return fmt.Errorf("slash.commands.%s%w", name, err)
			}
		}
	}
	if r := e.Roles.Search; r != nil && r.Tool == "" {
		return fmt.Errorf("roles.search.tool: required")
	}
	if r := e.Roles.Index; r != nil {
		if r.Tool == "" && r.CLI == "" {
			return fmt.Errorf("roles.index: needs tool (with forget) or cli")
		}
		if (r.Tool == "") != (r.Forget == "") {
			return fmt.Errorf("roles.index.forget: tool and forget come together")
		}
		if r.CLI != "" && r.CLI != "raglit" {
			return fmt.Errorf("roles.index.cli: %q — the one CLI dun drives is raglit", r.CLI)
		}
	}
	seen := map[string]bool{}
	for i, h := range e.Hooks {
		switch {
		case seen[h]:
			return fmt.Errorf("hooks[%d]: %q twice", i, h)
		case h == HookTurnBefore:
			if e.Roles.Search == nil {
				return fmt.Errorf("hooks[%d]: %s needs roles.search", i, h)
			}
		case indexHooks[h]:
			if e.Roles.Index == nil {
				return fmt.Errorf("hooks[%d]: %s needs roles.index", i, h)
			}
		default:
			return fmt.Errorf("hooks[%d]: unknown event %q", i, h)
		}
		seen[h] = true
	}
	for k := range e.Env {
		if !envName.MatchString(k) {
			return fmt.Errorf("env.%s: not an environment variable name", k)
		}
	}
	if e.Skills != "" && (filepath.IsAbs(e.Skills) || strings.HasPrefix(filepath.Clean(e.Skills), "..")) {
		return fmt.Errorf("skills: %q must be a directory inside the extension's", e.Skills)
	}
	return nil
}

// expandDir puts the extension's directory in for {{dir}}.
func (e *Extension) expandDir(dir string) {
	rep := func(s string) string { return strings.ReplaceAll(s, "{{dir}}", dir) }
	e.Server.Command = rep(e.Server.Command)
	for _, l := range [][]string{e.Server.Args, e.Server.Env} {
		for i := range l {
			l[i] = rep(l[i])
		}
	}
	for k, v := range e.Env {
		e.Env[k] = rep(v)
	}
}

// spec is the server as the config layers see it: the manifest's server with
// env merged in.
func (e Extension) spec() Spec {
	s := e.Server
	if len(e.Env) > 0 {
		keys := make([]string, 0, len(e.Env))
		for k := range e.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s.Env = append([]string{}, s.Env...)
		for _, k := range keys {
			s.Env = append(s.Env, k+"="+e.Env[k])
		}
	}
	return s
}

// server is e's server as a base layer, placeholders unexpanded (resolve
// expands them).
func (e Extension) server() Server {
	s := e.spec()
	auto := s.Autostart != nil && *s.Autostart
	return Server{ID: s.ID, Command: s.Command, Args: s.Args, URL: s.URL, Headers: requestHeaders(s), Bearer: s.Bearer,
		Env: s.Env, Timeout: s.Timeout, Autostart: auto, Disable: s.Disable, Via: s.Via, Instance: s.Instance, Shell: s.Shell, OAuth: s.OAuth}
}

// SkillsDir is e's skills directory, or "".
func (e Extension) SkillsDir() string {
	if e.Skills == "" || e.Path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(e.Path), e.Skills)
}

// BundledExtensions are raglit and poly-lsp, in bundledOrder: the
// registry's manifest for one where an entry fits (registry.go), else the
// embedded one. A broken embedded manifest is a build defect, caught by
// the tests.
func BundledExtensions() []Extension { return append([]Extension{}, bundled()...) }

// bundled is the bundled extensions as this process uses them.
func bundled() []Extension {
	exts, _ := withRegistry(embeddedExtensions())
	return exts
}

// embeddedExtensions parses the embedded manifests once.
var embeddedExtensions = sync.OnceValue(func() []Extension {
	out := make([]Extension, 0, len(bundledOrder))
	for _, id := range bundledOrder {
		b, err := bundledFS.ReadFile(path.Join(id, ExtensionFile))
		if err != nil {
			panic(err)
		}
		e, err := ParseExtension(b)
		if err != nil {
			panic(fmt.Sprintf("bundled extension %s: %v", id, err))
		}
		e.Source = ExtBundled
		out = append(out, e)
	}
	return out
})

// bundledExt is the bundled extension of id, if there is one.
func bundledExt(id string) (Extension, bool) {
	for _, e := range bundled() {
		if e.ID == CanonicalID(id) {
			return e, true
		}
	}
	return Extension{}, false
}

// UserExtensionsDir is where a user's extensions live.
func UserExtensionsDir() string { return filepath.Join(dunHomeDir(), "extensions") }

// ExtError is a manifest that did not load.
type ExtError struct {
	Path string
	Err  error
}

func (e ExtError) Error() string { return e.Path + ": " + e.Err.Error() }

// LoadExtensions reads the bundled, user and project (dir's .dun/extensions)
// manifests. A later source replaces an earlier one of the same id; a
// manifest that does not parse is skipped and reported.
func LoadExtensions(dir string) ([]Extension, []ExtError) {
	all := BundledExtensions()
	var errs []ExtError
	read := func(root, source string) {
		ents, _ := os.ReadDir(root)
		for _, d := range ents {
			if !d.IsDir() {
				continue
			}
			p := filepath.Join(root, d.Name(), ExtensionFile)
			b, err := os.ReadFile(p)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				errs = append(errs, ExtError{p, err})
				continue
			}
			e, err := ParseExtension(b)
			if err == nil && e.ID != d.Name() {
				err = fmt.Errorf("id: %q is not its directory's name %q", e.ID, d.Name())
			}
			if err != nil {
				errs = append(errs, ExtError{p, err})
				continue
			}
			e.Source, e.Path = source, p
			e.expandDir(filepath.Dir(p))
			replaced := false
			for i := range all {
				if all[i].ID == e.ID {
					all[i], replaced = e, true
				}
			}
			if !replaced {
				all = append(all, e)
			}
		}
	}
	read(UserExtensionsDir(), ExtUser)
	if dir != "" {
		read(filepath.Join(dir, project.DunDir, "extensions"), ExtProject)
	}
	// One slash per name: a later extension that takes a taken one loads
	// without it.
	taken := map[string]string{}
	for i := range all {
		n := all[i].SlashName()
		if n == "" {
			continue
		}
		if other, ok := taken[n]; ok {
			errs = append(errs, ExtError{all[i].Path, fmt.Errorf("slash.name: /%s is %s's; loaded without a slash command", n, other)})
			all[i].Slash = nil
			continue
		}
		taken[n] = all[i].ID
	}
	return all, errs
}

// ExtServers are the servers of the extensions that are not bundled (the
// bundled ones are in DefaultServers), for the base layer, skipping an id
// base already has.
func ExtServers(base []Server, exts []Extension) []Server {
	have := map[string]bool{}
	for _, s := range base {
		have[s.ID] = true
	}
	out := append([]Server{}, base...)
	for _, e := range exts {
		if e.Source != ExtBundled && !have[e.ID] {
			out = append(out, e.server())
		}
	}
	return out
}

var (
	placeholder = regexp.MustCompile(`\{\{[a-z_]+\}\}`)
	flagPattern = regexp.MustCompile(`^--[a-z][a-z0-9-]*$`)
)

// cmdPlaceholders are the placeholders a command may use.
var cmdPlaceholders = map[string]bool{"{{arg}}": true, "{{paths}}": true, "{{path}}": true,
	"{{index}}": true, "{{project}}": true, "{{workspace}}": true}

// AllSteps is c's steps: the inline one, or Steps.
func (c ExtSlashCmd) AllSteps() []ExtStep {
	if len(c.Steps) > 0 {
		return c.Steps
	}
	return []ExtStep{c.ExtStep}
}

// Empty reports whether s names nothing to do.
func (s ExtStep) Empty() bool {
	return s.Tool == "" && s.CLI == nil && s.Args == nil && s.If == "" && s.Unless == "" && s.With == ""
}

// strings are every string the step expands: its args' (nested too) and its
// argv.
func (s ExtStep) strings() []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			out = append(out, v)
		case []any:
			for _, x := range v {
				walk(x)
			}
		case map[string]any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(s.Args)
	return append(out, s.CLI...)
}

// Uses reports whether one of the step's strings holds p.
func (s ExtStep) Uses(p string) bool {
	for _, v := range s.strings() {
		if strings.Contains(v, p) {
			return true
		}
	}
	return false
}

// TakesInput reports whether the step needs text after the command.
func (s ExtStep) TakesInput() bool {
	return s.Uses("{{arg}}") || s.Uses("{{path}}") || s.Uses("{{paths}}")
}

// validate checks one command; the error starts with the field's tail
// (".tool: …", ".steps[1].if: …") for the caller to prefix.
func (c ExtSlashCmd) validate(index *IndexRole) error {
	inline := !c.ExtStep.Empty()
	if inline == (len(c.Steps) > 0) {
		return fmt.Errorf(": has a tool, a cli or steps (one of them)")
	}
	for i, s := range c.AllSteps() {
		at := ""
		if !inline {
			at = fmt.Sprintf(".steps[%d]", i)
		}
		if err := s.validate(index); err != nil {
			return fmt.Errorf("%s%w", at, err)
		}
	}
	return nil
}

func (s ExtStep) validate(index *IndexRole) error {
	switch {
	case (s.Tool == "") == (s.CLI == nil):
		return fmt.Errorf(".tool: a step has a tool or a cli, not both and not neither")
	case s.CLI != nil && len(s.CLI) == 0:
		return fmt.Errorf(".cli: empty")
	case s.CLI != nil && (index == nil || index.CLI == ""):
		return fmt.Errorf(".cli: needs roles.index.cli, the binary it runs")
	case s.CLI != nil && s.Args != nil:
		return fmt.Errorf(".args: a cli step's arguments are its argv")
	case s.If != "" && s.If != "file" && s.If != "dir":
		return fmt.Errorf(".if: %q — want file or dir", s.If)
	case s.If != "" && !s.Uses("{{path}}") && !s.Uses("{{paths}}"):
		return fmt.Errorf(".if: the step uses no {{path}} or {{paths}}")
	case s.Unless != "" && !flagPattern.MatchString(s.Unless):
		return fmt.Errorf(".unless: %q — want a flag, --name", s.Unless)
	case s.With != "" && !flagPattern.MatchString(s.With):
		return fmt.Errorf(".with: %q — want a flag, --name", s.With)
	case s.Uses("{{path}}") && s.Uses("{{paths}}"):
		return fmt.Errorf(": {{path}} and {{paths}} in one step")
	}
	for _, v := range s.strings() {
		for _, p := range placeholder.FindAllString(v, -1) {
			if !cmdPlaceholders[p] {
				return fmt.Errorf(": unknown placeholder %s", p)
			}
		}
		if strings.Contains(v, "{{paths}}") && v != "{{paths}}" {
			return fmt.Errorf(": {{paths}} is a whole value, not part of %q", v)
		}
	}
	return nil
}
