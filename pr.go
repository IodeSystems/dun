package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// worktree → PR.
//
// The agent's edits live on an isolated dun/<ts> branch (worktree.go). open_pr
// turns that branch into a reviewable pull request: commit the changes, push the
// branch, `gh pr create`. Opt-in (the host passes --pr) because pushing + opening
// a PR is an outward-facing action; without it, dun just leaves the diff on the
// branch for manual review.

func prToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "open_pr"
	td.Function.Description = "Submit your work as a pull request: commits the worktree changes onto the " +
		"session branch, pushes it, and opens a PR. Call this once the task is complete and verified. " +
		"Give a concise title and a summary body of what you changed and why."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{"type": "string", "description": "PR title (also the commit subject)"},
			"body":  map[string]any{"type": "string", "description": "PR body: what changed and why"},
			"base":  map[string]any{"type": "string", "description": "base branch (default: the repo's default)"},
		},
		"required": []string{"title"},
	}
	return td
}

// withPR wraps a dispatcher so open_pr is handled locally against wt.
func withPR(inner agent.ToolDispatcher, wt *Worktree, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "open_pr" {
			return inner(ctx, tc)
		}
		if wt == nil || wt.Branch == "" {
			return "ERROR: open_pr needs an isolated git branch (run dun on a git repo without --no-worktree)", nil
		}
		var args struct {
			Title, Body, Base string
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		if strings.TrimSpace(args.Title) == "" {
			return "ERROR: open_pr requires a title", nil
		}
		out := openPR(ctx, wt, args.Title, args.Body, args.Base)
		if onCall != nil {
			onCall("open_pr", map[string]any{"title": args.Title, "base": args.Base}, out)
		}
		return out, nil
	}
}

// openPR commits + pushes wt's branch and opens a PR (best-effort on the gh step:
// a push without a GitHub remote still succeeds and is reported).
func openPR(ctx context.Context, wt *Worktree, title, body, base string) string {
	run := func(dir, name string, args ...string) (string, error) {
		c := exec.CommandContext(ctx, name, args...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	if _, err := run(wt.Path, "git", "add", "-A"); err != nil {
		return "ERROR: git add: " + err.Error()
	}
	// Commit (ignore "nothing to commit" — the branch may already have commits).
	commitArgs := []string{"commit", "-m", title}
	if strings.TrimSpace(body) != "" {
		commitArgs = append(commitArgs, "-m", body)
	}
	if out, err := run(wt.Path, "git", commitArgs...); err != nil && !strings.Contains(out, "nothing to commit") {
		return "ERROR: git commit: " + out
	}
	if out, err := run(wt.Path, "git", "push", "-u", "origin", wt.Branch); err != nil {
		return "ERROR: git push (is there a remote?): " + out
	}

	ghArgs := []string{"pr", "create", "--head", wt.Branch, "--title", title, "--body", body}
	if strings.TrimSpace(base) != "" {
		ghArgs = append(ghArgs, "--base", base)
	}
	prOut, err := run(wt.Path, "gh", ghArgs...)
	if err != nil {
		return fmt.Sprintf("Pushed branch %s, but PR create failed:\n%s\nCreate it manually: gh pr create --head %s",
			wt.Branch, prOut, wt.Branch)
	}
	return "Opened pull request: " + prOut
}
