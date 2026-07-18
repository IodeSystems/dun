# ttydrive

Drive and inspect a terminal UI **non-interactively**. Runs a program in a
pseudo-terminal of a fixed size, feeds it a script of keystrokes on stdin, and
dumps the emulated screen as plain text (a vt10x terminal emulator parses the
child's output into a grid, so you see readable text, not escape codes).

Built for driving dun's Bubble Tea UIs from tests / CI / an agent, but works on
any TTY program — inline and alt-screen.

## Build

```sh
go build -o ttydrive .   # nested module; not part of dun's build
```

## Use

```sh
printf 'waitfor ready\ndump\nsend /\nwait 300\ndump\n' \
  | ttydrive --cols 100 --rows 30 --wait-timeout 40 dun -tui --workspace ./x
```

Flags: `--cols` `--rows` (terminal size), `--wait-timeout` (seconds, bump for
UIs slow to boot), `--settle` (ms before the implicit final dump).

## Script directives (stdin, one per line; `#` and blanks ignored)

| directive | effect |
|---|---|
| `send <text>` / `type <text>` | write text literally (`\n \t \e \r \\` honored) |
| `key <name>…` | send named keys: `enter tab esc space up down left right backspace delete home end pgup pgdn ctrl-c ctrl-<x>` |
| `wait <ms>` | sleep |
| `waitfor <substring>` | poll the screen until it appears (`--wait-timeout`) |
| `resize <cols> <rows>` | resize the terminal (delivers SIGWINCH) |
| `dump` | print the current screen |

At EOF it settles briefly and prints a final screen. On exit it kills the
child's whole process group.

## Note

When grepping the output, don't filter on a run of `─` — a TUI's own divider
line looks like the dump's frame border. Match the corners (`┌` / `└`) instead.
