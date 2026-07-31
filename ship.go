package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Ship — rebase, verify, push, integrate.
//
// Like the ship tool: ensures a clean tree, fetches latest, rebases onto the
// parent branch, runs configured checks, pushes, and fast-forwards the local
// base branch. The agent resolves any rebase conflicts before calling again.
//
// This is a LOCAL operation — it touches the worktree branch, origin, and the
// local base branch. No PR is opened (open_pr does that). Opt-in via --ship.

// ShipConfig controls the ship tool's behavior.
type ShipConfig struct {
	// Checks are shell commands run after rebase succeeds and before push.
	// All must exit 0. Empty → no checks.
	Checks []string
	// SkipChecks are command prefixes to skip from Checks. If a check starts
	// with any skip prefix, it is omitted.
	SkipChecks []string
}

func shipToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "ship"
	td.Function.Description = "Ship your work: fetch latest from origin, rebase onto the base branch, " +
		"run configured checks, push, and fast-forward the local base branch. " +
		"Requires a clean tree (commit everything first). If rebase conflicts occur, " +
		"resolve them in the working tree then call ship again with action:\"continue-rebase\". " +
		"Call this when the task is complete and verified."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"description": "ship (default: full flow), continue-rebase (resolve conflicts and continue), skip-checks (push without running checks)",
			},
		},
	}
	return td
}

// withShip wraps a dispatcher so the built-in "ship" tool is handled locally.
func withShip(inner agent.ToolDispatcher, wt *Worktree, cfg *ShipConfig, execFn func(ctx context.Context, command string) string, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "ship" {
			return inner(ctx, tc)
		}
		if wt == nil || wt.Branch == "" {
			return "ERROR: ship needs an isolated git worktree (run dun on a git repo without --no-worktree)", nil
		}
		var args struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		if args.Action == "" {
			args.Action = "ship"
		}

		var result string
		switch args.Action {
		case "continue-rebase":
			result = continueRebase(wt, cfg, execFn)
		case "skip-checks":
			result = shipSkipChecks(wt)
		default:
			result = doShip(ctx, wt, cfg, execFn)
		}

		if onCall != nil {
			onCall("ship", map[string]any{"action": args.Action}, result)
		}
		return result, nil
	}
}

// doShip runs the full ship flow: clean check → fetch → rebase → checks → push → ff local.
func doShip(ctx context.Context, wt *Worktree, cfg *ShipConfig, execFn func(ctx context.Context, command string) string) string {
	// 1. Clean tree check
	if !wt.IsClean() {
		status := wt.UncommittedStatus()
		return fmt.Sprintf("ERROR: uncommitted changes. Commit first, then ship.\n\n%s", status)
	}

	// 2. Fetch latest
	fetchOut := wt.Fetch()
	if strings.HasPrefix(fetchOut, "git fetch:") {
		return fmt.Sprintf("Fetch failed: %s\nShip cannot proceed without fetching.", fetchOut)
	}

	// 3. Rebase onto origin/<baseBranch>
	upstream := "origin/" + wt.BaseBranch
	rebaseOut := wt.RebaseOnto(upstream)
	if strings.Contains(rebaseOut, "git rebase:") {
		// Rebase failed — likely conflicts. Tell the agent to fix them.
		status := wt.UncommittedStatus()
		return fmt.Sprintf("Rebase onto %s has conflicts. Fix them, then call ship with action:\"continue-rebase\".\n\n%s\n\nStatus:\n%s",
			upstream, rebaseOut, status)
	}

	// 4. Run checks (if configured)
	if cfg != nil && len(cfg.Checks) > 0 {
		checkResult := runChecks(ctx, cfg, execFn)
		if checkResult != "" {
			return fmt.Sprintf("Checks failed. Fix the issues, commit, then ship again.\n\n%s", checkResult)
		}
	}

	// 5. Push
	pushOut := wt.PushWithUpstream()
	if strings.HasPrefix(pushOut, "git push:") {
		return fmt.Sprintf("Push failed: %s", pushOut)
	}

	// 6. Fast-forward local base branch
	ffOut := wt.FastForwardLocal()
	if strings.Contains(ffOut, "merge --ff-only:") {
		// ff-only failed — local base has diverged. This is OK; the push
		// succeeded and the work is safe. Just note it.
		return fmt.Sprintf("Shipped! Branch %s pushed to origin.\n\nNote: local %s could not be fast-forwarded (%s). "+
			"Run 'git pull' in your main checkout to sync.\nPush output:\n%s",
			wt.Branch, wt.BaseBranch, ffOut, pushOut)
	}

	// 7. Clean up — delete the shipped branch (local + remote)
	delOut := wt.DeleteBranch()
	if strings.Contains(delOut, "delete") && !strings.Contains(delOut, "Branch") {
		// Error deleting — note it but don't fail; the work is safe on main.
		return fmt.Sprintf("Shipped! Branch %s rebased onto %s, pushed, and local %s fast-forwarded.\n\n%s\n\nNote: branch cleanup: %s",
			wt.Branch, upstream, wt.BaseBranch, pushOut, delOut)
	}

	return fmt.Sprintf("Shipped! Branch %s rebased onto %s, pushed, local %s fast-forwarded, and branch cleaned up.\n\n%s",
		wt.Branch, upstream, wt.BaseBranch, pushOut)
}

// continueRebase continues a rebase after the agent has resolved conflicts.
func continueRebase(wt *Worktree, cfg *ShipConfig, execFn func(ctx context.Context, command string) string) string {
	out := wt.RebaseContinue()
	if strings.Contains(out, "git rebase --continue:") {
		status := wt.UncommittedStatus()
		return fmt.Sprintf("Rebase continue failed. There may be more conflicts to resolve.\n\n%s\n\nStatus:\n%s", out, status)
	}

	// Rebase succeeded — run checks then push + ff
	if cfg != nil && len(cfg.Checks) > 0 {
		checkResult := runChecks(context.Background(), cfg, execFn)
		if checkResult != "" {
			return fmt.Sprintf("Checks failed after rebase. Fix, commit, then ship again.\n\n%s", checkResult)
		}
	}

	pushOut := wt.PushWithUpstream()
	if strings.HasPrefix(pushOut, "git push:") {
		// Force push may be needed after rebase
		forceOut := wt.forcePush()
		if strings.HasPrefix(forceOut, "git push:") {
			return fmt.Sprintf("Push failed (even with force): %s\n%s", pushOut, forceOut)
		}
		pushOut = forceOut
	}

	ffOut := wt.FastForwardLocal()

	// Clean up shipped branch
	delOut := wt.DeleteBranch()

	return fmt.Sprintf("Rebase continued and shipped! Branch %s pushed.\n\n%s\n%s\n%s",
		wt.Branch, pushOut, ffOut, delOut)
}

// shipSkipChecks pushes without running checks (agent used --force equivalent).
func shipSkipChecks(wt *Worktree) string {
	if !wt.IsClean() {
		status := wt.UncommittedStatus()
		return fmt.Sprintf("ERROR: uncommitted changes. Commit first, then ship.\n\n%s", status)
	}

	pushOut := wt.PushWithUpstream()
	if strings.HasPrefix(pushOut, "git push:") {
		return fmt.Sprintf("Push failed: %s", pushOut)
	}

	ffOut := wt.FastForwardLocal()

	// Clean up shipped branch
	delOut := wt.DeleteBranch()

	return fmt.Sprintf("Pushed (checks skipped). Branch %s on origin.\n\n%s\n%s\n%s",
		wt.Branch, pushOut, ffOut, delOut)
}

func (w *Worktree) forcePush() string {
	if w.repoRoot == "" {
		return "not a git repo"
	}
	out, err := git("", "-C", w.Path, "push", "--force-with-lease", "origin", w.Branch)
	if err != nil {
		return "git push: " + err.Error()
	}
	return strings.TrimSpace(out)
}

func runChecks(ctx context.Context, cfg *ShipConfig, execFn func(ctx context.Context, command string) string) string {
	var failures []string
	for _, check := range cfg.Checks {
		// Skip if matches any skip prefix
		skipped := false
		for _, skip := range cfg.SkipChecks {
			if strings.HasPrefix(check, skip) {
				skipped = true
				break
			}
		}
		if skipped {
			continue
		}
		result := execFn(ctx, check)
		if strings.Contains(result, "[exit:") {
			failures = append(failures, fmt.Sprintf("FAILED: %s\n%s", check, result))
		}
	}
	if len(failures) > 0 {
		return strings.Join(failures, "\n\n")
	}
	return ""
}
