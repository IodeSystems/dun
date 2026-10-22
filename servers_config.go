package dun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
//	built-in DefaultServers  <  $DUN_HOME/mcp.json  <  the repo's files
//	  <  the project's state dir  <  the directory's overlay  <  Config.Servers (Go)
//
// Servers merge BY ID rather than replacing the list wholesale. Overriding one
// binary's path should not require restating the other two — that is how a
// config drifts out of sync with the defaults it silently forked.
//
// dun never writes into a repo (see project.go). A repo's own files are READ —
// in its root and in a .dun/ beside it, .dun/ winning — because a project may
// commit them on purpose. A dun.local.json there is the older layout, from
// before dun kept machine state in $DUN_HOME, and is still read. Everything dun
// writes (see SetAutostart) goes to $DUN_HOME/projects/<id>/.
const (
	// ProjectServersFile is committed and describes the project.
	ProjectServersFile = "dun.json"
	// LocalServersFile is machine state: never committed, written 0600.
	LocalServersFile = "dun.local.json"
	// DunDir is the directory in a repo where a project MAY commit dun.json
	// and skills/. dun reads it and never creates it.
	DunDir = ".dun"
)

// serverConfigPaths lists the layered config files, LOWEST precedence first.
// A missing file is skipped. It registers nothing: a directory dun has never
// seen has no project state to read.
func serverConfigPaths(dir string) []string {
	paths := []string{
		filepath.Join(dir, ProjectServersFile),
		filepath.Join(dir, DunDir, ProjectServersFile),
		filepath.Join(dir, LocalServersFile),
		filepath.Join(dir, DunDir, LocalServersFile),
	}
	if p := ResolveProject(dir); p != nil {
		paths = append(paths,
			filepath.Join(p.StateDir, ProjectServersFile),
			filepath.Join(p.StateDir, LocalServersFile))
		if p.OverlayDir != "" {
			paths = append(paths,
				filepath.Join(p.OverlayDir, ProjectServersFile),
				filepath.Join(p.OverlayDir, LocalServersFile))
		}
	}
	return paths
}

// ServerSpec is one MCP server as declared in a config file.
type ServerSpec struct {
	// ID is the merge key and the tool namespace. Reusing a built-in id
	// ("code", "shell", "docs") overrides that server rather than adding one.
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
	// Env entries are "KEY=value", passed to the spawned server.
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
	Disable []string `json:"disable,omitempty"`
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

// ServersFile is the on-disk shape of dun.json / dun.local.json.
type ServersFile struct {
	Servers []ServerSpec `json:"servers,omitempty"`
	// Mounts declares extra local paths that must be accessible from the
	// worktree (symlink) and Docker container (volume mount). Merged by name
	// across layers — a local file can override a source path without
	// restating the project's mount list.
	Mounts []MountSpec `json:"mounts,omitempty"`
	// Ship is the ship tool's policy and checks. Merged FIELD BY FIELD across
	// layers, so a machine can permit a mode or skip a check without
	// transcribing the project's whole check list.
	Ship *ShipConfig `json:"ship,omitempty"`
	// Commit is how `/worktree commit` writes its message. Merged field by
	// field, same as Ship.
	Commit *CommitConfig `json:"commit,omitempty"`
}

// LoadShip resolves the effective ship config for a workspace. nil means "no
// ship section anywhere", which ShipConfig's methods read as permissive
// defaults with no checks — see ShipConfig.
func LoadShip(dir string) *ShipConfig {
	var out *ShipConfig
	for _, path := range append([]string{UserServersFile()}, serverConfigPaths(dir)...) {
		f, err := readServersFile(path)
		if err != nil || f == nil || f.Ship == nil {
			continue
		}
		if out == nil {
			cp := *f.Ship
			out = &cp
			continue
		}
		merged := mergeShip(*out, *f.Ship)
		out = &merged
	}
	return out
}

// LoadCommit resolves the effective commit config for a workspace. nil means
// "no commit section anywhere", which CommitConfig reads as the conventional
// default — see CommitConfig.
func LoadCommit(dir string) *CommitConfig {
	var out *CommitConfig
	for _, path := range append([]string{UserServersFile()}, serverConfigPaths(dir)...) {
		f, err := readServersFile(path)
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

// mergeShip layers one ship section over another: a field the higher layer did
// not state is INHERITED, for the same reason ServerSpec fields are — turning
// one knob must not mean restating the rest of the policy.
func mergeShip(prev, next ShipConfig) ShipConfig {
	if len(next.Allow) > 0 {
		prev.Allow = next.Allow
	}
	if next.Default != "" {
		prev.Default = next.Default
	}
	if next.CheckTimeout != "" {
		prev.CheckTimeout = next.CheckTimeout
	}
	if len(next.Skip) > 0 {
		prev.Skip = next.Skip
	}
	if len(next.Checks) > 0 {
		prev.Checks = next.Checks
	}
	return prev
}

// LoadServers resolves the effective server list for a workspace.
//
// dir is where the project files are looked for (the repo being worked in).
// Both files are optional and a missing one is not an error — the built-in trio
// is a working default and most projects will never need to override it.
func LoadServers(dir, workspace, raglitHome string) ([]Server, error) {
	merged := map[string]ServerSpec{}
	order := []string{}
	for _, s := range DefaultServers(workspace, raglitHome) {
		auto := s.Autostart
		merged[s.ID] = ServerSpec{ID: s.ID, Command: s.Command, Args: s.Args, Autostart: &auto, Disable: s.Disable}
		order = append(order, s.ID)
	}

	for _, path := range append([]string{UserServersFile()}, serverConfigPaths(dir)...) {
		f, err := readServersFile(path)
		if err != nil {
			return nil, err
		}
		if f == nil {
			continue
		}
		for _, spec := range f.Servers {
			if spec.ID == "" {
				return nil, fmt.Errorf("dun: %s: a server entry has no id", path)
			}
			prev, existed := merged[spec.ID]
			if !existed {
				order = append(order, spec.ID)
			}
			merged[spec.ID] = mergeSpec(prev, spec)
		}
	}

	out := make([]Server, 0, len(order))
	for _, id := range order {
		s := merged[id]
		if s.Disabled {
			continue
		}
		if s.Command == "" && s.URL == "" {
			return nil, fmt.Errorf("dun: server %q has no command or url (declare one, or remove the entry)", id)
		}
		out = append(out, Server{
			ID:        s.ID,
			URL:       s.URL,
			Headers:   requestHeaders(s),
			Command:   s.Command,
			Args:      expandPlaceholders(s.Args, workspace, raglitHome),
			Env:       expandPlaceholders(s.Env, workspace, raglitHome),
			Timeout:   s.Timeout,
			Autostart: s.Autostart != nil && *s.Autostart,
			Disable:   s.Disable,
		})
	}
	return out, nil
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
		f, err := readServersFile(path)
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
func mergeSpec(prev, next ServerSpec) ServerSpec {
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
	return out
}

// requestHeaders resolves a remote server's headers: Headers, plus
// Authorization from Bearer with any $NAME read from the environment.
func requestHeaders(s ServerSpec) map[string]string {
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
	return filepath.Join(filepath.Dir(SessionsDir()), "mcp.json")
}

// PutUserServer adds spec to the user server list, replacing one of the same
// id. It returns the file written.
func PutUserServer(spec ServerSpec) (string, error) {
	path := UserServersFile()
	f, err := readServersFile(path)
	if err != nil {
		return "", err
	}
	if f == nil {
		f = &ServersFile{}
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

// RemoveUserServer drops id from the user server list; it reports whether
// it was there.
func RemoveUserServer(id string) (bool, error) {
	path := UserServersFile()
	f, err := readServersFile(path)
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

func writeUserServers(path string, f *ServersFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// expandPlaceholders substitutes the paths a config file cannot know.
//
// {{workspace}} is the same token llm-bench's toolsets already use, so anyone
// who has written one of those configs will recognise it.
func expandPlaceholders(in []string, workspace, raglitHome string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		s = strings.ReplaceAll(s, "{{workspace}}", workspace)
		s = strings.ReplaceAll(s, "{{raglit_home}}", raglitHome)
		out[i] = s
	}
	return out
}

func readServersFile(path string) (*ServersFile, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dun: read %s: %w", path, err)
	}
	var f ServersFile
	if err := json.Unmarshal(b, &f); err != nil {
		// Name the file. A JSON error with no filename in a two-file layered
		// config sends the reader to the wrong one.
		return nil, fmt.Errorf("dun: parse %s: %w", path, err)
	}
	return &f, nil
}

// SetAutostart persists whether a server spawns at startup, in dir's state
// directory: $DUN_HOME/projects/<id>/dun.local.json, or dir's overlay when dir
// is a linked worktree. Returns the file it wrote.
//
// Machine state, not project state: whether raglit is installed and worth
// spawning here is a fact about this box. It used to go to dir/.dun/ with a
// .gitignore beside it — a write into a repo that is not dun's to change.
func SetAutostart(dir, id string, on bool) (string, error) {
	if id == "" {
		return "", fmt.Errorf("dun: SetAutostart: empty server id")
	}
	p, err := EnsureProject(dir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(p.WriteDir(), LocalServersFile)
	f, err := readServersFile(path)
	if err != nil {
		return "", err
	}
	if f == nil {
		f = &ServersFile{}
	}
	found := false
	for i := range f.Servers {
		if f.Servers[i].ID == id {
			f.Servers[i].Autostart = &on
			found = true
			break
		}
	}
	if !found {
		f.Servers = append(f.Servers, ServerSpec{ID: id, Autostart: &on})
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", fmt.Errorf("dun: encode %s: %w", path, err)
	}
	if err := writeFileAtomic(path, append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ServerIDs lists the resolved ids, for logs and diagnostics.
func ServerIDs(servers []Server) []string {
	ids := make([]string, 0, len(servers))
	for _, s := range servers {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}
