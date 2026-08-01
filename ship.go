package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Ship — verify, then land.
//
// One invariant, and every decision here answers to it:
//
//	Never push anything that was not verified in exactly the state it will land in.
//
// That is bors' "not rocket science rule", and it is why the order is
// fetch → rebase → checks → push with nothing mutating in between. Verifying
// BEFORE the rebase (the obvious order, and the one ship used to imply) tests a
// tree that will never exist anywhere: code that compiles against the old base
// can fail against the new one. The rebase has to come first so the checks run
// on the commits that are actually going to origin.
//
// The one genuinely hard part is that origin can move between the checks and
// the push, which leaves a branch verified against a base that no longer
// exists. Merge queues (GitHub's, Zuul, bors) exist for exactly this. Without
// one, the honest approximation is to re-fetch immediately after the checks and
// go round again if the base moved — bounded, because an infinitely busy repo
// must fail loudly rather than spin.
//
// MODES are the terminal state, and nothing else varies between them: verify
// stops after the checks, push lands the branch on origin, pr lands it and
// hands the pull request back to a human (see handOffPR — dun does not run gh).
// `allow` in the config is the policy surface —
// a repo says "agents open PRs, they do not push" by listing modes, and a
// sub-agent is handed allow:["verify"] so it can run the whole pipeline and
// land nothing.

// ShipMode is a ship's terminal state.
type ShipMode string

const (
	// ShipVerify runs the pipeline and pushes nothing.
	ShipVerify ShipMode = "verify"
	// ShipPush lands the current branch on origin.
	ShipPush ShipMode = "push"
	// ShipPR lands the branch and hands the pull request off to a human.
	ShipPR ShipMode = "pr"
)

var allShipModes = []ShipMode{ShipVerify, ShipPush, ShipPR}

// ShipConfig is the `ship` section of dun.json / dun.local.json.
type ShipConfig struct {
	// Allow lists the permitted modes. Empty = all of them.
	Allow []ShipMode `json:"allow,omitempty"`
	// Default is the mode used when the model names none. Empty = push (or the
	// first permitted mode, when push is not one).
	Default ShipMode `json:"default,omitempty"`
	// AllowBasePush permits pushing the base branch itself — the
	// commit-straight-to-main case. A pointer so a local layer can turn it back
	// OFF, the same reason ServerSpec.Autostart is one. nil/false = refuse, and
	// tell the agent to move its commits onto a branch: landing on a shared
	// trunk unreviewed should be something a repo opts into, not a fallback.
	AllowBasePush *bool `json:"allowBasePush,omitempty"`
	// CheckTimeout bounds ONE check (a Go duration string). Empty = 10m.
	CheckTimeout string `json:"checkTimeout,omitempty"`
	// Skip names checks to omit, BY NAME.
	Skip []string `json:"skip,omitempty"`
	// Checks are waves: the list is serial, each map runs in parallel. The
	// shape carries the semantics — an array is ordered, an object is not:
	//
	//	[{"compile": "go build ./..."},
	//	 {"lint": "golangci-lint run", "vet": "go vet ./..."},
	//	 {"smoke": "go test -short ./..."}]
	//
	// Empty = no checks, and ship says so rather than implying it verified
	// something.
	Checks []map[string]string `json:"checks,omitempty"`
}

const defaultCheckTimeout = 10 * time.Minute

// maxShipRounds bounds the rebase→check→push loop when origin keeps moving.
// Two retries is generous for a repo one agent is working in; past that the
// right answer is to say the trunk is too busy, not to keep burning check runs.
const maxShipRounds = 3

func (c *ShipConfig) allowed() []ShipMode {
	if c == nil || len(c.Allow) == 0 {
		return allShipModes
	}
	return c.Allow
}

func (c *ShipConfig) permits(m ShipMode) bool {
	for _, a := range c.allowed() {
		if a == m {
			return true
		}
	}
	return false
}

func (c *ShipConfig) defaultMode() ShipMode {
	if c != nil && c.Default != "" && c.permits(c.Default) {
		return c.Default
	}
	if c.permits(ShipPush) {
		return ShipPush
	}
	return c.allowed()[0]
}

func (c *ShipConfig) allowBasePush() bool {
	return c != nil && c.AllowBasePush != nil && *c.AllowBasePush
}

func (c *ShipConfig) checkTimeout() time.Duration {
	if c == nil || c.CheckTimeout == "" {
		return defaultCheckTimeout
	}
	d, err := time.ParseDuration(c.CheckTimeout)
	if err != nil || d <= 0 {
		return defaultCheckTimeout
	}
	return d
}

func (c *ShipConfig) skipped(name string) bool {
	if c == nil {
		return false
	}
	for _, s := range c.Skip {
		if s == name {
			return true
		}
	}
	return false
}

// waveNames is one wave's check names, minus the skipped ones, SORTED. Go
// randomizes map iteration, so without this two identical runs produce
// different text — which reads to the model as a changed result.
func (c *ShipConfig) waveNames(wave map[string]string) []string {
	names := make([]string, 0, len(wave))
	for n := range wave {
		if !c.skipped(n) && strings.TrimSpace(wave[n]) != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func shipToolDef(cfg *ShipConfig) llm.ToolDef {
	modes := cfg.allowed()
	names := make([]string, len(modes))
	for i, m := range modes {
		names[i] = string(m)
	}
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "ship"
	td.Function.Description = "Ship your work: fetch origin, rebase onto the base branch, run the project's " +
		"checks, and land the result. Requires a clean tree — commit everything first. " +
		"If the rebase conflicts, resolve the conflicts in the worktree and call ship again; it resumes. " +
		"Call this when the task is complete. Permitted modes: " + strings.Join(names, ", ") +
		" (default " + string(cfg.defaultMode()) + ")."
	props := map[string]any{
		"mode": map[string]any{
			"type": "string",
			"enum": names,
			"description": "terminal state: verify runs the checks and pushes nothing; push lands the branch; " +
				"pr lands it and reports the command a human runs to open the pull request",
		},
	}
	td.Function.Parameters = map[string]any{"type": "object", "properties": props}
	return td
}

// withShip wraps a dispatcher so the built-in "ship" tool is handled locally.
func withShip(inner agent.ToolDispatcher, wt *Worktree, cfg *ShipConfig, execFn func(ctx context.Context, command string) ExecResult, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "ship" {
			return inner(ctx, tc)
		}
		if wt == nil || !wt.IsRepo() {
			return "ERROR: ship needs a git repository", nil
		}
		var args struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)

		mode := cfg.defaultMode()
		if m := ShipMode(strings.TrimSpace(args.Mode)); m != "" {
			if !cfg.permits(m) {
				names := make([]string, 0, len(cfg.allowed()))
				for _, a := range cfg.allowed() {
					names = append(names, string(a))
				}
				return fmt.Sprintf("ERROR: mode %q is not permitted in this repository. Permitted: %s.",
					m, strings.Join(names, ", ")), nil
			}
			mode = m
		}

		result := doShip(ctx, wt, cfg, execFn, mode)
		if onCall != nil {
			onCall("ship", map[string]any{"mode": string(mode)}, result)
		}
		return result, nil
	}
}

// doShip runs the pipeline: resume → clean → fetch → rebase → checks → (base
// moved? round again) → terminal action.
func doShip(ctx context.Context, wt *Worktree, cfg *ShipConfig, execFn func(ctx context.Context, command string) ExecResult, mode ShipMode) string {
	// Resuming comes FIRST, before anything reads the branch: a stopped rebase
	// leaves HEAD detached, so every other question ("what branch am I on?")
	// has no answer until the rebase is finished.
	//
	// That it is a state, not an argument, is the point. Asking the model to
	// pass action:"continue-rebase" was one more thing for it to get wrong, and
	// git already knows.
	if wt.RebaseInProgress() {
		out := wt.RebaseContinue()
		if strings.Contains(out, "git rebase --continue:") {
			return fmt.Sprintf("Rebase still conflicted. Resolve the remaining conflicts, then call ship again.\n\n%s\n\nStatus:\n%s",
				out, wt.UncommittedStatus())
		}
	}

	base := wt.BaseBranch
	head := wt.CurrentBranch()
	if head == "" {
		return "ERROR: HEAD is detached. Check out a branch before shipping."
	}
	onBase := head == base
	upstream := "origin/" + base

	// The clean-tree gate is first because everything after it assumes the
	// commits ARE the work. An uncommitted file is not shipped, and an
	// UNTRACKED one is the common agent mistake — a new file it forgot to add,
	// which lands as a build that compiles here and breaks on origin.
	if !wt.IsClean() {
		return fmt.Sprintf("ERROR: uncommitted changes. Commit everything (including new files) first, then ship.\n\n%s",
			wt.UncommittedStatus())
	}

	var rounds []string
	for round := 1; ; round++ {
		if out := wt.Fetch(); strings.HasPrefix(out, "git fetch:") {
			return fmt.Sprintf("Fetch failed: %s\nShip cannot verify against a base it could not fetch.", out)
		}
		before := wt.RemoteHead(base)

		if out := wt.RebaseOnto(upstream); strings.Contains(out, "git rebase:") {
			return fmt.Sprintf("Rebase onto %s conflicts. Resolve the conflicts in the worktree, commit nothing "+
				"(git rebase does that), then call ship again — it resumes automatically.\n\n%s\n\nStatus:\n%s",
				upstream, out, wt.UncommittedStatus())
		}

		if fail := runChecks(ctx, cfg, execFn); fail != "" {
			return fail
		}

		// Did the base move while we were checking? If it did, what we just
		// verified is not what would land.
		if out := wt.Fetch(); strings.HasPrefix(out, "git fetch:") {
			return fmt.Sprintf("Checks passed, but the pre-push fetch failed: %s\nNot pushing — the base may have moved.", out)
		}
		if wt.RemoteHead(base) == before {
			break
		}
		rounds = append(rounds, fmt.Sprintf("round %d: %s moved during the checks, re-verifying", round, upstream))
		if round >= maxShipRounds {
			return fmt.Sprintf("Gave up after %d rounds: %s keeps moving while the checks run, so nothing was "+
				"verified against the base it would land on. Try again when the trunk is quieter.\n\n%s",
				maxShipRounds, upstream, strings.Join(rounds, "\n"))
		}
	}

	verified := checkSummary(cfg)
	if len(rounds) > 0 {
		verified += "\n" + strings.Join(rounds, "\n")
	}

	switch mode {
	case ShipVerify:
		return fmt.Sprintf("Verified. Rebased onto %s and %s\nNothing pushed (mode: verify).", upstream, verified)

	case ShipPush:
		if onBase && !cfg.allowBasePush() {
			return fmt.Sprintf("Verified, but NOT pushed: you are on %s, the base branch, and this repository does "+
				"not permit agents to push it directly.\nMove your commits onto a branch "+
				"(git switch -c <name>) and ship again.\n\n%s", base, verified)
		}
		if !wt.NeedsPush(head) {
			return fmt.Sprintf("Nothing to ship — %s already matches origin/%s.\n%s", head, head, verified)
		}
		// A rebase rewrites the branch, so the ref no longer fast-forwards and
		// a plain push is REJECTED. --force-with-lease is the correct first
		// attempt (a compare-and-swap on the ref), not a fallback after failure.
		// The base branch is the exception: it must only ever fast-forward.
		out := wt.Push(head, !onBase)
		if strings.HasPrefix(out, "git push:") {
			return fmt.Sprintf("Checks passed but the push failed: %s", out)
		}
		return fmt.Sprintf("Shipped. %s pushed to origin.\n%s\n\n%s", head, verified, out)

	case ShipPR:
		if onBase {
			return fmt.Sprintf("Verified, but a pull request cannot be opened from %s onto itself. "+
				"Move your commits onto a branch (git switch -c <name>) and ship again.\n\n%s", base, verified)
		}
		if wt.NeedsPush(head) {
			out := wt.Push(head, true)
			if strings.HasPrefix(out, "git push:") {
				return fmt.Sprintf("Checks passed but the push failed: %s", out)
			}
		}
		return fmt.Sprintf("%s\n%s", handOffPR(head, base), verified)
	}
	return "ERROR: unknown ship mode " + string(mode)
}

// checkSummary names what actually ran, because "verified" with no checks
// configured is a claim ship has not earned.
func checkSummary(cfg *ShipConfig) string {
	var names []string
	if cfg != nil {
		for _, wave := range cfg.Checks {
			names = append(names, cfg.waveNames(wave)...)
		}
	}
	if len(names) == 0 {
		return "ran NO checks (none configured — add a `ship.checks` section to dun.json)."
	}
	return "passed: " + strings.Join(names, ", ") + "."
}

// runChecks runs the waves. Serial across waves, parallel within one, and the
// whole wave completes before its failures are reported: if compile and lint
// both fail the model should fix both in one turn, not discover the second
// after fixing the first. Returns "" when everything passed.
func runChecks(ctx context.Context, cfg *ShipConfig, execFn func(ctx context.Context, command string) ExecResult) string {
	if cfg == nil || len(cfg.Checks) == 0 {
		return ""
	}
	timeout := cfg.checkTimeout()
	for i, wave := range cfg.Checks {
		names := cfg.waveNames(wave)
		if len(names) == 0 {
			continue
		}
		outs := make([]ExecResult, len(names))
		var wg sync.WaitGroup
		for j, name := range names {
			wg.Add(1)
			go func(j int, cmd string) {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				// Pass/fail is the EXIT CODE. It used to be
				// strings.Contains(out, "[exit:"), which a check that printed
				// that marker — a test asserting on exec's own output, say —
				// turned into a false failure.
				outs[j] = execFn(cctx, cmd)
			}(j, wave[name])
		}
		wg.Wait()

		var failed []string
		for j, name := range names {
			if outs[j].Failed() {
				failed = append(failed, fmt.Sprintf("FAILED %s: %s\n%s",
					name, wave[name], strings.TrimSpace(outs[j].Render())))
			}
		}
		if len(failed) > 0 {
			return fmt.Sprintf("Checks failed at stage %d of %d. Fix these, commit, then ship again.\n\n%s",
				i+1, len(cfg.Checks), strings.Join(failed, "\n\n"))
		}
	}
	return ""
}

// handOffPR ends a pr-mode ship: the branch is on origin, and the pull request
// is the human's to open.
//
// dun used to shell out to `gh pr view` / `gh pr create` here. It no longer
// invokes gh at all. gh owns an auth lifecycle dun cannot participate in — when
// its stored token expires, `gh` does not fail, it starts an OAuth device flow
// and polls for a code that is printed into a pipe no human is reading. That is
// indistinguishable from slow work, and it is how a session came to sit idle
// for 14 minutes. Printing the command a person can run is strictly more honest
// than a tool that hangs when its credentials rot.
func handOffPR(head, base string) string {
	return fmt.Sprintf("Shipped. %s is on origin.\ndun does not open pull requests — run this yourself:\n"+
		"  gh pr create --head %s --base %s", head, head, base)
}
