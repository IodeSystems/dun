---
question: Can I use git stash to temporarily shelve changes in this repo?
date: 2026-10-21
session: 20261021-072743
commit: 78ef844
public: false
---
Never use `git stash` in this repo — a stash once silently lost untracked work. When you need to shelve work mid-task, commit it to a WIP branch instead.
