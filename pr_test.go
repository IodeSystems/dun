package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

func mkCall(name, args string) llm.ToolCall {
	var tc llm.ToolCall
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

// TestOpenPR_CommitsAndPushes drives openPR against a local bare "origin": the
// commit + push must land the session branch on origin. (The `gh pr create` step
// then fails — no GitHub remote — which the result reports; that path works on a
// real GitHub repo.)
func TestOpenPR_CommitsAndPushes(t *testing.T) {
	origin := t.TempDir()
	gitrun(t, origin, "init", "--bare", "-q")

	repo := t.TempDir()
	gitrun(t, repo, "init", "-q")
	gitrun(t, repo, "config", "user.email", "t@t")
	gitrun(t, repo, "config", "user.name", "t")
	gitrun(t, repo, "remote", "add", "origin", origin)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "init")
	gitrun(t, repo, "push", "-q", "-u", "origin", "HEAD")

	wt, isRepo, err := NewWorktree(repo)
	if err != nil || !isRepo {
		t.Fatalf("NewWorktree: %v", err)
	}
	defer wt.Cleanup()

	// The agent's edit, then open_pr.
	os.WriteFile(filepath.Join(wt.Path, "feature.txt"), []byte("new feature\n"), 0o644)
	out := openPR(context.Background(), wt, "Add feature", "adds feature.txt", "")

	// The branch must now exist on origin with the committed change.
	branches, _ := git("", "-C", origin, "branch", "--list")
	if !strings.Contains(branches, wt.Branch) {
		t.Fatalf("branch %s not pushed to origin; branches=%q\nopenPR out=%q", wt.Branch, branches, out)
	}
	files, _ := git("", "-C", origin, "ls-tree", "--name-only", wt.Branch)
	if !strings.Contains(files, "feature.txt") {
		t.Fatalf("pushed branch missing the commit; files=%q", files)
	}
	// gh isn't wired to a GitHub remote here, so openPR reports the push + manual step.
	if !strings.Contains(out, "Pushed branch") && !strings.Contains(out, "Opened pull request") {
		t.Fatalf("unexpected openPR result: %q", out)
	}
}

func TestWithPR_RequiresWorktree(t *testing.T) {
	d := withPR(agentDispatch(func(n string) string { return "MCP:" + n }), nil, nil)
	var tc = mkCall("open_pr", `{"title":"x"}`)
	out, _ := d(context.Background(), tc)
	if !strings.Contains(out, "isolated git branch") {
		t.Fatalf("open_pr without a worktree should error: %q", out)
	}
}
