---
question: Why is there no foreground exec path — why does every command start as a job?
source: plan/plan.md item 7 (2026-08-26)
links: [[gh-device-flow-hang]]
---
A session sat "working" ten minutes with zero LLM requests, twice in an hour.
Cause, three layers: `exit 7` sent into the persistent shell exits the SHELL,
so the sentinel line never prints; `dead()` read `cmd.ProcessState`, which Go
fills only after `Wait`, and nothing waited; a waiter goroutine then took
`h.mu`, held by `Run`. Fix: every command is a job; it reports inline if it
finishes inside a 30s grace, otherwise it is promoted (number, log, pane row,
notification). Nothing is killed, nothing re-runs.
