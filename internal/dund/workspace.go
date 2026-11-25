package dund

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/project"
)

// userHome is the home directory paths without a leading slash are read
// under: the home of the user dund runs as, which is the owner every token
// stands for. A variable so tests can point it at a temporary directory.
var userHome = os.UserHomeDir

// ResolveWorkspace is how dund reads a workspace a client typed: "~" and
// "~/x" are under the home, a path without a leading slash is under the home
// too (a client cannot know dund's working directory, and the home is what a
// person means by "src/project"), and the result is cleaned. It does not look
// at the filesystem; NormalizeWorkspace does.
func ResolveWorkspace(ws string) (string, error) {
	ws = strings.TrimSpace(ws)
	if ws == "" {
		return "", errors.New("enter a directory: an absolute path, ~/path, or a path under your home")
	}
	if filepath.IsAbs(ws) {
		return filepath.Clean(ws), nil
	}
	if strings.HasPrefix(ws, "~") {
		rest, ok := strings.CutPrefix(ws, "~")
		if rest != "" && !strings.HasPrefix(rest, "/") {
			ok = false
		}
		if !ok {
			return "", fmt.Errorf("%q: ~ means your home; ~user is not supported", ws)
		}
		ws = strings.TrimPrefix(rest, "/")
	}
	home, err := userHome()
	if err != nil || home == "" {
		return "", fmt.Errorf("%q is not an absolute path, and dund has no home directory to read it under", ws)
	}
	return filepath.Join(home, ws), nil
}

// NormalizeWorkspace is ResolveWorkspace, and the result must be an existing
// directory. Its errors are written for the person who typed the path: a
// client shows them as they are.
func NormalizeWorkspace(ws string) (string, error) {
	p, err := ResolveWorkspace(ws)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("no directory %s: it does not exist", p)
	case err != nil:
		return "", fmt.Errorf("cannot open %s: %v", p, errors.Unwrap(err))
	case !st.IsDir():
		return "", fmt.Errorf("%s is a file, not a directory", p)
	}
	return p, nil
}

// Workspace is a directory sessions ran in, for a client's new-session
// picker.
type Workspace struct {
	Path string `json:"path"`
	// LastActive is the newest transcript write there, RFC3339 in UTC.
	LastActive string `json:"last_active,omitempty"`
	// Running is whether a session there has a running engine.
	Running bool `json:"running"`
}

// workspacesMax bounds Workspaces.
const workspacesMax = 50

// Workspaces is where principal's sessions ran, most recent activity first,
// at most workspacesMax: the workspaces of principal's sessions in the
// registry and, with disk (the principal is the user dund runs as), every
// ~/.dun/sessions/<encoded workspace> whose directory still exists and is not
// under the temp directory. A running
// workspace does not jump the order: the picker is "where was I working",
// and recency answers that.
func (s *Service) Workspaces(principal string, disk bool) []Workspace {
	type acc struct {
		last    time.Time
		running bool
	}
	by := map[string]*acc{}
	get := func(p string) *acc {
		a := by[p]
		if a == nil {
			a = &acc{}
			by[p] = a
		}
		return a
	}
	s.mu.Lock()
	var mine []*session
	for _, ses := range s.sessions {
		if ses.isMember(principal) {
			mine = append(mine, ses)
		}
	}
	s.mu.Unlock()
	for _, ses := range mine {
		sum := ses.summary()
		a := get(filepath.Clean(ses.workspace))
		a.running = a.running || sum.Running
		if t, err := time.Parse(time.RFC3339, sum.LastActive); err == nil && t.After(a.last) {
			a.last = t
		}
	}
	if disk {
		root := harness.SessionsDir()
		entries, _ := os.ReadDir(root)
		dec := newRootDecoder()
		tmpName := filepath.Base(harness.RootDir(os.TempDir())) + "-"
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "-") {
				continue
			}
			// A transcript directory under the temp directory is a test's or an
			// agent's scratch run: hundreds of them, nowhere a person works.
			if strings.HasPrefix(e.Name(), tmpName) {
				continue
			}
			p, ok := dec.decode(e.Name())
			if !ok {
				continue
			}
			t := newestTranscript(filepath.Join(root, e.Name()))
			if t.IsZero() {
				continue // nothing was ever said there
			}
			if a := get(p); t.After(a.last) {
				a.last = t
			}
		}
		// Id-keyed session directories: look up the project registry for
		// directories whose sessions live under $DUN_HOME/sessions/<id>/.
		if entries2, err := project.List(); err == nil {
			for _, entry := range entries2 {
				dir := project.SessionDir(entry.ID)
				t := newestTranscript(dir)
				if t.IsZero() {
					continue
				}
				if a := get(entry.Source); t.After(a.last) {
					a.last = t
				}
			}
		}
	}
	out := make([]Workspace, 0, len(by))
	times := map[string]time.Time{}
	for p, a := range by {
		out = append(out, Workspace{Path: p, LastActive: rfc3339(a.last), Running: a.running})
		times[p] = a.last
	}
	slices.SortFunc(out, func(a, b Workspace) int {
		if c := times[b.Path].Compare(times[a.Path]); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	if len(out) > workspacesMax {
		out = out[:workspacesMax]
	}
	return out
}

// newestTranscript is the newest modification time of a non-empty session
// transcript in a workspace's sessions directory (sidecars, with a dot in the
// id, do not count); zero when there is none.
func newestTranscript(dir string) time.Time {
	entries, _ := os.ReadDir(dir)
	var newest time.Time
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || strings.Contains(id, ".") || e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// rootDecoder turns a harness.RootDir name back into the directory it was made
// from. The encoding ("/" to "-", " " to "_") loses information — a "-" may
// have been a "/" or a "-" — so the name is matched against the directories
// that exist, one level at a time. Directory listings are cached for one
// decoder: names share their prefixes.
type rootDecoder struct {
	dirs map[string][]string
}

func newRootDecoder() *rootDecoder { return &rootDecoder{dirs: map[string][]string{}} }

func encodePart(name string) string { return strings.ReplaceAll(name, " ", "_") }

// decode reports the existing directory whose RootDir name is name.
func (d *rootDecoder) decode(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "-")
	if !ok {
		return "", false
	}
	// The usual case, a path with no "-" or " " in it, costs one stat.
	if p := "/" + strings.ReplaceAll(rest, "-", "/"); isDir(p) && filepath.Base(harness.RootDir(p)) == name {
		return p, true
	}
	return d.walk("/", rest, 0)
}

func (d *rootDecoder) walk(dir, rest string, depth int) (string, bool) {
	if rest == "" {
		return dir, true
	}
	if depth > 64 {
		return "", false
	}
	for _, sub := range d.subdirs(dir) {
		enc := encodePart(sub)
		if rest == enc {
			return filepath.Join(dir, sub), true
		}
		if tail, ok := strings.CutPrefix(rest, enc+"-"); ok {
			if p, ok := d.walk(filepath.Join(dir, sub), tail, depth+1); ok {
				return p, true
			}
		}
	}
	return "", false
}

func (d *rootDecoder) subdirs(dir string) []string {
	if v, ok := d.dirs[dir]; ok {
		return v
	}
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if e.IsDir() || (e.Type()&fs.ModeSymlink != 0 && isDir(filepath.Join(dir, e.Name()))) {
			out = append(out, e.Name())
		}
	}
	d.dirs[dir] = out
	return out
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// DirListing is BrowseDirs's answer.
type DirListing struct {
	// Workspace is the typed path as Create would read it (ResolveWorkspace).
	Workspace string `json:"workspace"`
	// Problem is why Create would refuse Workspace (NormalizeWorkspace's
	// sentence); "" when it would start there.
	Problem string `json:"problem,omitempty"`
	// Dir is the directory listed.
	Dir string `json:"dir"`
	// Items are Dir's subdirectories that start with the last part of the
	// typed path (all of them after a trailing "/"), by name, as full paths.
	Items []string `json:"items"`
	// Truncated is whether there were more than the items returned.
	Truncated bool `json:"truncated"`
}

// dirsMax bounds a BrowseDirs listing.
const dirsMax = 200

// BrowseDirs lists directories for completing a typed workspace path. A path
// ending in "/" (or empty: the home) lists that directory; otherwise the last
// part is a prefix, matched in its parent, case-insensitively. Hidden
// directories are listed only when the prefix starts with ".". A directory
// that cannot be read lists nothing; that is not an error, it is a path still
// being typed.
func BrowseDirs(typed string) (DirListing, error) {
	if strings.TrimSpace(typed) == "" {
		typed = "~/"
	}
	resolved, err := ResolveWorkspace(typed)
	if err != nil {
		return DirListing{}, err
	}
	out := DirListing{Workspace: resolved, Items: []string{}}
	if _, err := NormalizeWorkspace(resolved); err != nil {
		out.Problem = err.Error()
	}
	// The prefix is the typed last part, before cleaning: "~/." asks for
	// hidden directories in the home, not for the home's siblings.
	dir, prefix := resolved, ""
	if t := strings.TrimSpace(typed); !strings.HasSuffix(t, "/") && t != "~" {
		i := strings.LastIndex(t, "/")
		prefix = t[i+1:]
		parent := t[:i+1]
		if i < 0 {
			parent = "~/" // "src": a first part, under the home
		}
		if dir, err = ResolveWorkspace(parent); err != nil {
			return DirListing{}, err
		}
	}
	out.Dir = dir
	entries, _ := os.ReadDir(dir)
	lp := strings.ToLower(prefix)
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(n), lp) {
			continue
		}
		full := filepath.Join(dir, n)
		if !e.IsDir() && (e.Type()&fs.ModeSymlink == 0 || !isDir(full)) {
			continue
		}
		if len(out.Items) == dirsMax {
			out.Truncated = true
			break
		}
		out.Items = append(out.Items, full)
	}
	slices.SortFunc(out.Items, func(a, b string) int {
		return strings.Compare(strings.ToLower(filepath.Base(a)), strings.ToLower(filepath.Base(b)))
	})
	return out, nil
}
