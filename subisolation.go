package dun

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// MCP servers stay the parent's, shared (no second index per child), so
// poly-lsp reads the parent's tree, not a child's worktree. The isolation is
// written beside the child's transcript (<sub>.iso.json) so a resumed child
// goes back to the same tree.

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
}

func (iso isolation) isolated() bool { return iso.Mode != "" && iso.Mode != IsoNone }

// newIsolation makes the tree a child of h works in.
func (h *Harness) newIsolation(mode string) (isolation, error) {
	switch mode {
	case "", IsoNone:
		return isolation{}, nil
	case IsoNoDir:
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

// apply points a child's config at its tree.
func (iso isolation) apply(cfg *Config) {
	switch iso.Mode {
	case IsoWorktree, IsoDocker:
		cfg.Workspace = iso.Path
		cfg.Worktree = &Worktree{Path: iso.Path, Branch: iso.Branch, repoRoot: iso.Repo, Mounts: cfg.ExtraMounts}
		if iso.Mode == IsoDocker {
			image := dockerImageOrDefault(cfg.DockerImage)
			cfg.Exec = DockerExec{Dir: iso.Path, Image: image, Network: true, ExtraMounts: cfg.ExtraMounts}
		} else {
			cfg.Exec = &HostShell{Dir: iso.Path}
		}
	case IsoNoDir:
		cfg.Workspace = iso.Path
		cfg.Worktree = nil
		cfg.NoDir = true
		cfg.Files = &FilesConfig{Tools: []string{}}
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
		if iso.Mode == IsoDocker {
			s += " Your commands run in a container with the worktree at /work."
		}
		return s
	case IsoNoDir:
		return "\n\n[isolation] You have no project: no file tools, and commands run in a scratch directory. " +
			"Work from what you know, your tools and what the task gives you."
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
			s += "\nTo take it: git merge " + iso.Branch + " (from your tree)."
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
	if p == "" || !iso.isolated() {
		return
	}
	if b, err := json.Marshal(iso); err == nil {
		_ = writeFileAtomic(p, b, 0o600)
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
