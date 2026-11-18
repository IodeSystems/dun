package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// Sub-agent isolation (USER 2026-10-27: "subagents working simultaneously …
// modes of nodir, isolation:worktree|docker"). By default a child works in
// its parent's tree (subagent.go: the cheap side). Two children editing one
// tree at once overwrite each other, so the agent tool takes an isolation:
//
//	none      the parent's tree, as before
//	worktree  a git worktree of its own, branched from the parent's last
//	          commit; its file tools and exec are rooted there. It commits
//	          there; the parent merges the branch, or does not.
//	docker    a worktree as above, its commands in a container on it
//	nodir     no project: no file tools, exec in a scratch directory; for
//	          research and summaries
//
// MCP servers stay the parent's, shared (no second index per child), except
// each server keyed on {{workspace}}: an isolated child resolves those
// against its own tree and runs its own instance (servers_share.go
// childOwn), so poly-lsp reads the child's worktree (its daemon gives it a
// root of its own). Servers not keyed on the workspace stay the parent's
// (USER 2026-11-06). The isolation is
// written beside the child's transcript (<sub>.iso.json) so a resumed child
// goes back to the same tree.
//
// dir (USER 2026-11-17: a parent in a non-git overseer directory starts a
// child meant for a repo below or elsewhere) names where the child works:
// any readable directory. With none it works there in place; with worktree
// or docker the worktree is branched from dir's repo, and the child works in
// the same subdirectory of it.

// Isolation modes.
const (
	IsoNone     = "none"
	IsoWorktree = "worktree"
	IsoDocker   = "docker"
	IsoNoDir    = "nodir"
)

// isolation is where a child works, when not in its parent's tree.
type isolation struct {
	Mode   string `json:"mode"`
	Path   string `json:"path"`             // the worktree or scratch directory
	Branch string `json:"branch,omitempty"` // worktree, docker
	Base   string `json:"base,omitempty"`   // the commit it branched from
	Repo   string `json:"repo,omitempty"`   // the repository's root
	// Dir is the directory the spawn named ("" is the parent's workspace).
	Dir string `json:"dir,omitempty"`
	// Sub is Dir relative to Repo: the child works in Path/Sub.
	Sub string `json:"sub,omitempty"`
	// Foreign: Repo is not the parent's, so the merge hint names it.
	Foreign bool `json:"foreign,omitempty"`
}

func (iso isolation) isolated() bool { return iso.Mode != "" && iso.Mode != IsoNone }

// workspace is the directory the child works in.
func (iso isolation) workspace() string {
	switch iso.Mode {
	case IsoWorktree, IsoDocker:
		return filepath.Join(iso.Path, iso.Sub)
	case IsoNoDir:
		return iso.Path
	}
	return iso.Dir
}

// newIsolation makes the tree a child of h works in; dir is the spawn's
// ("" is the parent's workspace).
func (h *Harness) newIsolation(mode, dir string) (isolation, error) {
	if dir != "" {
		d, err := h.resolveDir(dir)
		if err != nil {
			return isolation{}, err
		}
		dir = d
	}
	switch mode {
	case "", IsoNone:
		if dir == "" {
			return isolation{}, nil
		}
		// The child gets a host shell on dir; a parent sandboxed in a
		// container must not hand one out.
		switch h.cfg.Exec.(type) {
		case nil, *HostShell, HostExec, *HostExec:
		default:
			return isolation{}, fmt.Errorf("dir without isolation needs a host shell, and this session's commands run in %T", h.cfg.Exec)
		}
		return isolation{Mode: IsoNone, Dir: dir}, nil
	case IsoNoDir:
		if dir != "" {
			return isolation{}, fmt.Errorf("isolation nodir has no directory; drop dir, or use isolation none or worktree")
		}
		root := filepath.Join(DunHome(), "nodir")
		if err := os.MkdirAll(root, 0o700); err != nil {
			return isolation{}, err
		}
		d, err := os.MkdirTemp(root, "agent-")
		if err != nil {
			return isolation{}, err
		}
		return isolation{Mode: IsoNoDir, Path: d}, nil
	case IsoWorktree, IsoDocker:
		if h.cfg.NoDir {
			return isolation{}, fmt.Errorf("a --no-dir session has no repository to branch a worktree from")
		}
		if mode == IsoDocker {
			if _, err := exec.LookPath("docker"); err != nil {
				return isolation{}, fmt.Errorf("isolation docker needs docker on PATH: %v", err)
			}
		}
		if dir != "" {
			return h.dirWorktree(mode, dir)
		}
		wt, isRepo, err := NewWorktree(h.cfg.Workspace, h.cfg.ExtraMounts)
		if err != nil {
			return isolation{}, err
		}
		if !isRepo {
			return isolation{}, fmt.Errorf("isolation %s needs a git repository, and %s is not in one", mode, h.cfg.Workspace)
		}
		base, _ := git("", "-C", wt.Path, "rev-parse", "HEAD")
		return isolation{Mode: mode, Path: wt.Path, Branch: wt.Branch, Base: strings.TrimSpace(base), Repo: wt.RepoRoot()}, nil
	}
	return isolation{}, fmt.Errorf("isolation %q: want none, worktree, docker or nodir", mode)
}

// dirWorktree is a worktree for a spawn's dir: off the HEAD of the checkout
// holding dir, in its main repo's store (dir may be in a linked worktree,
// another child's), the child in the same subdirectory of it.
func (h *Harness) dirWorktree(mode, dir string) (isolation, error) {
	top, repo := RepoRoot(dir), MainRepo(dir)
	if top == "" || repo == "" {
		return isolation{}, notARepo(dir)
	}
	foreign := !sameDir(repo, MainRepo(h.cfg.Workspace))
	mounts := h.cfg.ExtraMounts
	if foreign {
		// The parent's mounts are its go.mod's; this repo has its own.
		mounts = mcpserver.LoadMounts(repo, repo)
	}
	wt, err := newWorktreeIn(top, repo, mounts)
	if err != nil {
		return isolation{}, err
	}
	base, _ := git("", "-C", wt.Path, "rev-parse", "HEAD")
	iso := isolation{Mode: mode, Path: wt.Path, Branch: wt.Branch, Base: strings.TrimSpace(base), Repo: repo, Dir: dir, Foreign: foreign}
	if rel, err := filepath.Rel(top, dir); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		iso.Sub = rel
	}
	// An untracked, ignored or empty dir is not in HEAD, so not in the tree.
	if fi, err := os.Stat(iso.workspace()); err != nil || !fi.IsDir() {
		iso.release()
		return isolation{}, fmt.Errorf("%s is not in %s's HEAD (untracked, ignored or empty): commit it, or pass the repo root as dir", dir, top)
	}
	return iso, nil
}

// resolveDir is a spawn's dir as a real directory: absolute, ~/, or relative
// to h's workspace, symlinks resolved.
func (h *Harness) resolveDir(dir string) (string, error) {
	d := strings.TrimSpace(dir)
	if d == "~" || strings.HasPrefix(d, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("dir %s: %v", dir, err)
		}
		d = filepath.Join(home, strings.TrimPrefix(d, "~"))
	} else if !filepath.IsAbs(d) {
		d = filepath.Join(h.Workspace(), d)
	}
	r, err := filepath.EvalSymlinks(d)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("dir %s does not exist", d)
	}
	if err != nil {
		return "", fmt.Errorf("dir %s: %v", d, err)
	}
	if fi, err := os.Stat(r); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("dir %s is not a directory", d)
	}
	return r, nil
}

// sameDir compares two directories by their real paths.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	if r, err := filepath.EvalSymlinks(a); err == nil {
		a = r
	}
	if r, err := filepath.EvalSymlinks(b); err == nil {
		b = r
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// notARepo refuses a worktree for dir, naming the repositories just below it
// (two levels; an overseer directory usually holds its repos at one).
func notARepo(dir string) error {
	var repos []string
	var scan func(d string, depth int)
	scan = func(d string, depth int) {
		ents, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range ents {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			p := filepath.Join(d, e.Name())
			if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
				repos = append(repos, p)
			} else if depth < 2 {
				scan(p, depth+1)
			}
		}
	}
	scan(dir, 1)
	if len(repos) == 0 {
		return fmt.Errorf("dir %s is not in a git repository", dir)
	}
	return fmt.Errorf("dir %s is not in a git repository; it holds these, name one as dir: %s", dir, strings.Join(repos, ", "))
}

// apply points a child's config at its tree.
func (iso isolation) apply(cfg *Config) {
	switch iso.Mode {
	case "", IsoNone:
		if iso.Dir == "" {
			return
		}
		cfg.Workspace = iso.Dir
		cfg.Worktree, _ = WorktreeInPlace(iso.Dir)
		cfg.Worktree.Mounts = cfg.ExtraMounts
		cfg.Exec = &HostShell{Dir: iso.Dir}
	case IsoWorktree, IsoDocker:
		if iso.Foreign {
			cfg.ExtraMounts = mcpserver.LoadMounts(iso.Repo, iso.Repo)
		}
		ws := iso.workspace()
		cfg.Workspace = ws
		// Path, not ws: git and the branch are the worktree's.
		cfg.Worktree = &Worktree{Path: iso.Path, Branch: iso.Branch, repoRoot: iso.Repo, Mounts: cfg.ExtraMounts}
		if iso.Mode == IsoDocker {
			image := dockerImageOrDefault(cfg.DockerImage)
			cfg.Exec = DockerExec{Dir: iso.Path, Sub: iso.Sub, Image: image, Network: true, ExtraMounts: cfg.ExtraMounts, TmpDir: MkIsoTmp(cfg.SessionFile)}
		} else {
			cfg.Exec = &HostShell{Dir: ws}
		}
	case IsoNoDir:
		cfg.Workspace = iso.Path
		cfg.Worktree = nil
		cfg.NoDir = true
		cfg.Files = &mcpserver.FilesConfig{Tools: []string{}}
		cfg.Exec = &HostShell{Dir: iso.Path}
	}
}

// brief is what the child is told about where it works, after its task.
func (iso isolation) brief() string {
	switch iso.Mode {
	case IsoWorktree, IsoDocker:
		s := fmt.Sprintf("\n\n[isolation] You work in your own git worktree, %s, on branch %s, branched from %s. "+
			"Other agents may be working at the same time in their own trees; nothing you do touches theirs or your "+
			"parent's. Commit your work on this branch when it is done: your parent merges the branch.",
			iso.Path, iso.Branch, short(iso.Base))
		if iso.Dir != "" {
			s += " It is a worktree of " + iso.Repo + "."
		}
		if iso.Sub != "" {
			s += " Your directory is " + iso.workspace() + "."
		}
		if iso.Mode == IsoDocker {
			s += " Your commands run in a container with the worktree at /work."
		}
		return s
	case "", IsoNone:
		if iso.Dir != "" {
			return "\n\n[dir] You work directly in " + iso.Dir + ", not in a worktree: your edits land there as you make them."
		}
	case IsoNoDir:
		return "\n\n[isolation] You have no project: no file tools, and commands run in a scratch directory. " +
			"If the node needs project files, plan_close it dropped at once with the reason \"missing tools: <names>\"; " +
			"do not work around it in the scratch directory. Otherwise work from what you know, your tools and what the task gives you."
	}
	return ""
}

// status is the report's line on the child's tree: its commits and its
// uncommitted files, and what the parent does with them.
func (iso isolation) status() string {
	switch iso.Mode {
	case IsoWorktree, IsoDocker:
		log, _ := git("", "-C", iso.Path, "log", "--oneline", iso.Base+"..HEAD")
		dirty, _ := git("", "-C", iso.Path, "status", "--porcelain")
		commits := nonEmptyLines(log)
		files := nonEmptyLines(dirty)
		s := fmt.Sprintf("\nisolation: %s %s, branch %s: %d commit(s) since %s, %d uncommitted file(s)",
			iso.Mode, iso.Path, iso.Branch, len(commits), short(iso.Base), len(files))
		for i, c := range commits {
			if i == 10 {
				s += fmt.Sprintf("\n  … %d more", len(commits)-10)
				break
			}
			s += "\n  " + c
		}
		if len(commits) > 0 {
			if iso.Foreign {
				s += "\nTo take it: git -C " + iso.Repo + " merge " + iso.Branch + "."
			} else {
				s += "\nTo take it: git merge " + iso.Branch + " (from your tree)."
			}
		}
		if len(files) > 0 {
			s += "\nUncommitted work is not on the branch: tell it to commit, or read it in " + iso.Path + "."
		}
		return s
	case IsoNoDir:
		return "\nisolation: nodir, scratch directory " + iso.Path
	}
	return ""
}

// empty reports whether a worktree child left nothing: no commits since its
// base and a clean tree.
func (iso isolation) empty() bool {
	log, err1 := git("", "-C", iso.Path, "log", "--oneline", iso.Base+"..HEAD")
	dirty, err2 := git("", "-C", iso.Path, "status", "--porcelain")
	return err1 == nil && err2 == nil && strings.TrimSpace(log) == "" && strings.TrimSpace(dirty) == ""
}

// release is the dismissal's part: a worktree that holds nothing is removed
// with its branch; one that holds work is kept, and said so.
func (iso isolation) release() string {
	switch iso.Mode {
	case IsoWorktree, IsoDocker:
		if !iso.empty() {
			return " Its worktree is kept, it holds work: " + iso.Path + " (branch " + iso.Branch + ")."
		}
		_, _ = git("", "-C", iso.Repo, "worktree", "remove", "--force", iso.Path)
		_, _ = git("", "-C", iso.Repo, "branch", "-D", iso.Branch)
		return " Its worktree held nothing and was removed."
	case IsoNoDir:
		_ = os.RemoveAll(iso.Path)
	}
	return ""
}

func isoFile(sessionFile string) string {
	if sessionFile == "" {
		return ""
	}
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".iso.json"
}

func saveIsolation(sessionFile string, iso isolation) {
	p := isoFile(sessionFile)
	if p == "" || (!iso.isolated() && iso.Dir == "") {
		return
	}
	if b, err := json.Marshal(iso); err == nil {
		_ = fsutil.WriteAtomic(p, b, 0o600)
	}
}

func loadIsolation(sessionFile string) isolation {
	var iso isolation
	if p := isoFile(sessionFile); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &iso)
		}
	}
	return iso
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
