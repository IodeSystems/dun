# dun

Work items are aw4 issues in project dun. This repo has no plan/ directory;
this overrides the global Planning rule.

- File a finding or a next step as an issue, run from
  ~/local/src/iodesystems/aw4 (its --home is relative):
  `aw4 issue ask "<one line>" --body "$(cat <file>)" --project dun --by <you>`.
  Write the body to a file with a quoted heredoc: a backtick in an inline
  --body is command-substituted.
- Design reasoning (why the code is the way it is) lives in docs/:
  docs/design.md, docs/decisions.md and the topic files beside them;
  docs/notes/ holds short answered questions.
- Testing: while working, run `make test-changed` (the packages changed
  vs main, BASE=… for another base, and the packages that import them) or
  `make test-pkg P=./internal/tui/...`. Run the full `make test` once,
  before a merge, and `make test-pinned` (tests against release/deps.json's
  tools) before a merge or release. Timings: docs/notes/test-times.md.
- Code comments that say "item N" or "plan item N" refer to the removed plan:
  `git show cb0e5bd:plan/plan.md` (and `plan/done.md`) has the text.
