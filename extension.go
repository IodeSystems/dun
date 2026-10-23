package dun

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/agentkit/ragnotify"
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
//	             /<name> <command> <text> calls commands[command].tool with
//	             {arg: text}
//	roles        lookups dun makes on demand:
//	               search {tool, get}: tool takes {"query"} and answers hits
//	                 (raglit's shape); get takes {"path", "max_chars"} and
//	                 answers {"text", "truncated"}, to inline a short hit.
//	                 When tool takes an "index", /<slash> attach|detach
//	                 adds other projects' indexes to it (attach.go)
//	               index {tool, forget, cli}: where documents go — see
//	                 indexSink for the MCP/CLI choice. With cli,
//	                 /<slash> watch <dir> has the CLI watch a directory
//	                 into an index
//	hooks        events dun sends:
//	               turn.before      search the conversation, surface a clear
//	                                winner as an aside (roles.search)
//	               workspace.open   index the workspace when the server starts
//	               memory.save      memory notes (needs a turn.before
//	                                extension, which is how a note comes back)
//	               skills.changed   the local skills, SKILL.md and references
//	               rules.changed    the rules' imports, in /rules index mode
//	               mcp.docs         MCP servers' resources, on /mcp add|refresh
//	             the index events need roles.index
//	host_hooks   the extension calls dun: {ENV_VAR: script} starts the server
//	             with ENV_VAR naming one of dun's host scripts
//	             (agents_md_guard: the AGENTS.md guard, agents_hook.go)
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

//go:embed examples/extensions/*/extension.json
var bundledFS embed.FS

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
	Server      ServerSpec        `json:"server"`
	Slash       *ExtSlash         `json:"slash,omitempty"`
	Roles       ExtRoles          `json:"roles,omitempty"`
	Hooks       []string          `json:"hooks,omitempty"`
	HostHooks   map[string]string `json:"host_hooks,omitempty"`
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
	Commands    map[string]ExtSlashCmd `json:"commands,omitempty"`
}

// ExtSlashCmd maps /<slash> <command> <text> to a tool call.
type ExtSlashCmd struct {
	Tool        string `json:"tool"`
	Arg         string `json:"arg,omitempty"` // the parameter <text> goes in; none: no arguments
	Description string `json:"description,omitempty"`
}

// ExtRoles are the lookups an extension serves.
type ExtRoles struct {
	Search *SearchRole `json:"search,omitempty"`
	Index  *IndexRole  `json:"index,omitempty"`
}

// SearchRole names the search tool and, optionally, the document reader.
type SearchRole struct {
	Tool string `json:"tool"`
	Get  string `json:"get,omitempty"`
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

// HostAgentsGuard is the AGENTS.md guard (agents_hook.go).
const HostAgentsGuard = "agents_md_guard"

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
	if !serverIDPattern.MatchString(e.ID) {
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
	if err := checkOAuth(e.Server, via); err != nil {
		return fmt.Errorf("server.oauth: %w", err)
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
			if c.Tool == "" {
				return fmt.Errorf("slash.commands.%s.tool: required", name)
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
	for k, v := range e.HostHooks {
		if !envName.MatchString(k) {
			return fmt.Errorf("host_hooks.%s: not an environment variable name", k)
		}
		if v != HostAgentsGuard {
			return fmt.Errorf("host_hooks.%s: unknown host script %q (have %s)", k, v, HostAgentsGuard)
		}
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
func (e Extension) spec() ServerSpec {
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
		Env: s.Env, Timeout: s.Timeout, Autostart: auto, Disable: s.Disable, Via: s.Via, OAuth: s.OAuth}
}

// skillsDir is e's skills directory, or "".
func (e Extension) skillsDir() string {
	if e.Skills == "" || e.Path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(e.Path), e.Skills)
}

// BundledExtensions are raglit and poly-lsp, in bundledOrder. A broken
// embedded manifest is a build defect, caught by the tests.
func BundledExtensions() []Extension { return append([]Extension{}, bundled()...) }

// bundled parses the embedded manifests once.
var bundled = sync.OnceValue(func() []Extension {
	out := make([]Extension, 0, len(bundledOrder))
	for _, id := range bundledOrder {
		b, err := bundledFS.ReadFile(path.Join("examples/extensions", id, ExtensionFile))
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
		if e.ID == canonicalServerID(id) {
			return e, true
		}
	}
	return Extension{}, false
}

// UserExtensionsDir is where a user's extensions live.
func UserExtensionsDir() string { return filepath.Join(DunHome(), "extensions") }

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
		read(filepath.Join(dir, DunDir, "extensions"), ExtProject)
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

// extServers are the servers of the extensions that are not bundled (the
// bundled ones are in DefaultServers), for the base layer, skipping an id
// base already has.
func extServers(base []Server, exts []Extension) []Server {
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

// Extensions is every loaded extension, bundled first.
func (h *Harness) Extensions() []Extension { return h.extensions() }

// ExtensionErrors are the manifests that did not load.
func (h *Harness) ExtensionErrors() []ExtError { return h.extErrs }

// extensions is the loaded list, or the bundled ones for a harness built
// without Start (tests).
func (h *Harness) extensions() []Extension {
	if h.exts != nil {
		return h.exts
	}
	return BundledExtensions()
}

// extension is the extension whose server is id.
func (h *Harness) extension(id string) (Extension, bool) {
	for _, e := range h.extensions() {
		if e.ID == id {
			return e, true
		}
	}
	return Extension{}, false
}

// hasTool reports whether tools has server id's tool name.
func hasTool(tools []mcpmgr.MCPTool, id, name string) bool {
	for _, t := range tools {
		if t.ServerID == id && t.Name == name {
			return true
		}
	}
	return false
}

// searchExts are the extensions taking turn.before whose search tool is in
// tools (running, and not hidden), in load order.
func searchExts(exts []Extension, tools []mcpmgr.MCPTool) []Extension {
	var out []Extension
	for _, e := range exts {
		if e.Subscribes(HookTurnBefore) && hasTool(tools, e.ID, e.Roles.Search.Tool) {
			out = append(out, e)
		}
	}
	return out
}

// hookExt is the first extension taking hook whose server has tools in
// tools, i.e. is running.
func hookExt(exts []Extension, hook string, tools []mcpmgr.MCPTool) (Extension, bool) {
	for _, e := range exts {
		if !e.Subscribes(hook) {
			continue
		}
		for _, t := range tools {
			if t.ServerID == e.ID {
				return e, true
			}
		}
	}
	return Extension{}, false
}

// hookStarted reports whether an extension taking hook has its server up.
func (h *Harness) hookStarted(hook string) bool {
	if h.mgr == nil {
		return false
	}
	for _, e := range h.extensions() {
		if e.Subscribes(hook) && h.mgr.ServerStarted(e.ID) {
			return true
		}
	}
	return false
}

// indexSink is where e's index role sends documents, or nil.
//
// THE SEAM: dun sends documents in batches (500 paths a call) and forgets
// them again. raglit's MCP `ingest` takes one URL and it has no `forget`
// tool (asked of raglit, 2026-10-23), so its manifest names the CLI
// fallback ("cli": "raglit") and dun runs the server's own binary with the
// store flags of its serve args. Once the server's index tool takes a
// `paths` array and its forget tool exists, the MCP tools are used instead,
// with no change to dun or to the manifest: {"index", "paths"} to each.
func (h *Harness) indexSink(e Extension) *mcpDocsSink {
	r := e.Roles.Index
	if r == nil {
		return nil
	}
	h.srvMu.Lock()
	spec, ok := h.spec(e.ID)
	h.srvMu.Unlock()
	if !ok {
		return nil
	}
	if r.Tool != "" && h.mgr != nil && h.mgr.ServerStarted(e.ID) {
		tools := h.mgr.GetTools()
		if toolTakes(tools, e.ID, r.Tool, "paths") && hasTool(tools, e.ID, r.Forget) {
			return mcpIndexSink(h.mgr, e.ID, r.Tool, r.Forget)
		}
	}
	if r.CLI != "" && filepath.Base(spec.Command) == r.CLI {
		return raglitDocsSink(spec)
	}
	return nil
}

// toolTakes reports whether server id's tool has the parameter param.
func toolTakes(tools []mcpmgr.MCPTool, id, tool, param string) bool {
	for _, t := range tools {
		if t.ServerID != id || t.Name != tool {
			continue
		}
		props, _ := t.InputSchema["properties"].(map[string]any)
		_, ok := props[param]
		return ok
	}
	return false
}

// mcpIndexSink sends documents through an extension's MCP tools, in the
// same batches the CLI gets.
func mcpIndexSink(mgr *mcpmgr.Manager, id, ingest, forget string) *mcpDocsSink {
	call := func(tool, index string, paths []string) error {
		for _, batch := range chunkArgs(paths, ingestBatch) {
			if _, err := mgr.CallTool(context.Background(), id, tool, map[string]any{"index": index, "paths": batch}); err != nil {
				return fmt.Errorf("%s %s: %w", id, tool, err)
			}
		}
		return nil
	}
	return &mcpDocsSink{
		ingest: func(index string, paths []string) error { return call(ingest, index, paths) },
		forget: func(index string, paths []string) error { return call(forget, index, paths) },
	}
}

// hostHookEnv is the env that arms e's host hooks for workspace.
func hostHookEnv(e Extension, workspace string) []string {
	var env []string
	keys := make([]string, 0, len(e.HostHooks))
	for k := range e.HostHooks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if e.HostHooks[k] == HostAgentsGuard {
			env = append(env, agentsHookEnv(k, workspace)...)
		}
	}
	return env
}

// surfacer is the proactive search over the turn.before extensions: their
// finders, merged; which one each hit came from; and the reader of a short
// hit. One extension is that extension's finder as it is.
type surfacer struct {
	finder agent.DocFinder
	fetch  docBodyFunc
	from   func(docID string) string // the extension a hit came from
}

// sel is each extension's attached-index selector (attach.go), "" for none.
func newSurfacer(mgr *mcpmgr.Manager, exts []Extension, sel map[string]string) *surfacer {
	if len(exts) == 0 {
		return nil
	}
	finders := make([]agent.DocFinder, len(exts))
	fetches := map[string]docBodyFunc{}
	for i, e := range exts {
		var opts ragnotify.Opts
		getIndex := ""
		if s := sel[e.ID]; s != "" {
			opts.ExtraArgs = map[string]any{"index": s}
			if toolTakes(mgr.GetTools(), e.ID, e.Roles.Search.Get, "index") {
				getIndex = s
			}
		}
		finders[i] = ragnotify.MCPFinder(mgr, e.ID, e.Roles.Search.Tool, opts)
		if e.Roles.Search.Get != "" {
			fetches[e.ID] = docsBodyFetcher(mgr, e.ID, e.Roles.Search.Get, getIndex)
		}
	}
	if len(exts) == 1 {
		id := exts[0].ID
		return &surfacer{finder: finders[0], fetch: fetches[id], from: func(string) string { return id }}
	}
	m := &multiFinder{ids: make([]string, len(exts)), finders: finders, from: map[string]string{}}
	for i, e := range exts {
		m.ids[i] = e.ID
	}
	fetch := func(ctx context.Context, docID string, max int) (string, bool) {
		if f := fetches[m.source(docID)]; f != nil {
			return f(ctx, docID, max)
		}
		return "", false
	}
	return &surfacer{finder: m, fetch: fetch, from: m.source}
}

// multiFinder asks every finder and remembers which one answered each hit.
// Scores from two extensions do not compare, so each extension after the
// first is its own surfacing group (see group).
type multiFinder struct {
	ids     []string
	finders []agent.DocFinder
	mu      sync.Mutex
	from    map[string]string
}

func (m *multiFinder) Find(ctx context.Context, texts []string) ([]agent.DocHit, error) {
	var all []agent.DocHit
	var firstErr error
	ok := false
	for i, f := range m.finders {
		hits, err := f.Find(ctx, texts)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok = true
		m.mu.Lock()
		for _, h := range hits {
			if _, seen := m.from[h.DocID]; !seen {
				m.from[h.DocID] = m.ids[i]
			}
		}
		m.mu.Unlock()
		all = append(all, hits...)
	}
	if !ok {
		return nil, firstErr
	}
	return all, nil
}

func (m *multiFinder) source(docID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.from[docID]
}

// group wraps a surfacing group function: a hit from an extension other
// than the first is grouped by that extension.
func (s *surfacer) group(first string, inner func(string) string) func(string) string {
	return func(docID string) string {
		if id := s.from(docID); id != "" && id != first {
			return "ext:" + id
		}
		return inner(docID)
	}
}

// RunExtCommand carries out /<slash> <command> <text>: the extension's tool,
// called with {arg: text} (or no arguments), its result the answer.
func (h *Harness) RunExtCommand(ctx context.Context, id, command, text string) (string, error) {
	e, ok := h.extension(id)
	if !ok || e.Slash == nil {
		return "", fmt.Errorf("no extension %q with a slash command", id)
	}
	c, ok := e.Slash.Commands[command]
	if !ok {
		return "", fmt.Errorf("/%s has no command %q", e.Slash.Name, command)
	}
	if h.mgr == nil || !h.mgr.ServerStarted(id) {
		return "", fmt.Errorf("%s is not running — /%s on first", id, e.Slash.Name)
	}
	args := map[string]any{}
	if c.Arg != "" {
		args[c.Arg] = strings.TrimSpace(text)
	}
	return h.mgr.CallTool(ctx, id, c.Tool, args)
}
