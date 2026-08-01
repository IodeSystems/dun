package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

func mkCall(name, args string) llm.ToolCall {
	var tc llm.ToolCall
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

// newShipRepo builds a repo with a real bare "origin" and returns both plus the
// base branch name (git's default differs by version, so it is read, not
// assumed).
func newShipRepo(t *testing.T) (origin, repo, base string) {
	t.Helper()
	origin = t.TempDir()
	gitrun(t, origin, "init", "--bare", "-q")

	repo = t.TempDir()
	gitrun(t, repo, "init", "-q")
	gitrun(t, repo, "config", "user.email", "t@t")
	gitrun(t, repo, "config", "user.name", "t")
	gitrun(t, repo, "remote", "add", "origin", origin)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "init")
	gitrun(t, repo, "push", "-q", "-u", "origin", "HEAD")

	b, _ := git("", "-C", repo, "rev-parse", "--abbrev-ref", "HEAD")
	return origin, repo, strings.TrimSpace(b)
}

// shipWorktree makes a session worktree with one committed change on it.
func shipWorktree(t *testing.T, repo string) *Worktree {
	t.Helper()
	wt, isRepo, err := NewWorktree(repo, nil)
	if err != nil || !isRepo {
		t.Fatalf("NewWorktree: %v", err)
	}
	t.Cleanup(wt.Cleanup)
	os.WriteFile(filepath.Join(wt.Path, "feature.txt"), []byte("new\n"), 0o644)
	gitrun(t, wt.Path, "add", ".")
	gitrun(t, wt.Path, "commit", "-qm", "feat: add feature")
	return wt
}

func okExec(context.Context, string) ExecResult { return ExecResult{Output: "ok"} }

// failExec is a check that fails the way a real one does: a non-zero code.
func failExec(out string, code int) func(context.Context, string) ExecResult {
	return func(context.Context, string) ExecResult { return ExecResult{Output: out, Code: code} }
}

// ── policy ─────────────────────────────────────────────────────────

// A nil config must be usable, because most repos will never write one.
func TestShipConfig_Defaults(t *testing.T) {
	var c *ShipConfig
	if got := c.defaultMode(); got != ShipPush {
		t.Errorf("default mode = %q, want push", got)
	}
	for _, m := range allShipModes {
		if !c.permits(m) {
			t.Errorf("nil config must permit %q", m)
		}
	}
	if c.checkTimeout() != defaultCheckTimeout {
		t.Errorf("timeout = %v, want %v", c.checkTimeout(), defaultCheckTimeout)
	}
}

// allow is the policy surface — a sub-agent gets verify and nothing else, and
// the default has to fall inside what is permitted rather than pointing at a
// mode the repo just forbade.
func TestShipConfig_AllowConstrainsDefault(t *testing.T) {
	c := &ShipConfig{Allow: []ShipMode{ShipVerify}}
	if c.permits(ShipPush) || c.permits(ShipPR) {
		t.Error("allow:[verify] must forbid push and pr")
	}
	if got := c.defaultMode(); got != ShipVerify {
		t.Errorf("default = %q, want verify (push is not permitted)", got)
	}
	// An explicitly configured default that is not permitted is a config bug;
	// falling back beats shipping in a mode the repo forbade.
	c = &ShipConfig{Allow: []ShipMode{ShipVerify, ShipPR}, Default: ShipPush}
	if got := c.defaultMode(); got == ShipPush {
		t.Error("a default outside allow must not be used")
	}
}

func TestWithShip_RejectsUnpermittedMode(t *testing.T) {
	cfg := &ShipConfig{Allow: []ShipMode{ShipVerify}}
	_, repo, _ := newShipRepo(t)
	wt := shipWorktree(t, repo)
	d := withShip(agentDispatch(func(n string) string { return "MCP:" + n }), wt, cfg, okExec, nil)
	out, _ := d(context.Background(), mkCall("ship", `{"mode":"push"}`))
	if !strings.Contains(out, "not permitted") || !strings.Contains(out, "verify") {
		t.Fatalf("an unpermitted mode must be refused and name what is allowed: %q", out)
	}
}

func TestWithShip_RequiresRepo(t *testing.T) {
	d := withShip(agentDispatch(func(n string) string { return "MCP:" + n }), nil, nil, okExec, nil)
	out, _ := d(context.Background(), mkCall("ship", `{}`))
	if !strings.Contains(out, "git repository") {
		t.Fatalf("ship without a repo should error: %q", out)
	}
}

// ── check waves ────────────────────────────────────────────────────

// The whole point of a wave: its commands run at the same time. Proven by
// making them wait for each other — serial execution cannot satisfy this.
func TestRunChecks_WaveRunsInParallel(t *testing.T) {
	cfg := &ShipConfig{Checks: []map[string]string{
		{"compile": "build", "lint": "lint"},
	}}
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	go func() { <-arrived; <-arrived; close(release) }()

	exec := func(ctx context.Context, cmd string) ExecResult {
		arrived <- struct{}{}
		select {
		case <-release:
			return ExecResult{Output: "ok"}
		case <-time.After(3 * time.Second):
			return ExecResult{Output: cmd + " ran alone — the wave is serial", Code: 1}
		}
	}
	if fail := runChecks(context.Background(), cfg, exec); fail != "" {
		t.Fatalf("%s", fail)
	}
}

// A wave completes before it reports: two broken checks are two things to fix
// in ONE turn, not a second failure discovered after fixing the first.
func TestRunChecks_ReportsEveryFailureInAWave(t *testing.T) {
	cfg := &ShipConfig{Checks: []map[string]string{
		{"compile": "build", "lint": "lint", "vet": "vet"},
	}}
	exec := func(_ context.Context, cmd string) ExecResult {
		if cmd == "vet" {
			return ExecResult{Output: "clean"}
		}
		return ExecResult{Output: "boom", Code: 1}
	}
	fail := runChecks(context.Background(), cfg, exec)
	for _, want := range []string{"compile", "lint"} {
		if !strings.Contains(fail, "FAILED "+want) {
			t.Errorf("report is missing %q:\n%s", want, fail)
		}
	}
	if strings.Contains(fail, "FAILED vet") {
		t.Errorf("a passing check must not be reported as failed:\n%s", fail)
	}
	// Sorted, or two identical runs produce different text.
	if strings.Index(fail, "FAILED compile") > strings.Index(fail, "FAILED lint") {
		t.Errorf("failures must be sorted by name:\n%s", fail)
	}
}

// Across waves it fails FAST: there is no point testing code that will not compile.
func TestRunChecks_LaterWavesSkippedAfterAFailure(t *testing.T) {
	cfg := &ShipConfig{Checks: []map[string]string{
		{"compile": "build"},
		{"smoke": "test"},
	}}
	var ran []string
	exec := func(_ context.Context, cmd string) ExecResult {
		ran = append(ran, cmd)
		return ExecResult{Output: "boom", Code: 1}
	}
	fail := runChecks(context.Background(), cfg, exec)
	if !strings.Contains(fail, "stage 1 of 2") {
		t.Errorf("the report must name the stage that failed:\n%s", fail)
	}
	if len(ran) != 1 || ran[0] != "build" {
		t.Errorf("wave 2 must not run after wave 1 failed; ran=%v", ran)
	}
}

func TestRunChecks_SkipByName(t *testing.T) {
	cfg := &ShipConfig{
		Skip:   []string{"lint"},
		Checks: []map[string]string{{"compile": "build", "lint": "lint"}},
	}
	var ran []string
	exec := func(_ context.Context, cmd string) ExecResult {
		ran = append(ran, cmd)
		return ExecResult{Output: "ok"}
	}
	if fail := runChecks(context.Background(), cfg, exec); fail != "" {
		t.Fatalf("%s", fail)
	}
	if len(ran) != 1 || ran[0] != "build" {
		t.Errorf("skip should have dropped lint by name; ran=%v", ran)
	}
}

// "Verified" with nothing configured is a claim ship has not earned.
func TestCheckSummary_SaysWhenNothingRan(t *testing.T) {
	if s := checkSummary(nil); !strings.Contains(s, "NO checks") {
		t.Errorf("an unconfigured ship must admit it verified nothing: %q", s)
	}
}

// ── pipeline ───────────────────────────────────────────────────────

func TestShip_RefusesDirtyTree(t *testing.T) {
	_, repo, _ := newShipRepo(t)
	wt := shipWorktree(t, repo)
	os.WriteFile(filepath.Join(wt.Path, "stray.txt"), []byte("uncommitted\n"), 0o644)

	out := doShip(context.Background(), wt, nil, okExec, ShipPush)
	if !strings.Contains(out, "uncommitted changes") {
		t.Fatalf("an untracked file is work the agent forgot to add — ship must refuse: %q", out)
	}
	if branches, _ := git("", "-C", repo, "ls-remote", "--heads", "origin", wt.Branch); strings.TrimSpace(branches) != "" {
		t.Error("nothing may reach origin when the tree is dirty")
	}
}

func TestShip_PushLandsTheBranch(t *testing.T) {
	origin, repo, _ := newShipRepo(t)
	wt := shipWorktree(t, repo)

	out := doShip(context.Background(), wt, nil, okExec, ShipPush)
	if !strings.Contains(out, "Shipped") {
		t.Fatalf("push mode should land the branch: %q", out)
	}
	files, _ := git("", "-C", origin, "ls-tree", "--name-only", wt.Branch)
	if !strings.Contains(files, "feature.txt") {
		t.Fatalf("the commit did not reach origin; files=%q\n%s", files, out)
	}
}

// The invariant: a failed check means nothing lands.
func TestShip_FailedCheckPushesNothing(t *testing.T) {
	_, repo, _ := newShipRepo(t)
	wt := shipWorktree(t, repo)
	cfg := &ShipConfig{Checks: []map[string]string{{"compile": "build"}}}
	exec := failExec("undefined: x", 1)

	out := doShip(context.Background(), wt, cfg, exec, ShipPush)
	if !strings.Contains(out, "Checks failed") {
		t.Fatalf("want a check failure, got %q", out)
	}
	if heads, _ := git("", "-C", repo, "ls-remote", "--heads", "origin", wt.Branch); strings.TrimSpace(heads) != "" {
		t.Fatal("a branch whose checks failed reached origin")
	}
}

func TestShip_VerifyPushesNothing(t *testing.T) {
	_, repo, _ := newShipRepo(t)
	wt := shipWorktree(t, repo)

	out := doShip(context.Background(), wt, nil, okExec, ShipVerify)
	if !strings.Contains(out, "Nothing pushed") {
		t.Fatalf("verify must say it pushed nothing: %q", out)
	}
	if heads, _ := git("", "-C", repo, "ls-remote", "--heads", "origin", wt.Branch); strings.TrimSpace(heads) != "" {
		t.Fatal("verify mode pushed a branch")
	}
}

// baseWorktree models a --no-worktree session: HEAD is the base branch itself.
func baseWorktree(t *testing.T, repo, base string) *Worktree {
	t.Helper()
	wt := &Worktree{Path: repo, BaseBranch: base, repoRoot: repo}
	os.WriteFile(filepath.Join(repo, "direct.txt"), []byte("straight to main\n"), 0o644)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "direct commit on base")
	return wt
}

// Commits made directly on the branch dun started on — the ordinary case for a
// --no-worktree session. It ships. dun never switches branches, so the branch
// HEAD is on IS the branch being shipped; refusing it was refusing the job, and
// in an in-place session "head == base" is true by construction.
func TestShip_ShipsTheBranchYouAreOn(t *testing.T) {
	origin, repo, base := newShipRepo(t)
	wt := baseWorktree(t, repo, base)

	out := doShip(context.Background(), wt, nil, okExec, ShipPush)
	if !strings.Contains(out, "Shipped") {
		t.Fatalf("the branch you are on is the branch you ship: %q", out)
	}
	files, _ := git("", "-C", origin, "ls-tree", "--name-only", base)
	if !strings.Contains(files, "direct.txt") {
		t.Fatalf("the base branch did not reach origin; files=%q", files)
	}
}

// pr mode pushes and then STOPS. dun does not invoke gh: an expired gh token
// does not fail, it starts an OAuth device flow and polls for a code printed
// into a pipe nobody reads — a 14-minute hang indistinguishable from slow work.
// The handoff has to name the command a human runs, or the branch is on origin
// with no indication of what is left to do.
func TestShip_PRModePushesThenHandsOff(t *testing.T) {
	_, repo, base := newShipRepo(t)
	wt := shipWorktree(t, repo)

	out := doShip(context.Background(), wt, nil, okExec, ShipPR)
	if !strings.Contains(out, "gh pr create --head "+wt.Branch+" --base "+base) {
		t.Fatalf("the handoff must be a runnable command: %q", out)
	}
	if strings.Contains(out, "Opened pull request") {
		t.Fatalf("dun must not claim to have opened a PR it never opened: %q", out)
	}
	heads, _ := git("", "-C", repo, "ls-remote", "--heads", "origin", wt.Branch)
	if strings.TrimSpace(heads) == "" {
		t.Fatal("pr mode still has to push the branch — the handoff is worthless without it")
	}
}

func TestShip_OnBaseBranchCannotOpenAPR(t *testing.T) {
	_, repo, base := newShipRepo(t)
	wt := baseWorktree(t, repo, base)

	out := doShip(context.Background(), wt, nil, okExec, ShipPR)
	if !strings.Contains(out, "cannot be opened") {
		t.Fatalf("a PR from base onto itself is not a thing: %q", out)
	}
}

func TestShip_NothingToShip(t *testing.T) {
	_, repo, base := newShipRepo(t)
	wt := &Worktree{Path: repo, BaseBranch: base, repoRoot: repo}
	out := doShip(context.Background(), wt, nil, okExec, ShipPush)
	if !strings.Contains(out, "Nothing to ship") {
		t.Fatalf("an already-pushed branch has nothing to do: %q", out)
	}
}

// The hard case: origin moves WHILE the checks run, so what passed is not what
// would land. Ship must notice and verify again against the new base.
func TestShip_ReVerifiesWhenBaseMovesDuringChecks(t *testing.T) {
	origin, repo, base := newShipRepo(t)
	wt := shipWorktree(t, repo)

	// A second clone stands in for "somebody else pushed", so the move happens
	// entirely outside the worktree under test.
	other := t.TempDir()
	gitrun(t, other, "clone", "-q", origin, ".")
	gitrun(t, other, "config", "user.email", "o@o")
	gitrun(t, other, "config", "user.name", "o")

	moved := false
	exec := func(context.Context, string) ExecResult {
		if !moved {
			moved = true
			os.WriteFile(filepath.Join(other, "upstream.txt"), []byte("theirs\n"), 0o644)
			gitrun(t, other, "add", ".")
			gitrun(t, other, "commit", "-qm", "upstream moved")
			gitrun(t, other, "push", "-q", "origin", "HEAD:"+base)
		}
		return ExecResult{Output: "ok"}
	}
	cfg := &ShipConfig{Checks: []map[string]string{{"compile": "build"}}}

	out := doShip(context.Background(), wt, cfg, exec, ShipPush)
	if !strings.Contains(out, "moved during the checks") {
		t.Fatalf("a base that moved mid-check must be noticed: %q", out)
	}
	if !strings.Contains(out, "Shipped") {
		t.Fatalf("after re-verifying it should still ship: %q", out)
	}
	// And the landed branch must contain BOTH the upstream commit and ours.
	files, _ := git("", "-C", origin, "ls-tree", "--name-only", wt.Branch)
	for _, want := range []string{"feature.txt", "upstream.txt"} {
		if !strings.Contains(files, want) {
			t.Errorf("shipped branch is missing %q; files=%q", want, files)
		}
	}
}

// A conflicted rebase is a state git already knows about — the model should not
// have to declare it by picking the right action string.
func TestShip_ResumesAConflictedRebase(t *testing.T) {
	_, repo, base := newShipRepo(t)

	// Upstream and the session edit the same line.
	wt := &Worktree{Path: repo, BaseBranch: base, repoRoot: repo}
	if wt.RebaseInProgress() {
		t.Fatal("a fresh repo is not mid-rebase")
	}
	gitrun(t, repo, "checkout", "-q", "-b", "conflicting")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("ours\n"), 0o644)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "ours")
	gitrun(t, repo, "checkout", "-q", base)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("theirs\n"), 0o644)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "theirs")
	gitrun(t, repo, "push", "-q", "origin", "HEAD:"+base)
	gitrun(t, repo, "checkout", "-q", "conflicting")

	wt = &Worktree{Path: repo, Branch: "conflicting", BaseBranch: base, repoRoot: repo}
	out := doShip(context.Background(), wt, nil, okExec, ShipPush)
	if !strings.Contains(out, "conflict") {
		t.Fatalf("want a conflict report, got %q", out)
	}
	if !wt.RebaseInProgress() {
		t.Fatal("the conflicted rebase should be left in place for the agent to resolve")
	}

	// The agent resolves it; a bare ship call must pick up where git left off.
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("resolved\n"), 0o644)
	out = doShip(context.Background(), wt, nil, okExec, ShipVerify)
	if wt.RebaseInProgress() {
		t.Fatalf("ship should have resumed and finished the rebase: %q", out)
	}
	if !strings.Contains(out, "Verified") {
		t.Fatalf("want a verified result after the resume, got %q", out)
	}
}

// ── config layering ────────────────────────────────────────────────

func TestLoadShip_LayersFieldByField(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ProjectServersFile), []byte(`{
	  "ship": {"allow":["verify","pr"], "default":"pr",
	           "checks":[{"compile":"go build ./..."}]}
	}`), 0o644)
	os.WriteFile(filepath.Join(dir, LocalServersFile), []byte(`{
	  "ship": {"skip":["compile"]}
	}`), 0o644)

	c := LoadShip(dir)
	if c == nil {
		t.Fatal("no ship config loaded")
	}
	if c.defaultMode() != ShipPR {
		t.Errorf("default = %q, want pr", c.defaultMode())
	}
	if len(c.Checks) != 1 {
		t.Errorf("the local layer must not have erased the project's checks: %v", c.Checks)
	}
	if !c.skipped("compile") {
		t.Error("the local layer's skip was dropped")
	}
	if c.permits(ShipPush) {
		t.Error("allow from the project layer was lost")
	}
}

// A later layer overrides what it states and inherits what it does not.
func TestMergeShip_LaterLayerOverridesWhatItStates(t *testing.T) {
	got := mergeShip(ShipConfig{Default: ShipPush}, ShipConfig{Default: ShipVerify})
	if got.Default != ShipVerify {
		t.Errorf("a later layer must win where it speaks, got %q", got.Default)
	}
	got = mergeShip(ShipConfig{Default: ShipPush}, ShipConfig{CheckTimeout: "1m"})
	if got.Default != ShipPush {
		t.Error("an unstated field must be inherited, not reset")
	}
}

// A feature branch with its OWN upstream rebases onto that upstream, not onto
// the base. This is the case the old head==base rule could not express: dun
// never switches branches, so "which branch am I integrating with" is answered
// by the branch's tracking ref, not by comparing names.
func TestShip_RebasesOntoTheBranchesOwnUpstream(t *testing.T) {
	origin, repo, base := newShipRepo(t)

	gitrun(t, repo, "switch", "-q", "-c", "feature-x")
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("one\n"), 0o644)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "feat: one")
	gitrun(t, repo, "push", "-q", "-u", "origin", "feature-x")

	// Someone else advances the FEATURE branch on origin, and the base too.
	other := t.TempDir()
	gitrun(t, other, "clone", "-q", origin, ".")
	gitrun(t, other, "config", "user.email", "t@t")
	gitrun(t, other, "config", "user.name", "t")
	gitrun(t, other, "switch", "-q", "feature-x")
	os.WriteFile(filepath.Join(other, "theirs.txt"), []byte("theirs\n"), 0o644)
	gitrun(t, other, "add", ".")
	gitrun(t, other, "commit", "-qm", "feat: theirs")
	gitrun(t, other, "push", "-q", "origin", "feature-x")

	// Our own second commit, made locally.
	os.WriteFile(filepath.Join(repo, "g.txt"), []byte("two\n"), 0o644)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "feat: two")

	wt := &Worktree{Path: repo, BaseBranch: base, repoRoot: repo}
	out := doShip(context.Background(), wt, nil, okExec, ShipPush)
	if !strings.Contains(out, "Shipped") {
		t.Fatalf("a tracked feature branch should ship: %q", out)
	}
	// Their commit survives, which is only true if we rebased onto
	// origin/feature-x rather than onto the base.
	files, _ := git("", "-C", origin, "ls-tree", "--name-only", "feature-x")
	for _, want := range []string{"f.txt", "g.txt", "theirs.txt"} {
		if !strings.Contains(files, want) {
			t.Errorf("%s missing from origin/feature-x; files=%q", want, files)
		}
	}
}

// A branch nobody has ever pushed has nothing to verify against. With no
// upstream and no remote base to fall back on, the checks ARE ship — it must
// not refuse, and it must not claim to have rebased onto something.
func TestShip_UntrackedBranchRunsChecksAndPushes(t *testing.T) {
	origin, repo, _ := newShipRepo(t)

	gitrun(t, repo, "switch", "-q", "-c", "brand-new")
	os.WriteFile(filepath.Join(repo, "n.txt"), []byte("new\n"), 0o644)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "feat: brand new")

	wt := &Worktree{Path: repo, BaseBranch: "brand-new", repoRoot: repo}
	if out := doShip(context.Background(), wt, nil, okExec, ShipVerify); !strings.Contains(out, "no upstream") {
		t.Errorf("verify should say there was nothing to rebase onto: %q", out)
	}
	out := doShip(context.Background(), wt, nil, okExec, ShipPush)
	if !strings.Contains(out, "Shipped") {
		t.Fatalf("an untracked branch should still ship: %q", out)
	}
	files, _ := git("", "-C", origin, "ls-tree", "--name-only", "brand-new")
	if !strings.Contains(files, "n.txt") {
		t.Fatalf("the new branch did not reach origin; files=%q", files)
	}
}
