---
question: Why commit scaffolding early in this repo, even half-finished?
source: plan/plan.md item 0b (2026-08-01)
links: [[stash-before-cherry-pick]]
---
On 2026-08-01 a cherry-pick run from outside the session stashed the tracked
files and cleaned the untracked ones. `git stash` does not take untracked files,
so ~3000 lines (subagent.go, bgjob.go, their tests, the sub-agent spec) were
lost for good — not on any branch, not in dangling objects. Rule: work that is
not committed does not exist; other agents run git in this repo at any time.
