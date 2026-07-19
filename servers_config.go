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
const (
	// ProjectServersFile is committed and describes the project.
	ProjectServersFile = "dun.json"
	// LocalServersFile is gitignored and describes this machine.
	LocalServersFile = "dun.local.json"
)

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
		merged[s.ID] = ServerSpec{ID: s.ID, Command: s.Command, Args: s.Args}
		order = append(order, s.ID)
	}

	for _, name := range []string{ProjectServersFile, LocalServersFile} {
		path := filepath.Join(dir, name)
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
			ID:      s.ID,
			Command: s.Command,
			Args:    expandPlaceholders(s.Args, workspace, raglitHome),
			Env:     expandPlaceholders(s.Env, workspace, raglitHome),
			Timeout: s.Timeout,
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

// ServerIDs lists the resolved ids, for logs and diagnostics.
func ServerIDs(servers []Server) []string {
	ids := make([]string, 0, len(servers))
	for _, s := range servers {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}
