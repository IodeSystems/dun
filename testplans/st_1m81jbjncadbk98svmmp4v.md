# Test plan: st_1m81jbjncadbk98svmmp4v — DunHome switch feasibility

## Research question (u1)

Is it safe to switch `dunconfig.DunHome`, `tui/render_script.go renderersDir`,
and `dundcli/cli.go loader.load` to `dunhome.Dir()`?

**Answer: yes.** `dunhome.Dir()` differs from the three sites only when
`DUN_HOME` is unset *under test* (it returns `/tmp/dun-test-home-<pid>` via a
`testing.Testing()` branch). All three packages pin `DUN_HOME` in every test,
so the branch is unreachable. In production (`DUN_HOME` unset, not testing)
all four return `~/.dun` identically. `dunhome` is a leaf package
(`fmt`, `os`, `path/filepath`, `testing`) — no import cycle.

## Block 1: all four touched packages pass their test suites

The three target packages and the `dunhome` leaf must all pass with no
changes (the switch is a one-liner per site; this confirms the baseline).

```sh
set -e
go test ./internal/dunhome/... ./internal/dunconfig/... ./internal/dundcli/... ./internal/tui/...
```

## Block 2: code inspection — the three sites and the leaf

Shows the three inline `DUN_HOME` reads that will become one-liners, and
the `dunhome.Dir` function they will call. Confirms the env branch is
identical and the only divergence is the `testing.Testing()` branch.

```sh produces=sites.go inputs=internal/dunhome/dunhome.go,internal/dunconfig/config.go,internal/tui/render_script.go,internal/dundcli/cli.go
set -e
mkdir -p "$OUT"
{
  echo "=== internal/dunhome/dunhome.go (the leaf) ==="
  cat internal/dunhome/dunhome.go
  echo ""
  echo "=== internal/dunconfig/config.go DunHome (target site 1) ==="
  grep -n -A8 '^func DunHome' internal/dunconfig/config.go
  echo ""
  echo "=== internal/tui/render_script.go renderersDir (target site 2) ==="
  grep -n -A8 '^func renderersDir' internal/tui/render_script.go
  echo ""
  echo "=== internal/dundcli/cli.go loader.load (target site 3) ==="
  grep -n -A8 '^func (l \*loader) load' internal/dundcli/cli.go
} > "$OUT/sites.go"
cat "$OUT/sites.go"
```
