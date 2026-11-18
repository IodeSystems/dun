package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/internal/update"
	"github.com/iodesystems/dun/release"
)

// Where a server's command comes from. dun manages its own tools
// (release.Supported: mcpshell, poly-lsp-mcp, raglit) under DUN_HOME (USER
// 2026-11-18, docs/decisions.md):
//
//	an override      $DUN_MCPSHELL ($DUN_POLY_LSP_MCP, $DUN_RAGLIT), else
//	                 tools.<name>.path in $DUN_HOME/mcp.json (env wins, as
//	                 env does over every config file); a server entry whose
//	                 command is a path is used as it is, before either
//	the pinned copy  $DUN_HOME/tools/<name>/<version>/<goos>-<goarch>/<name>, the version
//	                 this dun pins (PinVersion); when that one is not
//	                 there, the newest kept copy at or past the tool's Min
//	not installed    a MissingError
//
// The user's PATH and dun's own directory are not searched for these: a
// copy found there was whatever the shell had first (a Mac ran a stale
// mcpshell checkout from ~/.local/bin for weeks). A release build fetches
// a missing pinned copy the first time a session needs it (EnsureCommand);
// any other build does not, and `dun doctor --fix` go-installs it there.
//
// Every other command (a language server, ant) is looked for on PATH, then
// beside dun's executable.
//
// A command that is not installed is a setup step, not a failure: the rest
// of dun works without it.

// ToolInstall is how to get the commands dun's own servers run: each is a Go
// module of the iodesystems project (go install builds it from source, checked
// against the Go checksum database — no binary from elsewhere), and what a
// session lacks without it. Module carries the version this dun was tested
// with (release.Supported), never @latest: the proxy caches @latest, and a
// cached one can be a build that no longer works.
var ToolInstall = map[string]struct{ Module, Lost string }{
	"mcpshell":     {pinned("mcpshell"), "no eval tool (sandboxed calculations, data, SQL)"},
	"poly-lsp-mcp": {pinned("poly-lsp-mcp"), "no semantic code tools (/lsp)"},
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

// MissingError is a server command that is not installed: a managed tool
// with no override and no pinned copy, or another command on neither PATH
// nor beside dun. Why, when set, is the reason (an override that is not an
// executable, a fetch that failed). OnPath is a copy of a managed tool
// on PATH, which dun no longer runs: said, with how to use it.
type MissingError struct{ Command, Why, OnPath string }

func (e *MissingError) Error() string {
	msg := e.Command + " is not installed"
	if e.Why != "" {
		msg += " (" + e.Why + ")"
	}
	if t, ok := ToolInstall[e.Command]; ok {
		msg += ": " + t.Lost
	}
	msg += " — dun doctor says how to get it"
	if e.OnPath != "" {
		msg += "; " + e.OnPath + " on PATH is not used (set " + ToolEnv(e.Command) + " or tools." + e.Command + ".path to use it)"
	}
	return msg
}

// missingTool is managed tool name's MissingError, naming a copy on PATH.
func missingTool(name, why string) *MissingError {
	e := &MissingError{Command: name, Why: why}
	if p, err := exec.LookPath(name); err == nil {
		e.OnPath = p
	}
	return e
}

// Tool sources (Resolved.Source).
const (
	SourcePinned   = "pinned"
	SourceOverride = "override"
	SourceMissing  = "missing"
)

// Resolved is where a managed tool comes from, without fetching anything.
type Resolved struct {
	Name   string `json:"name"`
	Source string `json:"source"`         // SourcePinned, SourceOverride or SourceMissing
	Path   string `json:"path,omitempty"` // "" when missing
	// Via is the override's origin: "$DUN_MCPSHELL" or "tools.mcpshell.path
	// in <file>".
	Via string `json:"via,omitempty"`
	Pin string `json:"pin,omitempty"` // the version this dun pins
	// Version is a pinned copy's version (its directory): Pin, or an older
	// kept copy's when the pin's is not there.
	Version string `json:"version,omitempty"`
	Why     string `json:"why,omitempty"` // missing: why, when it is more than absent
}

// Stale is a pinned copy that is a kept older one, not the pin.
func (r Resolved) Stale() bool { return r.Source == SourcePinned && r.Version != r.Pin }

// IsManaged reports whether name is one of the tools dun manages.
func IsManaged(name string) bool {
	_, ok := release.SupportedTool(name)
	return ok
}

// ToolEnv is name's override variable: DUN_MCPSHELL, DUN_POLY_LSP_MCP,
// DUN_RAGLIT.
func ToolEnv(name string) string {
	return "DUN_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// ToolsDir is where the pinned copies are: $DUN_HOME/tools.
func ToolsDir() string { return filepath.Join(dunHomeDir(), "tools") }

// PinnedPath is name's copy at version for this machine:
// $DUN_HOME/tools/<name>/<version>/<goos>-<goarch>/<name> (a DUN_HOME on a
// shared or synced disk can hold several platforms' copies).
func PinnedPath(name, version string) string {
	return filepath.Join(ToolsDir(), name, version, runtime.GOOS+"-"+runtime.GOARCH, name)
}

// ToolConfig is one entry of the `tools` section of $DUN_HOME/mcp.json.
type ToolConfig struct {
	// Path is the binary dun runs for the tool instead of its pinned copy:
	// absolute, or ~/….
	Path string `json:"path,omitempty"`
}

// ResolveTool is where managed tool name comes from, read-only: doctor,
// dun tools and dun ps show it; nothing is fetched.
func ResolveTool(name string) Resolved {
	r := Resolved{Name: name, Pin: PinVersion(name)}
	if p, via := toolOverride(name); p != "" {
		r.Source, r.Via = SourceOverride, via
		if !isExec(p) {
			r.Source, r.Why = SourceMissing, via+" is "+p+", which is not an executable file"
			return r
		}
		r.Path = p
		return r
	}
	if p := PinnedPath(name, r.Pin); r.Pin != "" && isExec(p) {
		r.Source, r.Path, r.Version = SourcePinned, p, r.Pin
		return r
	}
	if v := keptVersion(name); v != "" {
		r.Source, r.Path, r.Version = SourcePinned, PinnedPath(name, v), v
		return r
	}
	r.Source = SourceMissing
	return r
}

// toolOverride is name's override and where it is set, or "": the
// environment first, then $DUN_HOME/mcp.json (a project's dun.json does
// not pick a machine's binaries, as with default_via).
func toolOverride(name string) (path, via string) {
	env := ToolEnv(name)
	if p := os.Getenv(env); p != "" {
		return expandHome(p), "$" + env
	}
	file := UserServersFile()
	f, err := ReadConfig(file)
	if err != nil || f == nil {
		return "", ""
	}
	if t, ok := f.Tools[name]; ok && t.Path != "" {
		return expandHome(t.Path), "tools." + name + ".path in " + file
	}
	return "", ""
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// keptVersion is the newest kept copy of name at or past its Min, or "".
// Old version directories are kept (no prune yet), so a pin raised by the
// registry while offline, or on a build that does not fetch, still runs
// the copy before it.
func keptVersion(name string) string {
	s, ok := SupportedTool(name)
	if !ok {
		return ""
	}
	ents, err := os.ReadDir(filepath.Join(ToolsDir(), name))
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range ents {
		v := e.Name()
		if !e.IsDir() || !semver.IsValid(v) || semver.Compare(v, s.Min) < 0 || !isExec(PinnedPath(name, v)) {
			continue
		}
		if best == "" || semver.Compare(v, best) > 0 {
			best = v
		}
	}
	return best
}

func isExec(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// findTimeout bounds a fetch FindCommand starts for a caller with no ctx.
const findTimeout = 2 * time.Minute

// FindCommand resolves a server's command: a path as it is; a managed tool
// as EnsureCommand does, saying a fetch in the log, for at most
// findTimeout; any other bare name on PATH, then beside this executable.
// A caller with a ctx uses FindCommandContext.
func FindCommand(name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), findTimeout)
	defer cancel()
	return EnsureCommand(ctx, name, nil)
}

// FindCommandContext is FindCommand under ctx: a fetch, or the wait for
// another dun's, stops when ctx ends.
func FindCommandContext(ctx context.Context, name string) (string, error) {
	return EnsureCommand(ctx, name, nil)
}

// EnsureCommand is FindCommand for a session that needs the command now: a
// managed tool whose pinned copy is missing (or a kept older one) is
// fetched first on a release build (FetchPinned), each step said through
// say (nil: the log). A fetch that fails leaves the kept copy, else a
// MissingError with the reason; it never fails the session.
func EnsureCommand(ctx context.Context, name string, say func(string)) (string, error) {
	if strings.ContainsRune(name, '/') {
		return name, nil
	}
	if !IsManaged(name) {
		return findOnPath(name)
	}
	r := ResolveTool(name)
	switch {
	case r.Source == SourceOverride:
		return r.Path, nil
	case r.Source == SourcePinned && !r.Stale():
		return r.Path, nil
	case r.Via != "": // a broken override is the person's to fix, not dun's to paper over
		return "", &MissingError{Command: name, Why: r.Why}
	}
	if say == nil {
		say = func(s string) { log.Printf("dun: %s", s) }
	}
	p, err := FetchPinned(ctx, name, say)
	switch {
	case err == nil:
		return p, nil
	case r.Source == SourcePinned: // a kept copy still runs
		return r.Path, nil
	case errors.Is(err, ErrNotPinned):
		return "", missingTool(name, "")
	}
	return "", missingTool(name, textfmt.OneLine(err.Error()))
}

// findOnPath is a command that is not managed: on PATH, then beside dun.
func findOnPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), name); isExec(p) {
			return p, nil
		}
	}
	return "", &MissingError{Command: name}
}

// ErrNotPinned is a tool this build has no download for: any build but a
// release, or a tool its manifest lacks for this platform.
var ErrNotPinned = errors.New("not in this release's tools manifest")

// Test seams: whether this is a release build, its pinned manifest, and
// the downloader (httptest serves plain HTTP).
var (
	releaseBuild = buildinfo.IsRelease
	pinnedDeps   = func() []byte { return release.Deps }
	toolFetcher  = &update.Fetcher{}
)

// fetchTimeout bounds one tool download (the archives are 15-40 MB).
const fetchTimeout = 10 * time.Minute

// PinnedDownload is name's download for this platform: the tools manifest
// this release build was made with (release/deps.json), or the registry's
// newer pin when it has this platform's file. false on any other build.
func PinnedDownload(name string) (update.Artifact, update.File, bool) {
	if !releaseBuild() {
		return update.Artifact{}, update.File{}, false
	}
	plat := update.Platform(runtime.GOOS, runtime.GOARCH)
	m, err := update.ParseManifest(pinnedDeps())
	if err != nil {
		return update.Artifact{}, update.File{}, false
	}
	a, f, ok := m.File(name, plat)
	// A newer pin in the registry (signed by the same key) wins, when it has
	// this platform's file: a tool's point release without a dun release.
	if t, rok := RegistryTool(name); rok {
		if rf, fok := t.Files[plat]; fok && (!ok || semver.Compare(t.Recommended, a.Version) > 0) {
			return update.Artifact{Version: t.Recommended, Files: t.Files}, rf, true
		}
	}
	return a, f, ok
}

// SupportedTool is name's entry in release.Supported, its Min and
// Recommended raised by the registry's pin for this dun (never lowered).
func SupportedTool(name string) (release.Tool, bool) {
	s, ok := release.SupportedTool(name)
	if !ok {
		return s, false
	}
	if t, rok := RegistryTool(name); rok {
		if semver.Compare(t.Min, s.Min) > 0 {
			s.Min = t.Min
		}
		if semver.Compare(t.Recommended, s.Recommended) > 0 {
			s.Recommended = t.Recommended
		}
	}
	return s, true
}

// PinVersion is the version this dun pins for name: the release download,
// else the version it supports (Recommended, raised by the registry).
func PinVersion(name string) string {
	if a, _, ok := PinnedDownload(name); ok {
		return a.Version
	}
	if s, ok := SupportedTool(name); ok {
		return s.Recommended
	}
	return ""
}

// FetchPinned downloads name's pinned release into PinnedPath, its sha256
// and size checked against the pin, and returns the path. Concurrent
// sessions and dund are safe: a lock file per tool, and the binary is
// written to a temp file and renamed into place (update.Fetcher.Install),
// so a second fetcher waits and then finds it there. say gets "fetching …"
// and the outcome; it is not called when the copy is already there.
func FetchPinned(ctx context.Context, name string, say func(string)) (string, error) {
	a, f, ok := PinnedDownload(name)
	if !ok {
		return "", ErrNotPinned
	}
	dst := PinnedPath(name, a.Version)
	if isExec(dst) {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	// The wait for another dun's fetch ends with ctx (a session closed, the
	// server switched off).
	unlock, err := fsutil.LockFileContext(ctx, filepath.Join(ToolsDir(), name, ".lock"))
	if err != nil {
		return "", err
	}
	defer unlock()
	if isExec(dst) { // another dun fetched it while this one waited
		return dst, nil
	}
	if say == nil {
		say = func(string) {}
	}
	say(fmt.Sprintf("fetching %s %s …", name, a.Version))
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	if _, err := toolFetcher.Install(ctx, f, name, dst); err != nil {
		say(fmt.Sprintf("fetching %s %s failed: %s — dun doctor says how to get it", name, a.Version, textfmt.OneLine(err.Error())))
		return "", fmt.Errorf("fetch %s %s: %w", name, a.Version, err)
	}
	say(fmt.Sprintf("fetched %s %s", name, a.Version))
	return dst, nil
}
