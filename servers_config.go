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
//	dun.local.json  machine-level, GITIGNORED. "on THIS box the binary is at
//	                /opt/bin/x and the DSN is ...". Belongs to the machine, and
//	                may hold secrets, so it is written 0600 and never committed.
//
// Precedence matches what dun already documents for its LLM settings
// (flag > env > file > default), extended down the middle:
//
//	built-in DefaultServers  <  dun.json  <  dun.local.json  <  Config.Servers (Go)
//
// Servers merge BY ID rather than replacing the list wholesale. Overriding one
// binary's path should not require restating the other two — that is how a
// config drifts out of sync with the defaults it silently forked.
//
// Both files are looked for in the workspace root AND in a .dun/ directory
// beside it, .dun/ winning. New state dun writes itself (see SetAutostart) goes
// to .dun/dun.local.json: per-workspace machine state belongs in one directory
// rather than scattered dotfiles, and that directory is where per-session state
// will live once one workspace runs several sessions.
const (
	// ProjectServersFile is committed and describes the project.
	ProjectServersFile = "dun.json"
	// LocalServersFile is gitignored and describes this machine.
	LocalServersFile = "dun.local.json"
	// DunDir is the per-workspace state directory.
	DunDir = ".dun"
)

// serverConfigPaths lists the layered config files, LOWEST precedence first.
// A missing file is skipped; the root-level pair is the pre-.dun/ layout and is
// still honored so an existing checkout keeps working.
func serverConfigPaths(dir string) []string {
	return []string{
		filepath.Join(dir, ProjectServersFile),
		filepath.Join(dir, DunDir, ProjectServersFile),
		filepath.Join(dir, LocalServersFile),
		filepath.Join(dir, DunDir, LocalServersFile),
	}
}

// ServerSpec is one MCP server as declared in a config file.
type ServerSpec struct {
	// ID is the merge key and the tool namespace. Reusing a built-in id
	// ("code", "shell", "docs") overrides that server rather than adding one.
	ID      string   `json:"id"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
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
}

// ServersFile is the on-disk shape of dun.json / dun.local.json.
type ServersFile struct {
	Servers []ServerSpec `json:"servers,omitempty"`
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
		merged[s.ID] = ServerSpec{ID: s.ID, Command: s.Command, Args: s.Args, Autostart: &auto}
		order = append(order, s.ID)
	}

	for _, path := range serverConfigPaths(dir) {
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
		if s.Command == "" {
			return nil, fmt.Errorf("dun: server %q has no command (declare one, or remove the entry)", id)
		}
		out = append(out, Server{
			ID:        s.ID,
			Command:   s.Command,
			Args:      expandPlaceholders(s.Args, workspace, raglitHome),
			Env:       expandPlaceholders(s.Env, workspace, raglitHome),
			Timeout:   s.Timeout,
			Autostart: s.Autostart != nil && *s.Autostart,
		})
	}
	return out, nil
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
	return out
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

// SetAutostart persists whether a server spawns at startup, in
// dir/.dun/dun.local.json. Returns the file it wrote.
//
// Machine state, not project state: whether raglit is installed and worth
// spawning here is a fact about this box. The .dun directory is created 0700
// and given a .gitignore covering the local file, so turning a server on never
// leaves something to accidentally commit.
func SetAutostart(dir, id string, on bool) (string, error) {
	if id == "" {
		return "", fmt.Errorf("dun: SetAutostart: empty server id")
	}
	dunDir := filepath.Join(dir, DunDir)
	if err := os.MkdirAll(dunDir, 0o700); err != nil {
		return "", fmt.Errorf("dun: create %s: %w", dunDir, err)
	}
	if err := ensureDunGitignore(dunDir); err != nil {
		return "", err
	}
	path := filepath.Join(dunDir, LocalServersFile)
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
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("dun: write %s: %w", path, err)
	}
	return path, nil
}

// ensureDunGitignore keeps the machine-local file out of git. It ignores the
// local file by name rather than the whole directory, so a committed
// .dun/dun.json remains possible.
func ensureDunGitignore(dunDir string) error {
	path := filepath.Join(dunDir, ".gitignore")
	b, err := os.ReadFile(path)
	if err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == LocalServersFile {
				return nil
			}
		}
		body := string(b)
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		return os.WriteFile(path, []byte(body+LocalServersFile+"\n"), 0o644)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("dun: read %s: %w", path, err)
	}
	return os.WriteFile(path, []byte(LocalServersFile+"\n"), 0o644)
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
