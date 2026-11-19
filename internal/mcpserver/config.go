// Package mcpserver is what an MCP server is to dun before it runs: the
// layered config files (dun.json, dun.local.json, $DUN_HOME/mcp.json) and
// their other sections, the built-in servers, the three switch scopes and
// how they resolve to a Server, extension manifests and the registry's
// overlay of them, and where a server's command is found. Starting,
// sharing and stopping servers is the session's, in internal/harness.
package mcpserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iodesystems/dun/internal/dunhome"
	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/internal/oauth"
	"github.com/iodesystems/dun/internal/project"
)

// Declarative MCP server configuration.
//
// dun's three tool servers were hardcoded in DefaultServers and had to be on
// PATH under exactly those names. That works until any of the following, all of
// which are ordinary:
//
//   - the binaries live somewhere unusual on this machine
//   - a project wants a FOURTH server (a database tool, a project-specific MCP)
//   - a project wants raglit off because it has no docs corpus
//   - a server needs an env var (a DSN, an endpoint) that differs per machine
//
// Two files, layered, because those are two different kinds of fact:
//
//	dun.json        project-level, COMMITTED. "this project needs a db tool."
//	                Belongs in version control because it describes the project.
//	dun.local.json  machine-level. "on THIS box the binary is at /opt/bin/x and
//	                the DSN is ...". Belongs to the machine, and may hold
//	                secrets, so it lives in $DUN_HOME/projects/<id>/, 0600,
//	                where nothing can commit it.
//
// Precedence matches what dun already documents for its LLM settings
// (flag > env > file > default), extended down the middle:
//
//	built-in DefaultServers (or Config.Servers)  <  $DUN_HOME/mcp.json
//	  <  the repo's files  <  the project's state dir  <  the directory's overlay
//
// mcp.json is the GLOBAL scope and the rest the PROJECT scope of a server's
// switches; a session's own switches sit on top (scope.go).
//
// Servers merge BY ID rather than replacing the list wholesale. Overriding one
// binary's path should not require restating the other two — that is how a
// config drifts out of sync with the defaults it silently forked.
//
// dun never writes into a repo (see internal/project). A repo's own files are READ —
// in its root and in a .dun/ beside it, .dun/ winning — because a project may
// commit them on purpose. A dun.local.json there is the older layout, from
// before dun kept machine state in $DUN_HOME, and is still read. Everything dun
// writes (see SetServerOn) goes to $DUN_HOME/projects/<id>/.
//
// The file names (project.ProjectServersFile, project.LocalServersFile,
// project.DunDir) are in internal/project, which owns the state directories.

// serverConfigPaths lists the layered config files, LOWEST precedence first.
// A missing file is skipped. It registers nothing: a directory dun has never
// seen has no project state to read.
func serverConfigPaths(dir string) []string {
	paths := []string{
		filepath.Join(dir, project.ProjectServersFile),
		filepath.Join(dir, project.DunDir, project.ProjectServersFile),
		filepath.Join(dir, project.LocalServersFile),
		filepath.Join(dir, project.DunDir, project.LocalServersFile),
	}
	if p := project.Resolve(dir); p != nil {
		paths = append(paths,
			filepath.Join(p.StateDir, project.ProjectServersFile),
			filepath.Join(p.StateDir, project.LocalServersFile))
		if p.OverlayDir != "" {
			paths = append(paths,
				filepath.Join(p.OverlayDir, project.ProjectServersFile),
				filepath.Join(p.OverlayDir, project.LocalServersFile))
		}
	}
	return paths
}

// Spec is one MCP server as declared in a config file.
type Spec struct {
	// ID is the merge key and the tool namespace. Reusing a built-in id
	// ("poly-lsp", "shell", "raglit") overrides that server rather than adding one.
	ID      string   `json:"id"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// URL is a remote server, reached over Streamable HTTP instead of
	// spawning Command. Bearer, when set, goes as Authorization: Bearer; a
	// value "$NAME" or "${NAME}" is read from that environment variable, so a
	// committed dun.json can name a token without holding it. Headers are
	// any other per-request headers.
	URL     string            `json:"url,omitempty"`
	Bearer  string            `json:"bearer,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Env entries are "KEY=value", passed to the spawned server. A value that
	// is only "$NAME" or "${NAME}" is read from dun's environment at start.
	Env []string `json:"env,omitempty"`
	// Timeout in seconds for tool discovery; 0 keeps dun's default.
	Timeout int `json:"timeout,omitempty"`
	// Disabled removes a server. This is why it exists rather than expecting
	// the file to re-list what it wants: dropping one built-in should be one
	// line, not a transcription of the other two.
	Disabled bool `json:"disabled,omitempty"`
	// Autostart controls whether dun spawns this server at startup. A pointer
	// because, unlike Disabled, this layer must be able to say "false"
	// explicitly: turning autostart back OFF is a normal thing to do
	// (/rag manual) and must not require deleting the entry that turned it on.
	// nil = inherit the layer below.
	Autostart *bool `json:"autostart,omitempty"`
	// Disable is a deny-list of tool names to hide from the model. When empty,
	// all tools discovered from this server are passed through. Mirrors
	// Claude's --disable-tools: name what to exclude, not what to keep.
	// Forward-compatible — new tools added by a server pass through by default.
	// An entry is a name or a glob ("tts_*", path.Match). An empty list (not
	// an absent one) hides nothing and overrides a lower layer's; see
	// MarshalJSON.
	Disable []string `json:"disable,omitempty"`
	// Tools turns single tools on (true) or off (false) by exact name, over
	// any deny-list. Written at project scope (dun.local.json in the
	// project's state); see scope.go.
	Tools map[string]bool `json:"tools,omitempty"`
	// Via is where the server runs: "" or "direct" — dun connects to it and
	// every tool's schema rides every request — or "shell": mcpshell hosts it
	// inside its one eval tool as a namespace (help("<ns>")), and dun does not
	// start it at all. A built-in server cannot be placed in the shell.
	Via string `json:"via,omitempty"`
	// Instance is how many connections the server gets: "shared" (or "", the
	// default: one in dund for every session whose resolved spec is the
	// same), "session" (the engine's own) or "agent" (one more per
	// sub-agent). A built-in server's is fixed (builtinInstances).
	Instance string `json:"instance,omitempty"`
	// Shell is passed to mcpshell for a server it hosts (Via "shell") and
	// ignored for a direct one. A later layer's block replaces a lower one's.
	Shell *ShellOptions `json:"shell,omitempty"`
	// OAuth logs in to a remote server instead of sending a fixed Bearer:
	// the token is obtained in the browser, kept in $DUN_HOME/oauth/<id>.json
	// and refreshed (see internal/oauth/mcpoauth.go). Not with Bearer, and not in the shell.
	OAuth *oauth.Spec `json:"oauth,omitempty"`
	// Attach names indexes the server's search role reads ("project:index")
	// and Detach ones it does not, at this scope; the nearest scope that
	// names an index decides it (harness/attach.go).
	Attach []string `json:"attach,omitempty"`
	Detach []string `json:"detach,omitempty"`
	// Exclude is directories, relative to the workspace, left out of the
	// server's index: passed as --exclude to a poly-lsp-mcp that takes it
	// (harness/indexstate.go). Written at project scope by /lsp exclude and by
	// dun itself when an index does not fit its startup budget.
	Exclude []string `json:"exclude,omitempty"`
	// IndexBudget is the server's index startup budget, as poly-lsp-mcp's
	// --index-budget takes it ("48000ms", "20000ops", "0" unbounded); ""
	// is its default. Written at project scope by /lsp budget, by the
	// person only: dun never raises it on its own (harness/indexstate.go).
	IndexBudget string `json:"index_budget,omitempty"`
	// IndexAnyway lets the server index a root that is not a project (the
	// home directory, /, a directory of clones): passed as --index-anyway to
	// a poly-lsp-mcp that takes it, and it lifts dun's root guard
	// (harness/rootguard.go). Written at project scope only when the person runs
	// /lsp index-anyway on (harness/indexanyway.go): no tool and no answer to a
	// question sets it.
	IndexAnyway bool `json:"index_anyway,omitempty"`
	// LSPRoots replaces dun's safe default for a poly-lsp-mcp's child LSPs
	// (lspSafeArgs: --lsp-roots single), as that flag takes it ("auto").
	// Set by the person in a config file; dun writes none. The removed
	// lsp_max_rss_mb and lsp_idle_stop keys are ignored on load.
	LSPRoots string `json:"lsp_roots,omitempty"`
	// Wake lets the server's messages start a turn on an idle session
	// (harness/serverwake.go). Each wake is a turn someone pays for, so it is off
	// unless set here.
	Wake bool `json:"wake,omitempty"`
}

// ShellOptions are mcpshell's settings for a server it hosts: its --mcp
// entry's "categories" and "unchecked" (mcpshell README).
type ShellOptions struct {
	// Categories groups the server's tools for help("<ns>"): category →
	// tool-name globs. Empty: mcpshell groups by name prefix.
	Categories map[string][]string `json:"categories,omitempty"`
	// Unchecked is tool-name globs whose arguments mcpshell sends as given:
	// no name, required or type check and no type conversion. For tools
	// whose schema is wrong; ["*"] is every tool of the server.
	Unchecked []string `json:"unchecked,omitempty"`
}

// mergeOAuth layers next over prev field by field, as mergeSpec does.
func mergeOAuth(prev, next *oauth.Spec) *oauth.Spec {
	if next == nil {
		return prev
	}
	if prev == nil {
		cp := *next
		return &cp
	}
	out := *prev
	if next.ClientID != "" {
		out.ClientID = next.ClientID
	}
	if next.ClientSecret != "" {
		out.ClientSecret = next.ClientSecret
	}
	if next.Scopes != nil {
		out.Scopes = next.Scopes
	}
	if next.Issuer != "" {
		out.Issuer = next.Issuer
	}
	if next.Audience != "" {
		out.Audience = next.Audience
	}
	if next.Resource != "" {
		out.Resource = next.Resource
	}
	if next.Send != "" {
		out.Send = next.Send
	}
	if next.RedirectPort != 0 {
		out.RedirectPort = next.RedirectPort
	}
	if next.AuthParams != nil {
		out.AuthParams = next.AuthParams
	}
	return &out
}

// CheckOAuth reports what makes a server's oauth block unusable.
func CheckOAuth(s Spec, via string) error {
	o := s.OAuth
	if o == nil {
		return nil
	}
	switch {
	case s.URL == "":
		return fmt.Errorf("oauth is for a remote server (a url), not a command")
	case s.Bearer != "":
		return fmt.Errorf("has both bearer and oauth — keep one")
	case o.Send != "" && o.Send != oauth.SendAccessToken && o.Send != oauth.SendIDToken:
		return fmt.Errorf("oauth send %q: want %q or %q", o.Send, oauth.SendAccessToken, oauth.SendIDToken)
	case o.RedirectPort < 0 || o.RedirectPort > 65535:
		return fmt.Errorf("oauth redirect_port %d is not a port", o.RedirectPort)
	}
	return nil
}

// MarshalJSON writes an empty, non-nil Disable as "disable": [] — omitempty
// would drop it, and then turning the last hidden tool back on would let a
// lower layer's list (the docs server's built-in one) hide them again.
func (s Spec) MarshalJSON() ([]byte, error) {
	type plain Spec
	b, err := json.Marshal(plain(s))
	if err != nil || s.Disable == nil || len(s.Disable) > 0 {
		return b, err
	}
	return append(b[:len(b)-1], `,"disable":[]}`...), nil
}

// Placements a Spec.Via may name. "" means ViaDirect.
const (
	ViaDirect = "direct"
	ViaShell  = "shell"
)

// What the user reads for a placement: standalone (direct — its own tools,
// every schema in every request) or scripted (in the shell — called through
// eval, its schemas out of the prompt). The stored values stay "direct" and
// "shell": an older dun refuses any other "via" and would not start.
const (
	ModeStandalone = "standalone"
	ModeScripted   = "scripted"
)

// ModeWord is a placement as the user reads it.
func ModeWord(via string) string {
	if via == ViaShell {
		return ModeScripted
	}
	return ModeStandalone
}

// NormalizeVia reads a Via value: "" for direct, ViaShell, or an error naming
// what is accepted. The user's words are synonyms.
func NormalizeVia(via string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(via)) {
	case "", ViaDirect, ModeStandalone:
		return "", nil
	case ViaShell, ModeScripted:
		return ViaShell, nil
	}
	return "", fmt.Errorf("mode %q: want %s or %s", via, ModeStandalone, ModeScripted)
}

// Instances a Spec.Instance may name (docs/mcp-and-tools.md, "One
// connection per server"). "" means InstanceShared. InstanceWorkspace is no
// switch value: it is poly-lsp's fixed one (builtinInstances).
const (
	InstanceShared    = "shared"
	InstanceSession   = "session"
	InstanceAgent     = "agent"
	InstanceWorkspace = "workspace"
)

// NormalizeInstance reads an Instance value: "" for shared,
// InstanceSession, InstanceAgent, or an error naming what is accepted.
func NormalizeInstance(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", InstanceShared:
		return "", nil
	case InstanceSession:
		return InstanceSession, nil
	case InstanceAgent:
		return InstanceAgent, nil
	}
	return "", fmt.Errorf("instance %q: want %s, %s or %s", v, InstanceShared, InstanceSession, InstanceAgent)
}

// MountSpec declares an extra local path that must be accessible from both
// the worktree (via symlink) and the Docker container (via volume mount).
// This is how a project tells dun about external dependencies like a sibling
// module referenced by go.mod replace (e.g. agentkit at ../agentkit).
type MountSpec struct {
	// Source is the absolute or relative path on the host. Relative paths
	// are resolved against the repo root.
	Source string `json:"source"`
	// Name is the symlink name inside the worktree parent directory and the
	// volume name for Docker. The Docker mount point is /<name>.
	Name string `json:"name"`
}

// ConfigFile is the on-disk shape of dun.json / dun.local.json.
type ConfigFile struct {
	Servers []Spec `json:"servers,omitempty"`
	// Mounts declares extra local paths that must be accessible from the
	// worktree (symlink) and Docker container (volume mount). Merged by name
	// across layers — a local file can override a source path without
	// restating the project's mount list.
	Mounts []MountSpec `json:"mounts,omitempty"`
	// Commit is how `/worktree commit` writes its message. Merged field by
	// field.
	Commit *CommitConfig `json:"commit,omitempty"`
	// Files configures the built-in file tools (harness/files.go). Merged field by
	// field; ignore patterns accumulate across layers.
	Files *FilesConfig `json:"files,omitempty"`
	// Rag is what dund's code and docs indexes are given (harness/rag_ingest.go). Limits merge field
	// by field; include and exclude accumulate across layers.
	Rag *RagConfig `json:"rag,omitempty"`
	// DefaultVia is the mode of a server no scope sets one for: "" or
	// ViaDirect (standalone), or ViaShell (scripted). Read from
	// $DUN_HOME/mcp.json only (LoadDefaultVia); a project file's is ignored.
	DefaultVia string `json:"default_via,omitempty"`
	// Tools overrides where dun's own tools come from: tools.<name>.path is
	// the binary run instead of the pinned copy (tools.go). Read from
	// $DUN_HOME/mcp.json only; a project file's is ignored.
	Tools map[string]ToolConfig `json:"tools,omitempty"`
	// Tiers and DefaultTier are a project's word on the model tiers
	// (dunconfig/tiers.go): each tier's use text, and the tier a spawn that
	// names none runs on. A tier's provider, model and effort are read only
	// to be ignored, with a warning (LoadTiers): they are $DUN_HOME/config.json's.
	Tiers       map[string]RepoTier `json:"tiers,omitempty"`
	DefaultTier string              `json:"default_tier,omitempty"`
}

// LoadDefaultVia is the global default mode for a server with no mode at any
// scope: ViaShell or ViaDirect. Unset is ViaDirect.
func LoadDefaultVia() (string, error) {
	path := UserServersFile()
	f, err := ReadConfig(path)
	if err != nil || f == nil || f.DefaultVia == "" {
		return ViaDirect, err
	}
	via, err := NormalizeVia(f.DefaultVia)
	if err != nil {
		return "", fmt.Errorf("dun: %s: default_via: %w", path, err)
	}
	if via == "" {
		via = ViaDirect
	}
	return via, nil
}

// SetDefaultVia stores the default mode (ViaShell or ViaDirect) in
// $DUN_HOME/mcp.json. Standalone is stored as "direct", not as absence:
// absence is whatever the default is, and an old dun reads "direct" too.
func SetDefaultVia(via string) (string, error) {
	if via != ViaShell && via != ViaDirect {
		return "", fmt.Errorf("default mode %q: want %s or %s", via, ModeStandalone, ModeScripted)
	}
	path := UserServersFile()
	f, err := ReadConfig(path)
	if err != nil {
		return "", err
	}
	if f == nil {
		f = &ConfigFile{}
	}
	f.DefaultVia = via
	return path, writeUserServers(path, f)
}

// LoadCommit resolves the effective commit config for a workspace. nil means
// "no commit section anywhere", which CommitConfig reads as the conventional
// default — see CommitConfig.
func LoadCommit(dir string) *CommitConfig {
	var out *CommitConfig
	for _, path := range append([]string{UserServersFile()}, serverConfigPaths(dir)...) {
		f, err := ReadConfig(path)
		if err != nil || f == nil || f.Commit == nil {
			continue
		}
		if out == nil {
			cp := *f.Commit
			out = &cp
			continue
		}
		if s := strings.TrimSpace(f.Commit.Format); s != "" {
			out.Format = s
		}
		if s := strings.TrimSpace(f.Commit.Instruction); s != "" {
			out.Instruction = s
		}
	}
	return out
}

// LoadServers resolves the effective server list for a workspace, with no
// session switches (see scope.go for the scopes).
//
// dir is where the project files are looked for (the repo being worked in).
// Both files are optional and a missing one is not an error — the built-in trio
// is a working default and most projects will never need to override it.
func LoadServers(dir, workspace, raglitHome string) ([]Server, error) {
	return Resolve(dir, workspace, raglitHome, DefaultServers(workspace, raglitHome), nil, nil)
}

// LoadMounts resolves the effective mount list for a workspace.
//
// Mounts merge BY NAME across layers (dun.json < dun.local.json). A local file
// can override a source path without restating the project's mount list.
// Relative source paths are resolved against repoRoot.
//
// Auto-discovered mounts from go.mod replace directives are prepended — so a
// project with "replace => ../agentkit" gets the agentkit mount automatically
// even without a mounts section.
func LoadMounts(dir, repoRoot string) []MountSpec {
	// Auto-discover mounts from go.mod replace directives.
	auto := discoverGoModReplaces(dir, repoRoot)
	merged := map[string]MountSpec{}
	order := []string{}
	for _, m := range auto {
		merged[m.Name] = m
		order = append(order, m.Name)
	}

	for _, path := range append([]string{UserServersFile()}, serverConfigPaths(dir)...) {
		f, err := ReadConfig(path)
		if err != nil || f == nil {
			continue
		}
		for _, spec := range f.Mounts {
			if spec.Name == "" {
				continue // skip malformed entries
			}
			prev, existed := merged[spec.Name]
			if !existed {
				order = append(order, spec.Name)
				merged[spec.Name] = spec
			} else if spec.Source != "" {
				prev.Source = spec.Source
				merged[spec.Name] = prev
			}
		}
	}

	out := make([]MountSpec, 0, len(order))
	for _, name := range order {
		m := merged[name]
		if m.Source == "" {
			continue
		}
		// Resolve relative paths against repoRoot.
		if !filepath.IsAbs(m.Source) {
			m.Source = filepath.Join(repoRoot, m.Source)
		}
		out = append(out, m)
	}
	return out
}

// discoverGoModReplaces reads go.mod and extracts local replace directives as
// MountSpecs. A replace like "github.com/iodesystems/agentkit -> ../agentkit"
// becomes a mount with name "agentkit" and source "../agentkit". Both the
// one-line form and a "replace ( … )" block are read.
func discoverGoModReplaces(dir, repoRoot string) []MountSpec {
	gomodPath := filepath.Join(repoRoot, "go.mod")
	b, err := os.ReadFile(gomodPath)
	if err != nil {
		return nil
	}
	var mounts []MountSpec
	seen := map[string]bool{}
	inBlock := false
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		// Format: replace module/path => ../local/path
		// or:     replace module/path v1.2.3 => ../local/path
		// or a block of the same lines without the keyword.
		var rest string
		switch {
		case inBlock && line == ")":
			inBlock = false
			continue
		case inBlock:
			rest = line
		case strings.HasPrefix(line, "replace") && strings.TrimSpace(strings.TrimPrefix(line, "replace")) == "(":
			inBlock = true
			continue
		case strings.HasPrefix(line, "replace "):
			rest = strings.TrimPrefix(line, "replace ")
		default:
			continue
		}
		parts := strings.SplitN(rest, "=>", 2)
		if len(parts) != 2 {
			continue
		}
		m, ok := replaceMount(strings.Trim(strings.TrimSpace(parts[1]), `"`))
		if !ok || seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		mounts = append(mounts, m)
	}
	return mounts
}

// replaceMount is what a replace target needs beside a worktree to resolve
// from inside it, and ok=false when it needs nothing.
//
// A session worktree is a CHECKOUT somewhere else, so "../x" from it is not the
// repo's sibling. The symlink goes in the worktree's parent under the FIRST
// path component past the "..": "../x/sub" needs <parent>/x, not <parent>/sub,
// for x/sub to exist under it. A target inside the repo ("./tools") needs
// nothing: the worktree has its own copy. Non-local targets (another module
// path and a version) are not paths at all.
//
// "../../x" climbs past the worktree's parent, where no symlink beside the
// worktree can reach. It still yields a mount: Docker mounts it at /x, and
// /work/../../x is /x. On the host it does not resolve (not handled yet).
func replaceMount(target string) (MountSpec, bool) {
	if filepath.IsAbs(target) {
		name := filepath.Base(target)
		if name == "" || name == "." || name == string(os.PathSeparator) {
			return MountSpec{}, false
		}
		return MountSpec{Source: target, Name: name}, true
	}
	if !strings.HasPrefix(target, ".") {
		return MountSpec{}, false
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(target)), "/")
	up := 0
	for up < len(parts) && parts[up] == ".." {
		up++
	}
	if up == 0 || up == len(parts) {
		return MountSpec{}, false // inside the repo, or a bare "../.." with no name
	}
	return MountSpec{Source: strings.Join(parts[:up+1], "/"), Name: parts[up]}, true
}

// mergeSpec layers next over prev field by field. An omitted field INHERITS
// rather than clearing, so a local file can override just a command path and
// keep the args it does not care about.
func mergeSpec(prev, next Spec) Spec {
	out := prev
	out.ID = next.ID
	if next.Command != "" {
		out.Command = next.Command
	}
	if next.Args != nil {
		out.Args = next.Args
	}
	if next.URL != "" {
		out.URL = next.URL
	}
	if next.Bearer != "" {
		out.Bearer = next.Bearer
	}
	if next.Headers != nil {
		out.Headers = next.Headers
	}
	if next.Env != nil {
		out.Env = next.Env
	}
	if next.Timeout != 0 {
		out.Timeout = next.Timeout
	}
	// Disabled is a bool and cannot distinguish "false" from "absent", so it is
	// only ever turned ON by a later layer. Re-enabling a disabled server means
	// removing the entry that disabled it — explicit, and it keeps a local file
	// from silently resurrecting something the project turned off.
	if next.Disabled {
		out.Disabled = true
	}
	if next.Autostart != nil {
		out.Autostart = next.Autostart
	}
	if next.Disable != nil {
		out.Disable = next.Disable
	}
	// Per tool, so one layer's switch does not drop another's.
	if next.Tools != nil {
		m := map[string]bool{}
		for k, v := range out.Tools {
			m[k] = v
		}
		for k, v := range next.Tools {
			m[k] = v
		}
		out.Tools = m
	}
	// "direct" is a value, not an absence, so a higher layer can move a server
	// back out of the shell.
	if next.Via != "" {
		out.Via = next.Via
	}
	if next.Instance != "" {
		out.Instance = next.Instance
	}
	if next.Shell != nil {
		out.Shell = next.Shell
	}
	out.OAuth = mergeOAuth(prev.OAuth, next.OAuth)
	if next.Attach != nil {
		out.Attach = next.Attach
	}
	if next.Detach != nil {
		out.Detach = next.Detach
	}
	if next.Exclude != nil {
		out.Exclude = next.Exclude
	}
	if next.IndexBudget != "" {
		out.IndexBudget = next.IndexBudget
	}
	if next.IndexAnyway {
		out.IndexAnyway = true
	}
	if next.LSPRoots != "" {
		out.LSPRoots = next.LSPRoots
	}
	if next.Wake {
		out.Wake = true
	}
	return out
}

// requestHeaders resolves a remote server's headers: Headers, plus
// Authorization from Bearer with any $NAME read from the environment.
func requestHeaders(s Spec) map[string]string {
	if s.URL == "" || (s.Bearer == "" && len(s.Headers) == 0) {
		return nil
	}
	out := map[string]string{}
	for k, v := range s.Headers {
		out[k] = os.ExpandEnv(v)
	}
	if tok := os.ExpandEnv(s.Bearer); tok != "" {
		out["Authorization"] = "Bearer " + tok
	}
	return out
}

// UserServersFile is the per-user server list, $DUN_HOME/mcp.json: servers
// added with /mcp add, there in every workspace. It is the lowest file layer
// — a project's dun.json may still override or disable one by id. 0600,
// because a bearer token lives in it.
func UserServersFile() string {
	return filepath.Join(dunHomeDir(), "mcp.json")
}

// PutUserServer adds spec to the user server list, replacing one of the same
// id. A mode or instance spec leaves empty is kept from the entry it replaces:
// they are switches, not part of the definition. It returns the file written.
func PutUserServer(spec Spec) (string, error) {
	spec, err := KeepSwitches(spec)
	if err != nil {
		return "", err
	}
	path := UserServersFile()
	f, err := ReadConfig(path)
	if err != nil {
		return "", err
	}
	if f == nil {
		f = &ConfigFile{}
	}
	replaced := false
	for i := range f.Servers {
		if f.Servers[i].ID == spec.ID {
			f.Servers[i], replaced = spec, true
		}
	}
	if !replaced {
		f.Servers = append(f.Servers, spec)
	}
	return path, writeUserServers(path, f)
}

// KeepSwitches fills in the mode and instance spec leaves empty from the
// user-list entry of the same id, as PutUserServer will store it.
func KeepSwitches(spec Spec) (Spec, error) {
	f, err := ReadConfig(UserServersFile())
	if err != nil || f == nil {
		return spec, err
	}
	for _, old := range f.Servers {
		if old.ID != spec.ID {
			continue
		}
		if spec.Via == "" {
			spec.Via = old.Via
		}
		if spec.Instance == "" {
			spec.Instance = old.Instance
		}
	}
	return spec, nil
}

// RemoveUserServer drops id from the user server list; it reports whether
// it was there.
func RemoveUserServer(id string) (bool, error) {
	path := UserServersFile()
	f, err := ReadConfig(path)
	if err != nil || f == nil {
		return false, err
	}
	kept := f.Servers[:0]
	for _, s := range f.Servers {
		if s.ID != id {
			kept = append(kept, s)
		}
	}
	if len(kept) == len(f.Servers) {
		return false, nil
	}
	f.Servers = kept
	return true, writeUserServers(path, f)
}

// UpsertUserServer edits id's entry in the user server list, adding one that
// holds only the edit when there is none (a global switch for a built-in, or
// for a server a repo defines). It returns the file written.
func UpsertUserServer(id string, edit func(*Spec)) (string, error) {
	path := UserServersFile()
	f, err := ReadConfig(path)
	if err != nil {
		return "", err
	}
	if f == nil {
		f = &ConfigFile{}
	}
	found := false
	for i := range f.Servers {
		if f.Servers[i].ID == id {
			edit(&f.Servers[i])
			found = true
		}
	}
	if !found {
		s := Spec{ID: id}
		edit(&s)
		f.Servers = append(f.Servers, s)
	}
	return path, writeUserServers(path, f)
}

// OnUserServersWritten is called after every write of the global server
// file. The engine under dund sets it to tell the owner's other sessions
// (proto.ServersChanged); nothing else needs it.
var OnUserServersWritten func()

func writeUserServers(path string, f *ConfigFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if OnUserServersWritten != nil {
		OnUserServersWritten()
	}
	return nil
}

// expandPlaceholders substitutes the paths a config file cannot know.
//
// {{workspace}} is the same token llm-bench's toolsets already use, so anyone
// who has written one of those configs will recognise it. {{project}} and
// {{index}} are raglit's corpus and membership names for the workspace
// (RaglitProject, RaglitIndex); they ask git, so only when used.
func expandPlaceholders(in []string, workspace, raglitHome string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		s = strings.ReplaceAll(s, "{{workspace}}", workspace)
		s = strings.ReplaceAll(s, "{{raglit_home}}", raglitHome)
		if strings.Contains(s, "{{project}}") {
			s = strings.ReplaceAll(s, "{{project}}", RaglitProject(workspace))
		}
		if strings.Contains(s, "{{index}}") {
			s = strings.ReplaceAll(s, "{{index}}", RaglitIndex(workspace))
		}
		out[i] = s
	}
	return out
}

// ReadConfig reads one config file; a missing one is nil, no error.
func ReadConfig(path string) (*ConfigFile, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dun: read %s: %w", path, err)
	}
	var f ConfigFile
	if err := json.Unmarshal(b, &f); err != nil {
		// Name the file. A JSON error with no filename in a two-file layered
		// config sends the reader to the wrong one.
		return nil, fmt.Errorf("dun: parse %s: %w", path, err)
	}
	for i := range f.Servers {
		f.Servers[i].ID = CanonicalID(f.Servers[i].ID)
	}
	return &f, nil
}

// UpdateLocalServer edits (or adds) id's entry in dir's machine-state
// dun.local.json and returns the file written.
func UpdateLocalServer(dir, id string, edit func(*Spec)) (string, error) {
	p, err := project.Ensure(dir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(p.WriteDir(), project.LocalServersFile)
	f, err := ReadConfig(path)
	if err != nil {
		return "", err
	}
	if f == nil {
		f = &ConfigFile{}
	}
	found := false
	for i := range f.Servers {
		if f.Servers[i].ID == id {
			edit(&f.Servers[i])
			found = true
			break
		}
	}
	if !found {
		s := Spec{ID: id}
		edit(&s)
		f.Servers = append(f.Servers, s)
	}
	// An entry an unset emptied goes: a bare id reads as a server with no
	// command (see switchesOnly).
	kept := f.Servers[:0]
	for _, s := range f.Servers {
		if b, _ := json.Marshal(s); string(b) != `{"id":`+strconvQuote(s.ID)+`}` {
			kept = append(kept, s)
		}
	}
	f.Servers = kept
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", fmt.Errorf("dun: encode %s: %w", path, err)
	}
	if err := fsutil.WriteAtomic(path, append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// RemoveLocalServer deletes id's entry (switches and tool switches) from
// dir's project state file, and from the linked-worktree overlay's when dir
// has one. It returns the files it changed. No project, or no entry, is none.
func RemoveLocalServer(dir, id string) ([]string, error) {
	p := project.Resolve(dir)
	if p == nil {
		return nil, nil
	}
	dirs := []string{p.StateDir}
	if p.OverlayDir != "" {
		dirs = append(dirs, p.OverlayDir)
	}
	var changed []string
	for _, d := range dirs {
		path := filepath.Join(d, project.LocalServersFile)
		f, err := ReadConfig(path)
		if err != nil {
			return changed, err
		}
		if f == nil {
			continue
		}
		kept := f.Servers[:0]
		for _, s := range f.Servers {
			if s.ID != id {
				kept = append(kept, s)
			}
		}
		if len(kept) == len(f.Servers) {
			continue
		}
		f.Servers = kept
		b, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			return changed, fmt.Errorf("dun: encode %s: %w", path, err)
		}
		if err := fsutil.WriteAtomic(path, append(b, '\n'), 0o600); err != nil {
			return changed, err
		}
		changed = append(changed, path)
	}
	return changed, nil
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// IDs lists the resolved ids, for logs and diagnostics.
func IDs(servers []Server) []string {
	ids := make([]string, 0, len(servers))
	for _, s := range servers {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}

// dunHomeDir is $DUN_HOME, else ~/.dun (harness.DunHome).
func dunHomeDir() string { return filepath.Clean(dunhome.Dir()) }
