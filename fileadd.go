package dun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	filetools "github.com/iodesystems/agentkit/files"
)

// /file add <path> (USER 2026-10-27): a file or directory outside the
// workspace that dun's file tools may read and change, for this session.
// A file grants that file alone, not its directory. Asked for one it has
// not been given, a file tool answers the model with the command for the
// person to run, and the person sees it on the call's row.
//
// Kept in the session's sidecar (<session>.files.json), so a resumed
// session keeps them.

func (h *Harness) addedFile() string {
	if h.cfg.SessionFile == "" {
		return ""
	}
	return strings.TrimSuffix(h.cfg.SessionFile, ".jsonl") + ".files.json"
}

// AddedPaths is the paths added for this session, sorted.
func (h *Harness) AddedPaths() []string {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	if h.added == nil {
		h.added = map[string]bool{}
		if f := h.addedFile(); f != "" {
			var list []string
			if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &list) == nil {
				for _, p := range list {
					h.added[p] = true
				}
			}
		}
	}
	out := make([]string, 0, len(h.added))
	for p := range h.added {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// AddPath grants a file or directory; RemovePath takes it back. Both rebuild
// the tool set and save.
func (h *Harness) AddPath(p string) (string, error) {
	abs, err := cleanAdded(p)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s: %v", p, err)
	}
	h.AddedPaths() // loaded
	h.srvMu.Lock()
	h.added[abs] = true
	h.srvMu.Unlock()
	if err := h.saveAdded(); err != nil {
		return "", err
	}
	h.applyTools()
	what := "the file"
	if st.IsDir() {
		what = "the directory, and everything under it,"
	}
	return fmt.Sprintf("files: %s %s readable and editable by dun's file tools for this session (/file remove %s takes it back)", abs, strings.Replace(what, "the ", "— ", 1), abs), nil
}

func (h *Harness) RemovePath(p string) (string, error) {
	abs, err := cleanAdded(p)
	if err != nil {
		return "", err
	}
	h.AddedPaths()
	h.srvMu.Lock()
	had := h.added[abs]
	delete(h.added, abs)
	h.srvMu.Unlock()
	if !had {
		return "", fmt.Errorf("%s was not added (/file lists them)", abs)
	}
	if err := h.saveAdded(); err != nil {
		return "", err
	}
	h.applyTools()
	return "files: " + abs + " taken back", nil
}

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

func (h *Harness) saveAdded() error {
	f := h.addedFile()
	if f == "" {
		return nil
	}
	b, _ := json.Marshal(h.AddedPaths())
	return writeFileAtomic(f, append(b, '\n'), 0o600)
}

// addedWriters is a writable toolset per added directory, and one per
// directory holding added files, held to those files; configured like the
// workspace's.
func (h *Harness) addedWriters() []*filetools.Toolset {
	cfg, ok := h.cfg.Files.toolsetConfig(h.cfg.Workspace)
	if !ok {
		return nil
	}
	var dirs []string
	files := map[string]map[string]bool{} // directory → its added files
	for _, p := range h.AddedPaths() {
		st, err := os.Stat(p)
		switch {
		case err != nil:
		case st.IsDir():
			dirs = append(dirs, p)
		default:
			d := filepath.Dir(p)
			if files[d] == nil {
				files[d] = map[string]bool{}
			}
			files[d][p] = true
		}
	}
	var out []*filetools.Toolset
	add := func(root string, resolve func(string) (string, error)) {
		c := cfg
		c.ExtraIgnore, c.Root, c.Resolve = nil, root, resolve
		if ts, err := filetools.New(c); err == nil {
			out = append(out, ts)
		}
	}
	for _, d := range dirs {
		root := d
		add(root, func(rel string) (string, error) { return filetools.Under(root, rel) })
	}
	for d, set := range files {
		root, allowed := d, set
		add(root, func(rel string) (string, error) {
			got, err := filetools.Under(root, rel)
			if err == nil && !allowed[got] {
				return "", fmt.Errorf("%s: only the files added from %s are open (ask the user: /file add %s)", got, root, got)
			}
			return got, err
		})
	}
	return out
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
