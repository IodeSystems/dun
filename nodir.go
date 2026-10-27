package dun

import (
	"os"
	"path/filepath"
	"strings"
)

// dun --no-dir (USER 2026-10-27): a session attached to no directory, to
// talk with dun, its skills and its MCP tools. It runs in one directory dun
// keeps for these sessions ($DUN_HOME/nodir, NoDirWorkspace), which is not a
// project: no rules, no file tools, no poly-lsp, raglit without the workspace,
// and no MCP server whose command names the workspace or the project. The
// shell's eval and exec run there; --continue and --resume find these
// sessions there as any workspace's.

// NoDirWorkspace is where --no-dir sessions run, made if missing.
func NoDirWorkspace() (string, error) {
	d := filepath.Join(DunHome(), "nodir")
	return d, os.MkdirAll(d, 0o700)
}

// needsDir reports whether s needs a project directory: poly-lsp and raglit
// (they index one), or a server whose command, arguments or environment
// name the workspace.
func needsDir(s Server, workspace string) bool {
	if s.ID == ServerCode {
		return true
	}
	if s.ID == ServerDocs {
		return false // other indexes, memory and skills; the workspace is not read (ragworkspace.go)
	}
	if s.ID == ServerShell {
		return false // its --files-dir is the session's directory, which it has
	}
	for _, part := range append(append([]string{s.Command}, s.Args...), s.Env...) {
		if workspace != "" && strings.Contains(part, workspace) {
			return true
		}
	}
	return false
}

// withoutDirServers is servers less those that need a directory, for a
// --no-dir session; servers as they are otherwise.
func withoutDirServers(servers []Server, cfg Config) []Server {
	if cfg.NoTools {
		return nil // --no-tools: no server starts (see also noToolsSystem)
	}
	if !cfg.NoDir {
		return servers
	}
	out := servers[:0:0]
	for _, s := range servers {
		if !needsDir(s, cfg.Workspace) {
			out = append(out, s)
		}
	}
	return out
}

// noDirSystem is the system prompt's note for a --no-dir session.
const noDirSystem = "\n\nThis session is not attached to a project directory: there are no project files to read " +
	"or change, and no code or document index. Answer from what you know, the tools you have, and what the user " +
	"tells or pastes; commands run in a scratch directory."
