package dun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/iodesystems/dun/release"
)

// A server whose command is not installed is a setup step, not a failure:
// a binary-only install has no mcpshell until someone puts it there, and the
// rest of dun works without it. dun looks for a server's command on PATH,
// then beside its own executable (so dun and its tools copied into one
// directory need no PATH entry), and a command in neither place is a
// MissingError: what the session lacks, and where the install step is.

// ToolInstall is how to get the commands dun's own servers run: each is a Go
// module of the iodesystems project (go install builds it from source, checked
// against the Go checksum database — no binary from elsewhere), and what a
// session lacks without it. Module carries the version this dun was tested
// with (release.Supported), never @latest: the proxy caches @latest, and a
// cached one can be a build that no longer works.
var ToolInstall = map[string]struct{ Module, Lost string }{
	"mcpshell":     {pinned("mcpshell"), "no eval tool (sandboxed calculations, data, SQL)"},
	"poly-lsp-mcp": {pinned("poly-lsp-mcp"), "no semantic code tools (/poly-lsp)"},
	"raglit":       {pinned("raglit"), "no docs search or memories (/rag)"},
}

// pinned is name's module at its recommended version.
func pinned(name string) string {
	t, ok := release.SupportedTool(name)
	if !ok {
		panic("dun: " + name + " is not in release.Supported")
	}
	return t.Module + "@" + t.Recommended
}

// MissingError is a server command found neither on PATH nor beside dun.
type MissingError struct{ Command string }

func (e *MissingError) Error() string {
	msg := e.Command + " is not installed"
	if t, ok := ToolInstall[e.Command]; ok {
		msg += ": " + t.Lost
	}
	return msg + " — dun doctor says how to get it"
}

// FindCommand resolves a server's command: a path as it is, a bare name on
// PATH, then beside this executable.
func FindCommand(name string) (string, error) {
	if strings.ContainsRune(name, '/') {
		return name, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", &MissingError{Command: name}
}
