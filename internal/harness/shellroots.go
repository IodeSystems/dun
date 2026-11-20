package harness

import (
	"context"
	"encoding/json"
	"log"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/update"
	"golang.org/x/mod/semver"
)

// MCP roots for mcpshell (USER: Files roots "follow file-access";
// docs/decisions.md). Since v0.15.0 mcpshell's Files.* refuse an absolute
// path outside every root. With --files-client-roots it asks its client
// for roots (roots/list) after initialized and again on each
// notifications/roots/list_changed. dun answers with what its own file
// tools may reach beyond the workspace: each /file grant that is on, and
// the session's read dirs (job logs, saved output) read only. The
// workspace is not listed: --files-dir already makes it a root, and a
// second entry would show twice in mcpshell's access-denied error. A grant open
// for writing carries _meta {"mcpshell/access":"write"}; any other root is
// read only to mcpshell.
//
// The shell is the session's own (a built-in, never shared through dund),
// and a child's shell is its root session's: the root answers, from the
// root's grants. An older mcpshell gets neither the flag nor the
// capability, and keeps its one root, --files-dir.

// shellClientRootsMin is the first mcpshell that takes --files-client-roots.
const shellClientRootsMin = "v0.15.0"

const (
	clientRootsFlag  = "--files-client-roots"
	rootsListMethod  = "roots/list"
	rootsChangedNote = "notifications/roots/list_changed"
	rootsAccessMeta  = "mcpshell/access"
)

// rootsCapabilities is what dun declares in initialize to a shell that
// takes client roots.
func rootsCapabilities() map[string]any {
	return map[string]any{"roots": map[string]any{"listChanged": true}}
}

// clientRootsVersion reports whether an mcpshell of version v takes
// --files-client-roots. A version that is not semver ("(devel)", "") says
// nothing, and gets no flag; so does an unstamped v0.0.0 pseudo-version.
func clientRootsVersion(v string) bool {
	return semver.IsValid(v) && semver.Compare(v, shellClientRootsMin) >= 0
}

// clientRootsBinary is clientRootsVersion for the binary at path, read from
// its build info without running it (as doctor reads a tool's version).
// A var for tests.
var clientRootsBinary = func(path string) bool {
	if p, err := mcpserver.FindCommand(path); err == nil {
		path = p
	}
	o, err := update.OfFile(path)
	return err == nil && clientRootsVersion(o.Version)
}

// withClientRoots is mcpshell's args with --files-client-roots, when the
// binary at command takes it and the args name a --files-dir (the flag
// needs one); ok reports whether the shell will ask for roots. A shell
// that is not mcpshell is left alone.
func withClientRoots(s mcpserver.Server, command string, args []string) ([]string, bool) {
	if filepath.Base(s.Command) != "mcpshell" || flagValue(args, "--files-dir") == "" || !clientRootsBinary(command) {
		return args, false
	}
	if hasFlag(args, clientRootsFlag) {
		return args, true
	}
	return append(slices.Clone(args), clientRootsFlag), true
}

// hasFlag reports whether args name flag, as --flag, -flag or --flag=….
func hasFlag(args []string, flag string) bool {
	bare := strings.TrimLeft(flag, "-")
	for _, a := range args {
		if a == "--"+bare || a == "-"+bare || strings.HasPrefix(a, "--"+bare+"=") || strings.HasPrefix(a, "-"+bare+"=") {
			return true
		}
	}
	return false
}

// flagValue is the value args give flag (--flag v, --flag=v, one dash too),
// "" when they give none. An empty --files-dir (a session with no
// workspace) is no workspace root to mcpshell, which then refuses
// --files-client-roots and exits.
func flagValue(args []string, flag string) string {
	bare := strings.TrimLeft(flag, "-")
	for i, a := range args {
		for _, p := range []string{"--" + bare, "-" + bare} {
			if a == p && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, p+"="); ok {
				return v
			}
		}
	}
	return ""
}

// shellRoot is one entry of a roots/list result.
type shellRoot struct {
	URI  string         `json:"uri"`
	Name string         `json:"name,omitempty"`
	Meta map[string]any `json:"_meta,omitempty"`
}

// fileURI is the file:// URI of the absolute path p.
func fileURI(p string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(p)}).String()
}

func newShellRoot(p string, write bool) shellRoot {
	r := shellRoot{URI: fileURI(p), Name: filepath.Base(p)}
	if write {
		r.Meta = map[string]any{rootsAccessMeta: "write"}
	}
	return r
}

// buildShellRoots is the roots list: each grant that is on (write as it
// is), then readDirs, read only. A path is listed once; the workspace
// (--files-dir's root already) is not listed.
func buildShellRoots(workspace string, grants []AddedPath, readDirs []string) []shellRoot {
	out := []shellRoot{}
	seen := map[string]bool{filepath.Clean(workspace): workspace != ""}
	add := func(p string, write bool) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, newShellRoot(p, write))
	}
	for _, g := range grants {
		if g.On {
			add(g.Path, g.Write)
		}
	}
	for _, d := range readDirs {
		add(d, false)
	}
	return out
}

// shellRoots is this session's roots list.
func (h *Harness) shellRoots() []shellRoot {
	return buildShellRoots(h.cfg.Workspace, h.AddedEntries(), h.sessionReadDirs())
}

// answerServerRequest answers a server's request to dun (the manager's
// request handler, set by the harness that owns it). Only the shell is
// asked for roots; anything else is not a method dun has.
func (h *Harness) answerServerRequest(_ context.Context, serverID, method string, _ json.RawMessage) (any, error) {
	if method != rootsListMethod || serverID != mcpserver.ServerShell {
		return nil, mcpmgr.ErrMethodNotFound
	}
	roots := h.shellRoots()
	h.rootsMu.Lock()
	h.rootsSent = rootsKey(roots)
	h.rootsMu.Unlock()
	return map[string]any{"roots": roots}, nil
}

func rootsKey(roots []shellRoot) string {
	b, _ := json.Marshal(roots)
	return string(b)
}

// setShellRoots records whether the running shell asks for roots.
func (h *Harness) setShellRoots(on bool) {
	h.rootsMu.Lock()
	h.rootsOn, h.rootsSent = on, ""
	h.rootsMu.Unlock()
}

// rootsChanged tells the shell its roots changed, when they did since it
// last asked: a /file change, a yes to a file-access question, a workspace
// mode. The shell is the root session's, so a child's change is told
// from the root.
func (h *Harness) rootsChanged() {
	r := h.restartOwner(mcpserver.ServerShell)
	r.rootsMu.Lock()
	on := r.rootsOn
	r.rootsMu.Unlock()
	if !on || !r.mgr.ServerStarted(mcpserver.ServerShell) {
		return
	}
	key := rootsKey(r.shellRoots())
	r.rootsMu.Lock()
	same := key == r.rootsSent
	r.rootsMu.Unlock()
	if same {
		return
	}
	if err := r.mgr.Notify(mcpserver.ServerShell, rootsChangedNote, nil); err != nil {
		log.Printf("dun: shell: %s not sent: %v", rootsChangedNote, err)
	}
}
