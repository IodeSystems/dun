package dun

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Per-project state, kept OUT of the repository.
//
// dun used to keep what it knows about a repo inside it: .dun/dun.local.json,
// .dun/agents_read, .dun/worktrees/. That asked every repo dun touched for a
// .gitignore entry, and dun cannot ask that of anyone — a repo is not dun's to
// change. So nothing dun writes goes into a repo. All of it lives in DUN_HOME:
//
//	$DUN_HOME/projects.json                  registry: absolute source dir → id
//	$DUN_HOME/projects/<id>/dun.json         optional, written by a human
//	$DUN_HOME/projects/<id>/dun.local.json   machine state dun writes (/raglit auto)
//	$DUN_HOME/projects/<id>/agents_read      the AGENTS.md guard's memory
//	$DUN_HOME/projects/<id>/worktrees/       session worktrees + mount symlinks
//	$DUN_HOME/projects/<id>/overlays/<dir>/  one linked worktree's own state
//
// The id is MINTED the first time dun sees a directory (its name, slugged, plus
// six random hex digits) and stored in the registry. It is not derived from the
// path: a path hash cannot be read by a human, and a bare name collides between
// two checkouts of one repo.
//
// A git worktree — dun's own, `git worktree add`, Claude Code's — maps to its
// SOURCE repo's id, found through `git rev-parse --git-common-dir`, so it sees
// the project's state. What dun writes for that worktree directory goes to an
// overlay beside it, which wins over the project's on read.
//
// A repo's committed dun.json, .dun/dun.json and .dun/skills/ are still READ;
// see serverConfigPaths for the order.

// Project is the state location for one directory.
type Project struct {
	ID     string
	Source string // the directory the id is registered for: a repo's main worktree, or a plain dir
	Dir    string // the directory asked about: its git toplevel, or itself
	// StateDir is $DUN_HOME/projects/<id>.
	StateDir string
	// OverlayDir is the state of Dir alone, when Dir is a linked worktree of
	// Source; "" when Dir is Source.
	OverlayDir string
}

// WriteDir is where state about Dir goes: its overlay when it has one.
func (p *Project) WriteDir() string {
	if p.OverlayDir != "" {
		return p.OverlayDir
	}
	return p.StateDir
}

// WorktreesDir is where dun puts this project's session worktrees, and the
// mount symlinks beside them that make a go.mod "replace => ../x" resolve.
func (p *Project) WorktreesDir() string { return filepath.Join(p.StateDir, "worktrees") }

// AgentsReadFile is the AGENTS.md guard's state for Dir.
func (p *Project) AgentsReadFile() string { return filepath.Join(p.WriteDir(), agentsReadName) }

const agentsReadName = "agents_read"

// DunHome is $DUN_HOME, else ~/.dun.
func DunHome() string { return filepath.Dir(SessionsDir()) }

func projectsRoot() string { return filepath.Join(DunHome(), "projects") }

func registryFile() string { return filepath.Join(DunHome(), "projects.json") }

// registry is the on-disk shape of projects.json.
type registry struct {
	Projects map[string]string `json:"projects"` // absolute source dir → id
}

// projectRoots finds what dir belongs to: source is the directory the id is
// registered for, top is dir's own root. A linked worktree has top != source.
func projectRoots(dir string) (source, top string) {
	abs := realPath(dir)
	t := gitLine(abs, "rev-parse", "--show-toplevel")
	if t == "" {
		return abs, abs // not a repo: the directory is the project
	}
	top = realPath(t)
	common := gitLine(top, "rev-parse", "--git-common-dir")
	if common == "" {
		return top, top
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(top, common)
	}
	common = realPath(common)
	// <main>/.git is the ordinary case. Anything else (a bare repo, a
	// submodule's .git/modules/x) has no main worktree to name, so the
	// toplevel stands for itself.
	if filepath.Base(common) != ".git" {
		return top, top
	}
	return filepath.Dir(common), top
}

// realPath is the absolute, symlink-resolved form of p, or the absolute form
// when it cannot be resolved. One directory must have one key: on macOS the
// temp dir is reached both as /var/… and /private/var/….
func realPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return filepath.Clean(abs)
}

func overlayDir(stateDir, top string) string {
	sum := sha256.Sum256([]byte(top))
	return filepath.Join(stateDir, "overlays", slug(filepath.Base(top))+"-"+hex.EncodeToString(sum[:4]))
}

func newProject(id, source, top string) *Project {
	p := &Project{ID: id, Source: source, Dir: top, StateDir: filepath.Join(projectsRoot(), id)}
	if top != source {
		p.OverlayDir = overlayDir(p.StateDir, top)
	}
	return p
}

// ResolveProject is the project dir belongs to, or nil when dun has never seen
// it. It creates nothing: reading config must not be what registers a
// directory.
func ResolveProject(dir string) *Project {
	source, top := projectRoots(dir)
	reg, err := readRegistry()
	if err != nil {
		return nil
	}
	id := reg.Projects[source]
	if id == "" {
		return nil
	}
	return newProject(id, source, top)
}

// EnsureProject is ResolveProject that registers dir on first sight: it mints
// the id, creates the state directory (and dir's overlay, when dir is a
// linked worktree), and imports state an older dun left in the repo.
func EnsureProject(dir string) (*Project, error) {
	source, top := projectRoots(dir)
	id, minted, err := registerProject(source)
	if err != nil {
		return nil, err
	}
	p := newProject(id, source, top)
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("dun: create %s: %w", p.StateDir, err)
	}
	if minted {
		importLegacyState(source, p.StateDir)
	}
	if p.OverlayDir != "" {
		if _, err := os.Stat(p.OverlayDir); os.IsNotExist(err) {
			if err := os.MkdirAll(p.OverlayDir, 0o700); err != nil {
				return nil, fmt.Errorf("dun: create %s: %w", p.OverlayDir, err)
			}
			importLegacyState(top, p.OverlayDir)
		}
	}
	return p, nil
}

// registerProject returns source's id, minting and saving one when it has
// none. The registry is read and written under an exclusive lock: two dun
// processes started in two new repos at once would otherwise each write a
// registry missing the other's entry, and a lost entry is a project whose
// worktrees and settings are silently orphaned on the next start.
func registerProject(source string) (id string, minted bool, err error) {
	if err := os.MkdirAll(DunHome(), 0o700); err != nil {
		return "", false, fmt.Errorf("dun: create %s: %w", DunHome(), err)
	}
	unlock, err := lockFile(registryFile() + ".lock")
	if err != nil {
		return "", false, err
	}
	defer unlock()
	reg, err := readRegistry()
	if err != nil {
		return "", false, err
	}
	if id := reg.Projects[source]; id != "" {
		return id, false, nil
	}
	for range 10 {
		cand := slug(filepath.Base(source)) + "-" + randomHex(3)
		if _, err := os.Stat(filepath.Join(projectsRoot(), cand)); os.IsNotExist(err) && !idTaken(reg, cand) {
			id = cand
			break
		}
	}
	if id == "" {
		return "", false, fmt.Errorf("dun: could not mint a project id for %s", source)
	}
	reg.Projects[source] = id
	if err := writeFileAtomic(registryFile(), mustJSON(reg), 0o600); err != nil {
		return "", false, err
	}
	return id, true, nil
}

func idTaken(reg *registry, id string) bool {
	for _, v := range reg.Projects {
		if v == id {
			return true
		}
	}
	return false
}

func readRegistry() (*registry, error) {
	reg := &registry{Projects: map[string]string{}}
	b, err := os.ReadFile(registryFile())
	if os.IsNotExist(err) {
		return reg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dun: read %s: %w", registryFile(), err)
	}
	if err := json.Unmarshal(b, reg); err != nil {
		// Refuse rather than start over: an empty registry would mint every
		// project a new id and orphan everything filed under the old ones.
		return nil, fmt.Errorf("dun: parse %s: %w", registryFile(), err)
	}
	if reg.Projects == nil {
		reg.Projects = map[string]string{}
	}
	return reg, nil
}

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return append(b, '\n')
}

// writeFileAtomic replaces path with data via a temp file in the same
// directory and a rename, so a reader never sees half a file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, perm)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("dun: write %s: %w", path, werr)
	}
	return nil
}

// lockFile takes an exclusive flock on path, creating it; the returned func
// releases it. flock rather than an O_EXCL marker: the kernel drops it when a
// process dies, so a crash cannot leave the registry locked.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("dun: lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("dun: lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// slug is name reduced to what reads well in a path: lower-case letters,
// digits, '-', '_' and '.', at most 32 characters.
func slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.'
		switch {
		case ok:
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-.")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-.")
	}
	if s == "" {
		return "project"
	}
	return s
}

// legacyState is what an older dun wrote into <dir>/.dun/ and the name it has
// in a state directory now.
var legacyState = []string{LocalServersFile, agentsReadName}

// importLegacyState copies what an older dun left in dir/.dun/ into stateDir.
// A COPY: the original is the user's file in the user's tree, and dun no
// longer writes there. It says once (this runs only when stateDir is new)
// that the repo copy can go.
func importLegacyState(dir, stateDir string) {
	for _, name := range legacyState {
		from := filepath.Join(dir, DunDir, name)
		to := filepath.Join(stateDir, name)
		if _, err := os.Stat(to); err == nil {
			continue // never overwrite state that is already here
		}
		if err := copyFile(from, to, 0o600); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("dun: could not import %s: %v", from, err)
			}
			continue
		}
		log.Printf("dun: imported %s into %s — dun no longer writes in the repo; the repo copy can be removed", from, to)
	}
}

func copyFile(from, to string, perm os.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	return writeFileAtomic(to, data, perm)
}
