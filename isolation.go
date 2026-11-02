package dun

import (
	"fmt"
	"path/filepath"

	"github.com/iodesystems/dun/internal/mounttool"
)

// SetExec swaps the exec backend at runtime (e.g. host ↔ Docker) and rebuilds
// the tool set so the change takes effect immediately.
func (h *Harness) SetExec(backend ExecBackend) {
	h.srvMu.Lock()
	h.cfg.Exec = backend
	h.srvMu.Unlock()
	h.applyTools()
}

// Rehoist reconfigures the workspace layer atomically: workspace path, exec
// backend, and worktree. This is the right primitive for /worktree new and
// /docker on — they change where the agent's feet are and must update all
// three together so the agent doesn't split across directories.
//
// dockerOn controls the exec backend: true → DockerExec with cfg.DockerImage,
// false → HostExec. Both use the new workspace path and cfg.ExtraMounts.
func (h *Harness) Rehoist(workspace string, wt *Worktree, dockerOn bool) {
	h.srvMu.Lock()
	h.cfg.Workspace = workspace
	h.cfg.Worktree = wt
	image := dockerImageOrDefault(h.cfg.DockerImage)
	// The outgoing backend's shell is nobody's now. Closed in the background
	// because Close waits on the shell's lock, and a command may still be
	// running in it — rehoisting must not block on someone else's build.
	if old, ok := h.cfg.Exec.(*HostShell); ok {
		go old.Close()
	}
	if dockerOn {
		h.cfg.Exec = DockerExec{Dir: workspace, Image: image, Network: true, ExtraMounts: h.cfg.ExtraMounts}
	} else {
		// Host: persistent shell so exports survive between exec calls while
		// the working directory resets to the project root each time.
		h.cfg.Exec = &HostShell{Dir: workspace}
	}
	h.srvMu.Unlock()
	h.applyTools()
}

// AddMount adds a mount to the running session mid-flight. This is called by
// the mount tool after the user approves. It resolves the source path, adds
// the mount to the config, recreates the DockerExec backend with the new
// volume, and creates the symlink in the worktree parent directory.
func (h *Harness) AddMount(source, name string) error {
	repoRoot := ""
	if h.cfg.Worktree != nil {
		repoRoot = h.cfg.Worktree.RepoRoot()
	}
	resolved, err := mounttool.ResolveSource(source, repoRoot)
	if err != nil {
		return err
	}

	if err := h.addMountLocked(resolved, name, repoRoot); err != nil {
		return err
	}
	// After srvMu is released: the rebuild takes it (docs spec, skills).
	h.applyTools()
	return nil
}
func (h *Harness) addMountLocked(resolved, name, repoRoot string) error {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()

	// Check for duplicate name.
	for _, m := range h.cfg.ExtraMounts {
		if m.Name == name {
			return fmt.Errorf("mount name %q is already in use", name)
		}
	}

	spec := MountSpec{Source: resolved, Name: name}
	h.cfg.ExtraMounts = append(h.cfg.ExtraMounts, spec)

	// Reconfigure the exec backend with the new mount.
	if exec, ok := h.cfg.Exec.(DockerExec); ok {
		exec.ExtraMounts = h.cfg.ExtraMounts
		h.cfg.Exec = exec
	}

	// Create the symlink in the worktree parent directory — only a session
	// worktree's, in the project's state dir. Working in place, the parent is
	// wherever the user keeps the repo; an old-layout worktree's parent is
	// <repo>/.dun/worktrees/. dun writes in neither.
	if wt := h.cfg.Worktree; wt != nil && wt.IsRepo() && wt.Branch != "" {
		if p := ResolveProject(repoRoot); p != nil && filepath.Dir(wt.Path) == p.WorktreesDir() {
			if err := linkMount(p.WorktreesDir(), MountSpec{Source: resolved, Name: name}); err != nil {
				return err
			}
		}
	}

	// Update the worktree's mount list too.
	if h.cfg.Worktree != nil {
		h.cfg.Worktree.Mounts = h.cfg.ExtraMounts
	}
	return nil
}

// DefaultIsolationImage is the image /isolate docker uses until
// /isolate docker image names another.
const DefaultIsolationImage = "golang:1.23"

func dockerImageOrDefault(image string) string {
	if image == "" {
		return DefaultIsolationImage
	}
	return image
}

// DockerImage is the image /isolate docker runs commands in (and sub-agents
// with isolation docker), the default when none was chosen.
func (h *Harness) DockerImage() string {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	return dockerImageOrDefault(h.cfg.DockerImage)
}

// SetDockerImage chooses the image for /isolate docker and for sub-agents. A
// session already running commands in Docker switches to it at once.
func (h *Harness) SetDockerImage(image string) {
	h.srvMu.Lock()
	h.cfg.DockerImage = image
	d, isDocker := h.cfg.Exec.(DockerExec)
	if isDocker {
		d.Image = image
		h.cfg.Exec = d
	}
	h.srvMu.Unlock()
	if isDocker {
		h.applyTools()
	}
}

// IsDocker reports whether the current exec backend is DockerExec.
func (h *Harness) IsDocker() bool {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	_, ok := h.cfg.Exec.(DockerExec)
	return ok
}

// SetWorktree swaps the worktree at runtime and rebuilds the tool set.
// Deprecated: use Rehoist to change workspace + exec + worktree together.
func (h *Harness) SetWorktree(wt *Worktree) {
	h.srvMu.Lock()
	h.cfg.Worktree = wt
	h.srvMu.Unlock()
	h.applyTools()
}
