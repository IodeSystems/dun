package dun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	filetools "github.com/iodesystems/agentkit/files"
)

// /file (USER 2026-10-27, scoped 2026-10-28): files and directories outside
// the workspace that dun's file tools may use, switched like /mcp's servers.
// Each scope — global ($DUN_HOME/paths.json), project (its state dir's
// paths.json), session (<session>.paths.json) — may name a path, and for it:
//
//	on     whether it is open (/file off turns it off at a scope)
//	mode   "read" (the default) or "write": a read-only path refuses edits
//	index  whether a directory is searchable (the default): raglit watches it
//	       into an index of its own, switched on in /rag at the same scope
//
// The nearest scope that sets a field decides it: session, then project,
// then global. A file grants that file alone, not its directory. Asked for
// one it has not been given, a file tool answers the model with the command
// for the person to run, and the person sees it on the call's row.
//
// The session's grants used to be a list in <session>.files.json, all of
// them read and write; it is read once into the session's paths.json.

const pathsKind = "paths"

// PathEntry is one scope's say about a path; nil and "" are unset.
type PathEntry struct {
	On    *bool  `json:"on,omitempty"`
	Mode  string `json:"mode,omitempty"`  // "read" or "write"
	Index *bool  `json:"index,omitempty"` // a directory: raglit watches it
	// IndexName is the raglit index it was watched into ("project:name"),
	// once it was.
	IndexName string `json:"index_name,omitempty"`
}

type pathsFile struct {
	Paths map[string]PathEntry `json:"paths,omitempty"`
	// Workspace is this scope's mode for the session's own working tree,
	// "read" or "write" ("" unset): the /file table's first row (USER
	// 2026-10-28). Unset at every scope, dun.json's files.read_only decides.
	Workspace string `json:"workspace,omitempty"`
}

// AddedPath is a path as the scopes decide it.
type AddedPath struct {
	Path      string
	On        bool
	Write     bool
	Index     bool
	IndexName string
	From      string            // the scope that decided On
	Scopes    map[string]string // scope → "on", "off" or ""
	Dir       bool
}

func (h *Harness) pathsFile(scope string) string { return h.switchFile(pathsKind, scope) }

func (h *Harness) loadPaths(scope string) map[string]PathEntry { return h.loadPathsFile(scope).Paths }

func (h *Harness) loadPathsFile(scope string) pathsFile {
	if scope == ScopeSession {
		h.migrateAdded()
	}
	var f pathsFile
	if p := h.pathsFile(scope); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &f)
		}
	}
	if f.Paths == nil {
		f.Paths = map[string]PathEntry{}
	}
	return f
}

// workspaceWrite is whether the file tools may change the working tree, and
// the scope that decided it ("" : dun.json's files.read_only, or the default).
func (h *Harness) workspaceWrite() (write bool, from string) {
	for _, sc := range []string{ScopeSession, ScopeProject, ScopeGlobal} {
		if p := h.pathsFile(sc); p == "" {
			continue
		}
		if m := h.loadPathsFile(sc).Workspace; m != "" {
			return m == "write", sc
		}
	}
	c := h.cfg.Files
	return c == nil || c.ReadOnly == nil || !*c.ReadOnly, ""
}

// SetWorkspaceMode sets the working tree's mode at scope: "read", "write",
// or "" to unset it there.
func (h *Harness) SetWorkspaceMode(scope, mode string) (string, error) {
	if mode != "read" && mode != "write" && mode != "" {
		return "", fmt.Errorf("mode %q: want read or write", mode)
	}
	if err := h.editPathsFile(scope, func(f *pathsFile) { f.Workspace = mode }); err != nil {
		return "", err
	}
	h.applyTools()
	write, from := h.workspaceWrite()
	what := map[bool]string{true: "read and write", false: "read only"}[write]
	if from == "" {
		from = "dun.json"
	} else {
		from = scopeWords(from)
	}
	return fmt.Sprintf("files: the workspace %s is %s (decided %s)", h.cfg.Workspace, what, from), nil
}

// isWorkspace reports whether p (as typed) names the session's working tree.
func (h *Harness) isWorkspace(p string) bool {
	p = strings.TrimSpace(p)
	if p == "." || p == "./" {
		return true
	}
	abs, err := cleanAdded(p)
	return err == nil && h.cfg.Workspace != "" && abs == filepath.Clean(h.cfg.Workspace)
}

func (h *Harness) editPaths(scope string, edit func(map[string]PathEntry)) error {
	p := h.pathsFile(scope)
	if p == "" {
		return fmt.Errorf("there is no %s scope here (an in-memory session)", scope)
	}
	return h.editPathsFile(scope, func(f *pathsFile) { edit(f.Paths) })
}

func (h *Harness) editPathsFile(scope string, edit func(*pathsFile)) error {
	p := h.pathsFile(scope)
	if p == "" {
		return fmt.Errorf("there is no %s scope here (an in-memory session)", scope)
	}
	f := h.loadPathsFile(scope)
	edit(&f)
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(p, append(b, '\n'), 0o600)
}

// migrateAdded reads the old <session>.files.json list into the session's
// paths.json, as read-and-write and unindexed (what it granted), once.
func (h *Harness) migrateAdded() {
	if h.cfg.SessionFile == "" {
		return
	}
	old := strings.TrimSuffix(h.cfg.SessionFile, ".jsonl") + ".files.json"
	b, err := os.ReadFile(old)
	if err != nil {
		return
	}
	var list []string
	if json.Unmarshal(b, &list) != nil {
		return
	}
	p := h.pathsFile(ScopeSession)
	var f pathsFile
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	if f.Paths == nil {
		f.Paths = map[string]PathEntry{}
	}
	on, no := true, false
	for _, path := range list {
		if _, ok := f.Paths[path]; !ok {
			f.Paths[path] = PathEntry{On: &on, Mode: "write", Index: &no}
		}
	}
	nb, _ := json.MarshalIndent(f, "", "  ")
	if writeFileAtomic(p, append(nb, '\n'), 0o600) == nil {
		_ = os.Remove(old)
	}
}

// AddedEntries is every path a scope names, decided, sorted by path.
func (h *Harness) AddedEntries() []AddedPath {
	layers := []struct {
		scope string
		m     map[string]PathEntry
	}{{ScopeSession, h.loadPaths(ScopeSession)}, {ScopeProject, h.loadPaths(ScopeProject)}, {ScopeGlobal, h.loadPaths(ScopeGlobal)}}
	names := map[string]bool{}
	for _, l := range layers {
		for p := range l.m {
			names[p] = true
		}
	}
	var out []AddedPath
	for p := range names {
		a := AddedPath{Path: p, Index: true, Scopes: map[string]string{}}
		onSet, modeSet, idxSet := false, false, false
		for _, l := range layers {
			e, ok := l.m[p]
			if !ok {
				continue
			}
			if e.On != nil {
				a.Scopes[l.scope] = map[bool]string{true: "on", false: "off"}[*e.On]
				if !onSet {
					a.On, a.From, onSet = *e.On, l.scope, true
				}
			}
			if e.Mode != "" && !modeSet {
				a.Write, modeSet = e.Mode == "write", true
			}
			if e.Index != nil && !idxSet {
				a.Index, idxSet = *e.Index, true
			}
			if a.IndexName == "" {
				a.IndexName = e.IndexName
			}
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			a.Dir = true
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// AddedPaths is the paths open to the file tools, sorted.
func (h *Harness) AddedPaths() []string {
	var out []string
	for _, a := range h.AddedEntries() {
		if a.On {
			out = append(out, a.Path)
		}
	}
	return out
}

// PathOptions is /file add's flags.
type PathOptions struct {
	Write   bool
	NoIndex bool
}

// AddPath opens p at scope: on, read (or write), indexed unless NoIndex.
func (h *Harness) AddPath(p, scope string, o PathOptions) (string, error) {
	abs, err := cleanAdded(p)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s: %v", p, err)
	}
	on := true
	mode := "read"
	if o.Write {
		mode = "write"
	}
	idx := !o.NoIndex && st.IsDir()
	if err := h.editPaths(scope, func(m map[string]PathEntry) {
		e := m[abs]
		e.On, e.Mode, e.Index = &on, mode, &idx
		m[abs] = e
	}); err != nil {
		return "", err
	}
	h.applyTools()
	what := "the file"
	if st.IsDir() {
		what = "the directory, and everything under it,"
	}
	msg := fmt.Sprintf("files: %s — %s open to dun's file tools for %s, %s", abs, strings.TrimPrefix(what, "the "), scopeWords(scope),
		map[bool]string{true: "read and write", false: "read only (/file mode " + abs + " write lets dun change it)"}[o.Write])
	if st.IsDir() {
		msg += " · " + h.indexPath(abs, scope, idx)
	}
	return msg + " · /file remove " + abs + " takes it back", nil
}

// RemovePath unsets p at scope.
func (h *Harness) RemovePath(p, scope string) (string, error) {
	abs, err := cleanAdded(p)
	if err != nil {
		return "", err
	}
	had := false
	name := ""
	if err := h.editPaths(scope, func(m map[string]PathEntry) {
		var e PathEntry
		e, had = m[abs]
		name = e.IndexName
		delete(m, abs)
	}); err != nil {
		return "", err
	}
	if !had {
		return "", fmt.Errorf("%s is not added for %s (/file lists them)", abs, scopeWords(scope))
	}
	if name != "" {
		_, _, _, _ = h.SetIndex(context.Background(), ServerDocs, name, scope, nil)
	}
	h.applyTools()
	return "files: " + abs + " taken back for " + scopeWords(scope), nil
}

// SetPathOn turns p on or off at scope (off keeps its other settings).
func (h *Harness) SetPathOn(p, scope string, on bool) (string, error) {
	abs, err := cleanAdded(p)
	if err != nil {
		return "", err
	}
	if err := h.editPaths(scope, func(m map[string]PathEntry) {
		e := m[abs]
		e.On = &on
		m[abs] = e
	}); err != nil {
		return "", err
	}
	h.applyTools()
	return fmt.Sprintf("files: %s %s for %s", abs, map[bool]string{true: "on", false: "off"}[on], scopeWords(scope)), nil
}

// SetPathMode sets p's mode, "read" or "write", at scope.
func (h *Harness) SetPathMode(p, scope, mode string) (string, error) {
	if mode != "read" && mode != "write" {
		return "", fmt.Errorf("mode %q: want read or write", mode)
	}
	if h.isWorkspace(p) {
		return h.SetWorkspaceMode(scope, mode)
	}
	abs, err := cleanAdded(p)
	if err != nil {
		return "", err
	}
	if err := h.editPaths(scope, func(m map[string]PathEntry) {
		e := m[abs]
		e.Mode = mode
		m[abs] = e
	}); err != nil {
		return "", err
	}
	h.applyTools()
	return fmt.Sprintf("files: %s is %s for %s", abs, map[string]string{"read": "read only", "write": "read and write"}[mode], scopeWords(scope)), nil
}

// SetPathIndex sets whether directory p is indexed, at scope.
func (h *Harness) SetPathIndex(p, scope string, on bool) (string, error) {
	abs, err := cleanAdded(p)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s: only a directory is indexed", abs)
	}
	if err := h.editPaths(scope, func(m map[string]PathEntry) {
		e := m[abs]
		e.Index = &on
		m[abs] = e
	}); err != nil {
		return "", err
	}
	return "files: " + abs + " · " + h.indexPath(abs, scope, on), nil
}

// indexPath watches directory abs into its own raglit index and switches it
// on in /rag at scope, or switches it off there. It says what happened; a
// failure to index does not take the path back.
func (h *Harness) indexPath(abs, scope string, on bool) string {
	var name string
	for _, a := range h.AddedEntries() {
		if a.Path == abs {
			name = a.IndexName
		}
	}
	if !on {
		if name == "" {
			return "not indexed"
		}
		if _, _, _, err := h.SetIndex(context.Background(), ServerDocs, name, scope, ptrBool(false)); err != nil {
			return "not indexed (" + err.Error() + ")"
		}
		return "not searched: " + name + " off in /rag for " + scopeWords(scope) + " (its index is kept)"
	}
	if name == "" {
		idx, _, err := h.WatchDir(context.Background(), ServerDocs, abs, pathIndexName(abs))
		if err != nil {
			return "not indexed: " + err.Error()
		}
		if s, err := h.knownServer(ServerDocs); err == nil {
			name = h.ownProject(s) + ":" + idx
		}
		_ = h.editPaths(scope, func(m map[string]PathEntry) {
			e := m[abs]
			e.IndexName = name
			m[abs] = e
		})
	}
	if _, _, _, err := h.SetIndex(context.Background(), ServerDocs, name, scope, ptrBool(true)); err != nil {
		return "indexed as " + name + ", but not switched on: " + err.Error()
	}
	return "indexed as " + name + " (raglit keeps it fresh), on in /rag for " + scopeWords(scope)
}

// pathIndexName is the index a directory is watched into: its name and a
// short hash of its path, so two directories of one name do not share one.
func pathIndexName(abs string) string {
	sum := sha256.Sum256([]byte(abs))
	return "file-" + normalizeProject(filepath.Base(abs)) + "-" + hex.EncodeToString(sum[:3])
}

func ptrBool(b bool) *bool { return &b }

func cleanAdded(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("which file or directory? /file add <path>")
	}
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, p[2:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if abs == "/" {
		return "", fmt.Errorf("not the whole filesystem")
	}
	return abs, nil
}

// addedWriters is a toolset per open directory, and one per directory
// holding open files, held to those files; configured like the workspace's.
// readOnly marks the ones whose paths are open for reading only: an edit or
// a write routed to one is refused (withFiles).
func (h *Harness) addedWriters() (out []*filetools.Toolset, readOnly map[*filetools.Toolset]bool) {
	readOnly = map[*filetools.Toolset]bool{}
	cfg, ok := h.cfg.Files.toolsetConfig(h.cfg.Workspace)
	if !ok {
		return nil, readOnly
	}
	type fileSet struct {
		files map[string]bool
		write bool
	}
	var dirs []AddedPath
	files := map[string]*fileSet{} // directory → its open files
	for _, a := range h.AddedEntries() {
		if !a.On {
			continue
		}
		st, err := os.Stat(a.Path)
		switch {
		case err != nil:
		case st.IsDir():
			dirs = append(dirs, a)
		default:
			d := filepath.Dir(a.Path)
			if files[d] == nil {
				files[d] = &fileSet{files: map[string]bool{}}
			}
			files[d].files[a.Path] = true
			files[d].write = files[d].write || a.Write
		}
	}
	add := func(root string, write bool, resolve func(string) (string, error)) {
		c := cfg
		c.ExtraIgnore, c.Root, c.Resolve = nil, root, resolve
		if ts, err := filetools.New(c); err == nil {
			out = append(out, ts)
			readOnly[ts] = !write
		}
	}
	for _, d := range dirs {
		root := d.Path
		add(root, d.Write, func(rel string) (string, error) { return filetools.Under(root, rel) })
	}
	for d, set := range files {
		root, allowed := d, set.files
		add(root, set.write, func(rel string) (string, error) {
			got, err := filetools.Under(root, rel)
			if err == nil && !allowed[got] {
				return "", fmt.Errorf("%s: only the files added from %s are open (ask the user: /file add %s)", got, root, got)
			}
			return got, err
		})
	}
	return out, readOnly
}

// outsideHint is the answer to a file call for an absolute path dun has not
// been given: the model is to ask the person for it, with the command.
func outsideHint(ts *filetools.Toolset, name, args string) string {
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(args), &a) != nil || !filepath.IsAbs(strings.TrimSpace(a.Path)) {
		return ""
	}
	p := filepath.Clean(strings.TrimSpace(a.Path))
	if _, err := filetools.Under(ts.Root(), p); err == nil {
		return ""
	}
	dir := p
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		dir = filepath.Dir(p)
	}
	return fmt.Sprintf("ERROR: %s is outside this workspace (%s), and the user has not added it. "+
		"Ask the user to add it: /file add %s (or /file add %s for its directory). Do not work around this with exec.",
		p, ts.Root(), p, dir)
}

// pathsSwitchKind is /file's table: every path a scope names, on, off or
// unset at each scope; its mode and index in the row.
func (h *Harness) pathsSwitchKind() switchKind {
	return switchKind{name: pathsKind, slash: "file", view: h.pathsView, set: h.setPathSwitch}
}

func (h *Harness) pathsView() SwitchView {
	v := SwitchView{Kind: pathsKind, Slash: "file", Modes: map[string]string{}}
	// The working tree first: always open, its mode switchable.
	if ws := h.cfg.Workspace; ws != "" {
		write, from := h.workspaceWrite()
		src := map[bool]string{true: "read and write", false: "read"}[write] + " · this workspace, always open"
		if from == "" {
			src += " · mode from dun.json"
		}
		v.Items = append(v.Items, SwitchState{SwitchItem: SwitchItem{Name: filepath.Clean(ws), Source: src}, On: true, From: from})
	}
	for _, a := range h.AddedEntries() {
		src := "read"
		if a.Write {
			src = "read and write"
		}
		if a.Dir {
			if a.Index {
				src += " · indexed"
				if a.IndexName != "" {
					src += " (" + a.IndexName + ")"
				}
			} else {
				src += " · not indexed"
			}
		} else {
			src += " · a file"
		}
		v.Items = append(v.Items, SwitchState{
			SwitchItem: SwitchItem{Name: a.Path, Source: src},
			On:         a.On, From: a.From,
			Global: a.Scopes[ScopeGlobal], Project: a.Scopes[ScopeProject], Session: a.Scopes[ScopeSession],
		})
	}
	v.Status = []string{"/file add <path> [--global|--project|--session] [--write] [--no-index] · /file mode <path> read|write · /file index <path> on|off",
		"the nearest scope that says decides · ctrl+g/p/s on → off → unset"}
	return v
}

// setPathSwitch is the table's switch on a path at scope: on, off, or unset
// (nil: the scope no longer names it).
func (h *Harness) setPathSwitch(name, scope string, on *bool) (string, error) {
	if h.isWorkspace(name) {
		return "", fmt.Errorf("the workspace is always open — ctrl+w, or /file mode . read|write, sets what the tools may do in it")
	}
	if on == nil {
		return h.RemovePath(name, scope)
	}
	return h.SetPathOn(name, scope, *on)
}
