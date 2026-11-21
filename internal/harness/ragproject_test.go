package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/raglit"
)

// gitRepo makes a one-commit git repo in dir with origin set to remote, and
// returns the root commit's sha.
func gitRepoOrigin(t *testing.T, dir, remote string) string {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if remote != "" {
		run("remote", "add", "origin", remote)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init")
	out, err := exec.Command("git", "-C", dir, "rev-list", "--max-parents=0", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestRegisterProject_RegistersAndPersists(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	root := gitRepoOrigin(t, ws, "git@github.com:org/repo.git")

	var gotID, gotLabel string
	orig := raglitRegister
	raglitRegister = func(ctx context.Context, c *raglit.Client, projectID, label string) (raglit.Registration, error) {
		gotID, gotLabel = projectID, label
		return raglit.Registration{
			ProjectID: projectID,
			Namespace: "ns-1234",
			Base:      "ns-1234__base",
			Private:   "ns-1234__u-abcd-priv",
			Public:    "ns-1234__u-abcd-pub",
		}, nil
	}
	t.Cleanup(func() { raglitRegister = orig })

	h := &Harness{cfg: Config{Workspace: ws}}
	ns, err := h.RegisterProject(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if ns != "ns-1234" {
		t.Errorf("namespace = %q, want ns-1234", ns)
	}
	if gotID != root {
		t.Errorf("project_id = %q, want root commit %q", gotID, root)
	}
	if gotLabel != "github-com-org-repo" {
		t.Errorf("label = %q, want github-com-org-repo", gotLabel)
	}
}

func TestRegisterProject_Idempotent(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	gitRepoOrigin(t, ws, "git@github.com:org/repo.git")

	calls := 0
	orig := raglitRegister
	raglitRegister = func(ctx context.Context, c *raglit.Client, projectID, label string) (raglit.Registration, error) {
		calls++
		return raglit.Registration{ProjectID: projectID, Namespace: "ns-1234"}, nil
	}
	t.Cleanup(func() { raglitRegister = orig })

	h := &Harness{cfg: Config{Workspace: ws}}
	if _, err := h.RegisterProject(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if _, err := h.RegisterProject(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("register calls = %d, want 1 (second call should resolve from disk)", calls)
	}
}

func TestRegisterProject_PersistsStateFile(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	gitRepoOrigin(t, ws, "git@github.com:org/repo.git")

	orig := raglitRegister
	raglitRegister = func(ctx context.Context, c *raglit.Client, projectID, label string) (raglit.Registration, error) {
		return raglit.Registration{ProjectID: projectID, Namespace: "ns-1234", Base: "ns-1234__base", Private: "p", Public: "q"}, nil
	}
	t.Cleanup(func() { raglitRegister = orig })

	h := &Harness{cfg: Config{Workspace: ws}}
	if _, err := h.RegisterProject(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	path, err := raglitStateFile(ws)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	if !strings.Contains(string(b), "ns-1234") {
		t.Errorf("state file missing namespace: %s", b)
	}
}

func TestOriginHostPath(t *testing.T) {
	cases := map[string]string{
		"git@github.com:org/repo.git":  "github.com/org/repo",
		"https://github.com/org/repo.git": "github.com/org/repo",
		"https://example.com/a/b":      "example.com/a/b",
		"user@host.example:grp/proj.git": "host.example/grp/proj",
	}
	for in, want := range cases {
		if got := originHostPath(in); got != want {
			t.Errorf("originHostPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOriginLabel_NoOrigin(t *testing.T) {
	ws := t.TempDir()
	gitRepoOrigin(t, ws, "")
	if got := originLabel(ws); got != "" {
		t.Errorf("originLabel with no origin = %q, want empty", got)
	}
}

func TestOriginLabel_FromRepo(t *testing.T) {
	ws := t.TempDir()
	gitRepoOrigin(t, ws, "git@github.com:org/repo.git")
	if got := originLabel(ws); got != "github-com-org-repo" {
		t.Errorf("originLabel = %q, want github-com-org-repo", got)
	}
}

func TestRootCommitID_NotARepo(t *testing.T) {
	ws := t.TempDir()
	if got := rootCommitID(ws); got != "" {
		t.Errorf("rootCommitID for non-repo = %q, want empty", got)
	}
}

func TestRootCommitID_MatchesRoot(t *testing.T) {
	ws := t.TempDir()
	root := gitRepoOrigin(t, ws, "")
	if got := rootCommitID(ws); got != root {
		t.Errorf("rootCommitID = %q, want %q", got, root)
	}
}

func TestRegisterProject_NotAGitRepo(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h := &Harness{cfg: Config{Workspace: ws}}
	_, err := h.RegisterProject(context.Background(), ws)
	if err == nil {
		t.Fatal("expected an error for a non-git workspace")
	}
}

func TestRaglitSavings_NotRegistered(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h := &Harness{cfg: Config{Workspace: ws}}
	_, err := h.RaglitSavings(context.Background())
	if err == nil {
		t.Fatal("expected an error for an unregistered project")
	}
}

func TestRaglitSavings_ReturnsLayers(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	gitRepoOrigin(t, ws, "git@github.com:org/repo.git")

	orig := raglitRegister
	raglitRegister = func(ctx context.Context, c *raglit.Client, projectID, label string) (raglit.Registration, error) {
		return raglit.Registration{ProjectID: projectID, Namespace: "ns-1234", Base: "ns-1234__base", Private: "p", Public: "q"}, nil
	}
	t.Cleanup(func() { raglitRegister = orig })

	h := &Harness{cfg: Config{Workspace: ws}}
	if _, err := h.RegisterProject(context.Background(), ws); err != nil {
		t.Fatal(err)
	}

	want := raglit.Savings{
		ProjectID:          "abc123",
		DuplicateDocuments: 5,
		DuplicateBytes:     1024,
		DuplicateEmbeddings: 12,
		FanoutNow:          3,
		FanoutWithBase:     1,
		Recommend:          true,
		Reason:             "enough overlap",
		Layers: []raglit.LayerSavings{
			{Name: "private", Documents: 10, Duplicates: 3},
			{Name: "public", Documents: 7, Duplicates: 2},
		},
	}
	origSav := raglitSavings
	raglitSavings = func(ctx context.Context, c *raglit.Client, projectID, namespace string) (raglit.Savings, error) {
		if namespace != "ns-1234" {
			t.Errorf("namespace = %q, want ns-1234", namespace)
		}
		return want, nil
	}
	t.Cleanup(func() { raglitSavings = origSav })

	sv, err := h.RaglitSavings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sv.DuplicateDocuments != 5 || sv.FanoutNow != 3 || !sv.Recommend {
		t.Errorf("savings = %+v, want aggregates from the seam", sv)
	}
	if len(sv.Layers) != 2 || sv.Layers[0].Name != "private" || sv.Layers[1].Documents != 7 {
		t.Errorf("layers = %+v, want 2 layers with the right names and counts", sv.Layers)
	}
}
