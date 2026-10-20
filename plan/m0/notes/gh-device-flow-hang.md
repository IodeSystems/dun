---
question: Why does ship print the `gh pr create` line instead of running gh?
source: plan/done.md (2026-07-31)
links: [[exec-every-command-is-a-job]]
---
A live session sat idle 14 minutes: the model ran `gh auth login` through exec,
and gh (stdin /dev/null, stdout a pipe, no tty) started an OAuth DEVICE FLOW and
polled GitHub for a code printed into a pipe nobody read. gh 2.45 does not gate
the device flow on CanPrompt(). Found with `ps --forest` + `/proc/PID/fd`. So gh
is out of ship, and foreground exec dies at 5m.
