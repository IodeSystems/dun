package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/project"
	"github.com/iodesystems/dun/internal/raglit"
)

// ragproject.go — register a workspace's project with the raglit daemon so
// shared memory has a home.
//
// One project per git root commit: the project's identity is the first root
// commit of the repository (`git rev-list --max-parents=0 HEAD`), so a repo and
// every worktree of it resolve to the same project, and a fork (a different
// root) is a different one. The daemon answers with a namespace and the three
// index names over it — the maintained public base and the caller's private and
// public layers (the plan's "private/public layers on a remote raglit").
//
// The namespace is persisted to the project's state dir, so a later session in
// the same workspace resolves to the same namespace — and the same notes —
// without re-registering. A later session is what makes "memory persists
// across sessions" true of the remote: the note travels to a layer, and the
// namespace is the key that finds it again.
//
// Registration only happens against a raglit that has keys and a login: a
// raglit without keys is left as it was (no project, no namespace), and a keyed
// one with no login tells the caller to /rag login.

// raglitState is the daemon's registration, kept in the project's state dir.
type raglitState struct {
	Namespace string `json:"namespace"`
	ProjectID string `json:"project_id"`
	Base      string `json:"base"`
	Private   string `json:"private"`
	Public    string `json:"public"`
}

// raglitStateFile is where a project's registration is kept: the project state
// dir, the same dir a project's settings are written to.
func raglitStateFile(dir string) (string, error) {
	p, err := project.Ensure(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(p.StateDir, "raglit.json"), nil
}

// raglitRegister is the seam a test replaces: it registers the project with
// the daemon and returns the registration. The default speaks to the shared
// per-user daemon with the client's own transport.
var raglitRegister = func(ctx context.Context, c *raglit.Client, projectID, label string) (raglit.Registration, error) {
	return c.Register(ctx, projectID, label)
}

// RegisterProject registers workspace's project with the raglit daemon and
// returns its namespace. It is idempotent: a project already registered resolves
// to the persisted namespace without asking the daemon again.
func (h *Harness) RegisterProject(ctx context.Context, workspace string) (string, error) {
	// A project already registered resolves to its namespace; nothing is asked.
	if ns, ok := h.raglitNamespace(workspace); ok {
		return ns, nil
	}
	id := rootCommitID(workspace)
	if id == "" {
		return "", fmt.Errorf("not a git repository (no root commit)")
	}
	label := originLabel(workspace)
	c := raglit.New("")
	c.Token = h.raglitBearer
	reg, err := raglitRegister(ctx, c, id, label)
	if err != nil {
		return "", err
	}
	if reg.Namespace == "" {
		return "", fmt.Errorf("the daemon returned no namespace")
	}
	if err := h.saveRaglitNamespace(workspace, raglitState{
		Namespace: reg.Namespace,
		ProjectID: reg.ProjectID,
		Base:      reg.Base,
		Private:   reg.Private,
		Public:    reg.Public,
	}); err != nil {
		return "", err
	}
	return reg.Namespace, nil
}

// raglitNamespace reads a project's persisted namespace; ok is false when it has
// not been registered (or its state cannot be read).
func (h *Harness) raglitNamespace(workspace string) (string, bool) {
	path, err := raglitStateFile(workspace)
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var s raglitState
	if json.Unmarshal(b, &s) != nil || s.Namespace == "" {
		return "", false
	}
	return s.Namespace, true
}

// saveRaglitNamespace persists a registration to the project's state dir.
func (h *Harness) saveRaglitNamespace(workspace string, s raglitState) error {
	path, err := raglitStateFile(workspace)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("dun: save raglit registration: %w", err)
	}
	return nil
}

// RegisterDocsServer runs project registration when the user turns on the docs
// (raglit) server: the message a caller appends to the switch line. It is empty
// when there is nothing to say — the server is not a keyed raglit, so it is left
// as it was. A keyed raglit with no login says so (the existing errRaglitLogin
// message); a registration failure is a note, not a refusal to start the server
// (same rule as the start itself).
func (h *Harness) RegisterDocsServer(ctx context.Context, id string) string {
	if id != mcpserver.ServerDocs {
		return ""
	}
	spec, ok := h.spec(id)
	if !ok {
		return ""
	}
	if _, keyed := raglitKeyed(spec); !keyed {
		return ""
	}
	if h.raglitBearer() == "" {
		return "  " + errRaglitLogin.Error()
	}
	ns, err := h.RegisterProject(ctx, h.cfg.Workspace)
	if err != nil {
		return "  project: not registered — " + oneLine(err.Error(), 160)
	}
	return "  project: " + ns
}

// raglitBearer is the credential the daemon wants: the session's key once one is
// minted, else the principal token /rag login kept on disk.
func (h *Harness) raglitBearer() string {
	r := h.ragRoot()
	r.ragMu.Lock()
	key := r.ragKey
	r.ragMu.Unlock()
	if key != "" {
		return key
	}
	return loadRaglitToken()
}

// rootCommitID is the first root commit of workspace's repository — the project
// id. It resolves worktrees to the main checkout the same way RaglitProject
// does, so a worktree registers the same project as its parent.
func rootCommitID(workspace string) string {
	root := gitMainRoot(workspace)
	if root == "" {
		return ""
	}
	return project.GitLine(root, "rev-list", "--max-parents=0", "HEAD")
}

// gitMainRoot is the main checkout of workspace's repo, or "" when it is not a
// repository. It is repoIdentity's logic, which lives in mcpserver.
func gitMainRoot(workspace string) string {
	common := project.GitLine(workspace, "rev-parse", "--git-common-dir")
	if common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(workspace, common)
	}
	return filepath.Dir(common)
}

// originLabel is the normalized origin remote: the host and path of the origin
// URL, so `git@github.com:org/repo.git` and `https://github.com/org/repo.git`
// both read `github-com-org-repo`. The daemon's project id is the identity; this
// is the human-facing label a sibling sees. "" when there is no origin.
func originLabel(workspace string) string {
	root := gitMainRoot(workspace)
	if root == "" {
		return ""
	}
	u := project.GitLine(root, "remote", "get-url", "origin")
	if u == "" {
		u = project.GitLine(root, "config", "remote.origin.url")
	}
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	return mcpserver.NormalizeProject(originHostPath(u))
}

// originHostPath reduces a remote URL to its host and path, for both the URL
// form (scheme://host/path) and the scp form (user@host:path).
func originHostPath(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		if p, err := url.Parse(u); err == nil && p.Host != "" {
			return strings.TrimSuffix(p.Host+p.Path, ".git")
		}
	}
	// scp form: user@host:path
	if i := strings.LastIndex(u, "@"); i >= 0 {
		u = u[i+1:]
	}
	host, path, ok := strings.Cut(u, ":")
	if !ok {
		return strings.TrimSuffix(u, ".git")
	}
	return strings.TrimSuffix(host+"/"+strings.TrimPrefix(path, "/"), ".git")
}
