# ttyrec

Record a terminal program to an asciicast v2 file, and render the recording to
an animated GIF and per-marker PNG + text. Pure Go: a pty (creack/pty), a vt
emulator (charmbracelet/x/vt), and a system monospace font (DejaVu Sans Mono,
then Noto Sans Mono, DejaVu Sans, Symbola, with Go Mono built in as the last
fallback). Box drawing and block elements are drawn, not taken from the font,
so borders meet edge to edge.

```sh
go build -o ttyrec .   # nested module; `make test` runs its tests

ttyrec rec -script ../../ux/walks/onboarding.json -result r.json -o run.cast -- dun
ttyrec render -gif run.gif -marks out/ run.cast
```

- `rec` runs the command at the script's cols×rows (or `-cols`/`-rows`), feeds
  it the script's steps, and writes a `"m"` marker named by each step's id. The
  emulator answers the program's terminal queries, as a real terminal would.
  Without `-script` it forwards stdin until the program exits.
- `render` writes `NN-<marker>.png`, `NN-<marker>.txt` (the keys/commands the
  screen names, then every character on it) and `render.json` (font used, cell
  size, markers). The GIF has a frame per change at most `-fps` a second, and
  any pause longer than `-max-idle` (1.5 s) is cut to it.

## Script (walk) format

```json
{"cols": 100, "rows": 30, "typing_ms": 45, "steps": [
  {"id": "promise-named-step", "promise": "…", "settle_ms": 700, "actions": [
    {"wait_for": "text on screen", "timeout_ms": 15000},
    {"type": "${ENV_VAR} or text", "say": "how it reads in the log"},
    {"key": "enter"},
    {"wait_gone": "spinner text"},
    {"sleep_ms": 300}
  ]}
]}
```

Keys: `enter tab shift+tab esc space backspace delete up down left right home
end pgup pgdn alt+enter ctrl+<letter>`, space-separated for several. A step
ends when the screen has been quiet for `settle_ms` (at most 5 s). A failed
wait still writes the step's marker, so the picture shows where it stuck.
