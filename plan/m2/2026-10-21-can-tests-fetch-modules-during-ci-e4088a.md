---
question: Can tests fetch modules during CI for this repo?
date: 2026-10-21
session: 20261021-073015
commit: 78ef844
public: false
---
The CI runner for this repo has NO network access. Tests must never fetch modules — vendor them (e.g. `go mod vendor` / vendor directory committed) so CI can build and run offline.
