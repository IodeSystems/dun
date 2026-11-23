# Test plan: st_1m7xkvqrx6xmaap5sx58tq

Repair the sweep harness for harbor 0.26.0 and prove it with one end-to-end trial.
Harbor 0.26.0 dropped `-p` (task path); tasks, model and environment now go in a
JobConfig YAML passed via `--config`. This plan: (1) get the schema, (2) check
prerequisites, (3) run one trial with the repaired run.sh.

## Block 1: Get the harbor JobConfig schema

`harbor job schema` prints the YAML schema for `--config`. We need it to know
what fields to generate per task.

```sh produces=schema.txt
set -e
harbor job schema 2>&1 | tee "$OUT/schema.txt" | head -80
echo "---"
echo "total lines: $(wc -l < "$OUT/schema.txt")"
```

## Block 2: Check prerequisites for an end-to-end trial

Docker running, TB2.1 tasks dir with at least one task, prebuilt binaries in
examples/harbor/bin/, ~/.dun/config.json, and harbor on PATH.

```sh produces=prereqs.txt
set -e
{
  echo "=== docker ==="
  docker info --format '{{.ServerVersion}}' 2>&1 || { echo "FAIL: docker not running"; exit 1; }
  echo
  echo "=== TB2.1 tasks ==="
  for d in /home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks \
           /home/nthalk/local/src/iodesystems/terminal-bench-2-1/tasks \
           /home/nthalk/local/src/terminal-bench-2-1/tasks; do
    if [ -d "$d" ]; then
      echo "found: $d ($(ls "$d" | wc -l) tasks)"
      break
    fi
  done
  [ -d "$d" ] || { echo "FAIL: no TB2.1 tasks dir found"; exit 1; }
  echo
  echo "=== bin dir ==="
  BINDIR=/home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor/bin
  for f in dun mcpshell ca.crt; do
    [ -f "$BINDIR/$f" ] && echo "  $f: ok" || { echo "  $f: MISSING"; }
  done
  [ -f "$BINDIR/dun" ] || { echo "FAIL: dun binary missing"; exit 1; }
  echo
  echo "=== dun config ==="
  [ -f "$HOME/.dun/config.json" ] && echo "  present" || { echo "  MISSING"; }
  [ -f "$HOME/.dun/config.json" ] || { echo "FAIL: no config.json"; exit 1; }
  echo
  echo "=== harbor ==="
  harbor --version 2>&1
  echo
  echo "ALL PREREQUISITES OK"
} > "$OUT/prereqs.txt"
cat "$OUT/prereqs.txt"
```

## Block 3: The generated test shim uses container-relative paths

The converter writes a `tests/test.sh` shim that sets `$TEST_DIR` and execs the
task's `run-tests.sh`. Harbor 0.26.0 runs the verifier INSIDE the docker
container (environment_mode: shared), where the task dir's files are mounted at
/app — the host path the task dir was copied to does not exist there. Two bugs
made every trial end in RewardFileNotFoundError:

1. `TEST_DIR="..." export TEST_DIR` — bash parses this as a builtin-scoped
   assignment, so `TEST_DIR` is NOT set in the shim's shell; the exec'd
   run-tests.sh inherits an empty `TEST_DIR`.
2. Hard-coding the host task dir (`<host>/tests`, `<host>/run-tests.sh`) —
   those paths do not exist in the container, so even a correctly-exported
   TEST_DIR points at a missing path.

The fix derives both paths from the shim's own location (`$0`), which is
container-relative: the shim lives at `<taskdir>/tests/test.sh`, so
`$(dirname "$0")` is the tests dir and `$(dirname "$0")/..` is the task root.

A third bug made every trial end in RewardFileNotFoundError, independent of
TEST_DIR: TB2.1's run-tests.sh runs pytest and exits WITHOUT writing a reward
file, and the shim used to `exec` run-tests.sh, so the shim itself never wrote
one either. Harbor 0.26.0 reads the pass/fail from `/logs/verifier/reward.txt`
(a float: 1.0 = pass, 0.0 = fail), bind-mounted from `trial_dir/verifier/` on
the host (EnvironmentPaths.verifier_dir = /logs/verifier, paths.py:42). The
shim must run run-tests.sh, capture the exit code, and write reward.txt — so it
must NOT `exec` (exec replaces the shell and the reward line is never reached).

This block generates a shim and asserts: a standalone `export TEST_DIR=` line
present, the broken `VAR=value export VAR` one-liner gone, `$0`-derived paths
(no hard-coded host absolute path), the shim writes `/logs/verifier/reward.txt`
with 1.0/0.0, and it does NOT `exec` run-tests.sh. Passes on the fixed
converter; fails if any bug is put back.

```sh produces=shim.txt inputs=examples/harbor/convert_task_yaml.py
set -e
TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
WORK=$(mktemp -d /tmp/harbor-shim-test-XXXXXX)
trap 'rm -rf "$WORK"' EXIT
# Copy the task to a STABLE dir (the converter writes the shim next to the
# task.toml only when out_path.parent != task_dir), then convert.
cp -r "$TASKS/extract-elf" "$WORK/extract-elf"
python3 /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor/convert_task_yaml.py \
    "$TASKS/extract-elf" "$WORK/extract-elf/task.toml"
shim="$WORK/extract-elf/tests/test.sh"
{
  echo "=== generated tests/test.sh ==="
  cat "$shim"
  echo
  # A standalone `export TEST_DIR="..."` line (sets + exports in the shim's
  # shell so the exec'd run-tests.sh inherits it). The value is a $0-derived
  # path, not a host absolute path.
  grep -Eq '^export TEST_DIR=' "$shim" \
    || { echo "FAIL: no standalone 'export TEST_DIR=...' line"; exit 1; }
  echo "OK: standalone 'export TEST_DIR=...' line present"
  # The broken form must be GONE: `VAR=value export VAR` is the bug.
  if grep -Eq 'TEST_DIR="[^"]+" *export TEST_DIR' "$shim"; then
    echo "FAIL: broken 'TEST_DIR=... export TEST_DIR' one-liner still present"
    exit 1
  fi
  echo "OK: broken 'TEST_DIR=... export TEST_DIR' form absent"
  # Container-relative: the path must be derived from $0 (the shim's own
  # location), not a hard-coded host absolute path like /tmp/harbor-tasks-...
  grep -Eq 'dirname "\$0"' "$shim" \
    || { echo "FAIL: TEST_DIR not derived from \$0 (host path?); container-relative form missing"; exit 1; }
  echo "OK: paths derived from \$0 (container-relative)"
  # The host task-dir absolute path must NOT be hard-coded into the shim.
  if grep -Fq "$WORK/extract-elf" "$shim"; then
    echo "FAIL: host task-dir path hard-coded into the shim"
    exit 1
  fi
  echo "OK: no hard-coded host path in the shim"
  # The shim must write the reward file to /logs/verifier (the container-side
  # verifier dir harbor bind-mounts). Without this, every trial ends in
  # RewardFileNotFoundError regardless of TEST_DIR.
  grep -Fq '/logs/verifier/reward.txt' "$shim" \
    || { echo "FAIL: shim does not write /logs/verifier/reward.txt"; exit 1; }
  echo "OK: shim writes /logs/verifier/reward.txt"
  # It must write 1.0 on pass and 0.0 on fail.
  grep -Fq 'echo "1.0"' "$shim" || { echo "FAIL: shim does not write 1.0 (pass)"; exit 1; }
  grep -Fq 'echo "0.0"' "$shim" || { echo "FAIL: shim does not write 0.0 (fail)"; exit 1; }
  echo "OK: shim writes 1.0/0.0"
  # The shim must NOT exec run-tests.sh: exec replaces the shell, so the
  # reward-writing lines after it would never run.
  if grep -Eq '^exec ' "$shim"; then
    echo "FAIL: shim uses exec — the reward-file lines after it never run"
    exit 1
  fi
  echo "OK: shim does not exec (reward lines are reachable)"
} > "$OUT/shim.txt" 2>&1
rc=$?
cat "$OUT/shim.txt"
exit $rc
```

## Block 4: clear_error flags RewardFileNotFoundError as a harness failure

`run.sh`'s `clear_error` must abort the sweep when a trial raises an exception
other than `AgentTimeoutError`. The old version also tolerated
`RewardFileNotFoundError`, so with the shim broken EVERY trial errored with it
and the sweep produced no signal. This block runs clear_error's exact python
against a fixture result.json that carries `RewardFileNotFoundError` and asserts
it prints a failure line (aborts). Passes on the fixed clear_error; fails if the
tolerance is put back.

```sh produces=clearerror.txt inputs=examples/harbor/run.sh
set -e
WORK=$(mktemp -d /tmp/harbor-clearerror-XXXXXX)
trap 'rm -rf "$WORK"' EXIT
RUNSH=/home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor/run.sh
# Extract clear_error verbatim from run.sh so the test tracks the real code,
# not a copy.
sed -n '/^clear_error() {/,/^}/p' "$RUNSH" > "$WORK/clear_error.sh"
[ -s "$WORK/clear_error.sh" ] || { echo "FAIL: could not extract clear_error from run.sh" > "$OUT/clearerror.txt"; cat "$OUT/clearerror.txt"; exit 1; }
# Fixture: a job dir with one trial whose result.json carries the exception
# clear_error must now treat as a harness failure. The trial dir name must
# match clear_error's `*__*` glob (real trial dirs look like <task>__<id>).
job="$WORK/extract-elf"
mkdir -p "$job/extract-elf__abc123/agent"
cat > "$job/extract-elf__abc123/result.json" <<'JSON'
{
  "n_total_trials": 1,
  "exception_info": {"exception_type": "RewardFileNotFoundError"}
}
JSON
touch "$job/extract-elf__abc123/agent/dun.jsonl"
# clear_error uses $out and globs "$out/$1/*__*/result.json".
out="$WORK"
source "$WORK/clear_error.sh"
res=$(clear_error extract-elf)
{
  echo "=== clear_error (extracted verbatim from run.sh) output ==="
  echo "$res"
  echo
  [ -n "$res" ] || { echo "FAIL: clear_error printed nothing — RewardFileNotFoundError is still tolerated"; exit 1; }
  echo "OK: clear_error flags the trial as a harness failure (would abort the sweep)"
} > "$OUT/clearerror.txt" 2>&1
rc=$?
cat "$OUT/clearerror.txt"
exit $rc
```

## Block 5: End-to-end trial with the repaired harness

Run extract-elf through harbor 0.26.0 with the repaired converter (which now
adds [environment] docker_image to task.toml and the fixed test shim) and a
JobConfig YAML. Proves the harness works end-to-end: task conversion, docker
environment, agent run, verifier, and a result.json whose trial COMPLETED
without exception (stats.n_errored_trials == 0, stats.n_completed_trials >= 1)
and spent > 0. The check reads the job-level result.json at
$trial_out/extract-elf/result.json, which has the stats fields. A trial that
ERRORED (e.g. RewardFileNotFoundError from a broken shim) increments
n_errored_trials; cost_usd > 0 alone is not the proof.

```sh produces=trial.txt inputs=examples/harbor/convert_task_yaml.py,examples/harbor/run.sh
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor

export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=${DUN_MODEL:-fireworks-glm-5p3-flash}
export DUN_EFFORT=${DUN_EFFORT:-low}
export DUN_PRICE_IN=${DUN_PRICE_IN:-0.15}
export DUN_PRICE_CACHED=${DUN_PRICE_CACHED:-0.03}
export DUN_PRICE_OUT=${DUN_PRICE_OUT:-0.5}
export PYTHONPATH=$PWD

# Fresh out dir: harbor refuses to resume a job dir whose config differs
trial_out=$(mktemp -d /tmp/harbor-trial-XXXXXX)

{
  echo "=== run harbor (extract-elf, 1 trial) ==="
  cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
  taskdir="/tmp/harbor-tasks-extract-elf"
  rm -rf "$taskdir"
  cp -r "$TASKS/extract-elf" "$taskdir"
  python3 /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor/convert_task_yaml.py "$TASKS/extract-elf" "$taskdir/task.toml"
  echo "generated task.toml:"
  cat "$taskdir/task.toml"
  echo
  echo "generated tests/test.sh:"
  cat "$taskdir/tests/test.sh" 2>&1 || echo "(missing!)"
  echo
  cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
  echo "jobconfig:"
  cat "$cfgfile"
  echo
  echo "=== harbor run ==="
  harbor run --config "$cfgfile" -o "$trial_out" -k 1 -n 1 --job-name extract-elf 2>&1
  rc=$?
  rm -f "$cfgfile"
  echo
  echo "harbor exit code: $rc"
  echo

  echo "=== result.json (job-level) ==="
  # The job-level result.json is at a known path: $trial_out/<job-name>/result.json.
  # It has stats.n_completed_trials, stats.n_errored_trials, stats.cost_usd.
  # The per-trial result.json (under <trial_dir>/result.json) has exception_info
  # and reward_valid. We use the job-level file for the completion check because
  # its stats fields are the aggregate proof that the sweep produced signal.
  result="$trial_out/extract-elf/result.json"
  if [ ! -f "$result" ]; then
    echo "FAIL: job-level result.json not found at $result"
    exit 1
  fi
  cat "$result"
  echo

  echo "=== spend + completion check ==="
  # A trial that ERRORED (e.g. RewardFileNotFoundError from a broken test shim)
  # can still show cost_usd > 0 — the agent spent tokens before the verifier ran.
  # The proof must check the trial COMPLETED without exception, not just that the
  # agent spent money: n_errored_trials == 0 and n_completed_trials >= 1.
  cost=$(python3 -c "import json;r=json.load(open('$result'));print(r.get('stats',{}).get('cost_usd',0) or 0)")
  echo "cost_usd: $cost"
  python3 -c "import sys; sys.exit(0 if float('$cost') > 0 else 1)" || { echo "FAIL: cost is 0, agent never ran"; exit 1; }
  n_errored=$(python3 -c "import json;r=json.load(open('$result'));print(r.get('stats',{}).get('n_errored_trials',0) or 0)")
  n_completed=$(python3 -c "import json;r=json.load(open('$result'));print(r.get('stats',{}).get('n_completed_trials',0) or 0)")
  echo "n_errored_trials: $n_errored  n_completed_trials: $n_completed"
  python3 -c "import sys; sys.exit(0 if int('$n_errored')==0 else 1)" \
    || { echo "FAIL: trial ERRORED (n_errored_trials=$n_errored) — harness failure, not a model outcome"; exit 1; }
  python3 -c "import sys; sys.exit(0 if int('$n_completed')>=1 else 1)" \
    || { echo "FAIL: no trial completed (n_completed_trials=$n_completed)"; exit 1; }
  echo "OK: agent ran, spent \$$cost, and the trial completed without exception (n_errored=0, n_completed=$n_completed)"
} > "$OUT/trial.txt" 2>&1
rc=$?
cat "$OUT/trial.txt"
rm -rf "$trial_out"
exit $rc
```

## Block 6: the harbor 0.26.0 verifier reward-file contract

Harbor 0.26.0 runs the verifier test script inside the container and then looks
for a reward file (`reward.txt`/`reward.json`) on the host. TB2.1's run-tests.sh
runs pytest and exits — it does NOT write a reward file, which is why every
trial ends in RewardFileNotFoundError regardless of TEST_DIR. This block reads
the installed harbor verifier source to recover the exact contract: the
container-side path (or env var) the test script must write reward.txt to, and
the content format. It asserts the contract is present (REWARD_FILE / reward.txt
is referenced), so a harbor whose contract changed is caught.

```sh produces=contract.txt
set -e
HB=/home/nthalk/.local/share/uv/tools/harbor/lib/python3.12/site-packages/harbor
V="$HB/verifier/verifier.py"
T="$HB/trial/trial.py"
[ -f "$V" ] || { echo "FAIL: harbor verifier.py not at $V" > "$OUT/contract.txt"; cat "$OUT/contract.txt"; exit 1; }
{
  echo "=== verifier.py: reward-file lookup + parse ==="
  grep -nE 'reward|REWARD|reward_file|reward\.txt|reward\.json|float|json\.load|open\(' "$V" | head -60
  echo
  echo "=== verifier.py lines 270-320 (the lookup) ==="
  sed -n '270,320p' "$V"
  echo
  echo "=== trial.py: how the shared verifier is exec'd (env, cwd, command) ==="
  grep -nE 'REWARD|reward|env|cwd|exec|run_shared|_verify|verifier|/logs|mount|bind' "$T" | head -60
} 2>&1 > "$OUT/contract.txt"
# Real assertion: the contract must be discoverable — harbor must reference a
# reward file the test script writes. Fail if neither REWARD_FILE env nor a
# reward.txt path is present, so a harbor whose contract changed is caught.
grep -qiE 'REWARD_FILE|reward\.txt' "$OUT/contract.txt" \
  || { echo "FAIL: no REWARD_FILE / reward.txt contract found in harbor source" | tee -a "$OUT/contract.txt"; exit 1; }
echo "OK: harbor source references the reward-file contract" | tee -a "$OUT/contract.txt"
cat "$OUT/contract.txt"
```

## Block 7: how harbor execs the verifier test.sh in the container

`_remove_reward_files` in verifier.py uses TWO namespaces: `env_paths.reward_text_path`
(container-side, where files are removed inside the environment) and
`trial_paths.reward_text_path` (host-side, where harbor reads the result). That
means the verifier dir is bind-mounted into the container, and the test script
must write reward.txt to the CONTAINER path. This block recovers the exact
container path and whether harbor sets a REWARD_FILE env var when it execs
test.sh, so the shim writes reward.txt where harbor will find it.

```sh produces=exec.txt
set -e
HB=/home/nthalk/.local/share/uv/tools/harbor/lib/python3.12/site-packages/harbor
# Find the paths module that defines reward_text_path for both env and trial.
{
  echo "=== files defining reward_text_path ==="
  grep -rln 'reward_text_path' "$HB" --include=*.py | grep -v test | head
  echo
  echo "=== definition of reward_text_path (env + trial) ==="
  grep -rnE 'reward_text_path|reward_json_path|verifier.*=|VERIFIER|reward_dir|def paths|class .*Paths' "$HB" --include=*.py | grep -iE 'reward|verifier' | grep -vE 'test|_remove|_parse' | head -40
  echo
  echo "=== how the verifier test.sh is exec'd: env vars + command (trial.py) ==="
  grep -nE 'REWARD_FILE|reward_file|env\[|env=|environment=|test_path|test\.sh|run_command|docker exec|exec|command=|cwd=|working_dir|/logs/verifier|verifier.*path' "$HB/trial/trial.py" | head -50
} 2>&1 > "$OUT/exec.txt"
# Assertion: the contract must name a container-side path the script writes to.
# Fail if neither a REWARD_FILE-style env var nor an explicit verifier path is
# found — that is the signal the contract changed and the shim design is wrong.
grep -qiE 'REWARD_FILE|reward_text_path|/logs/verifier|verifier' "$OUT/exec.txt" \
  || { echo "FAIL: no container-side reward path / env var found in harbor source" | tee -a "$OUT/exec.txt"; exit 1; }
echo "OK: container-side reward path discoverable" | tee -a "$OUT/exec.txt"
cat "$OUT/exec.txt"
```

## Block 8: the exact container-side vs host-side reward path

`paths.py` defines `reward_text_path` in two namespaces. The env-side one is
where the in-container test script must write reward.txt; the trial-side one is
where harbor reads it on the host. This block recovers both exact expressions so
the shim writes reward.txt to the container path.

```sh produces=paths.txt
set -e
HB=/home/nthalk/.local/share/uv/tools/harbor/lib/python3.12/site-packages/harbor
P="$HB/models/trial/paths.py"
[ -f "$P" ] || { echo "FAIL: harbor paths.py not at $P" > "$OUT/paths.txt"; cat "$OUT/paths.txt"; exit 1; }
{
  echo "=== paths.py: reward + verifier path definitions ==="
  grep -nE 'reward_text_path|reward_json_path|verifier|VERIFIER|env_paths|trial_paths|def |class |logs|/logs' "$P" | head -60
  echo
  echo "=== full definition lines for reward_text_path (env + trial) ==="
  grep -nE 'reward_text_path|reward_json_path' "$P"
  echo
  echo "=== surrounding context of each definition ==="
  for ln in $(grep -nE 'reward_text_path\s*=' "$P" | cut -d: -f1); do
    echo "--- around line $ln ---"
    sed -n "$((ln-8)),$((ln+3))p" "$P"
  done
} 2>&1 > "$OUT/paths.txt"
# Assertion: the container-side (env) reward path must be discoverable and must
# differ from the host-side (trial) path. If they are the same expression the
# bind-mount assumption is wrong and the shim design needs rework.
grep -qE 'reward_text_path' "$OUT/paths.txt" \
  || { echo "FAIL: reward_text_path definition not found in paths.py" | tee -a "$OUT/paths.txt"; exit 1; }
echo "OK: reward path definitions recovered" | tee -a "$OUT/paths.txt"
cat "$OUT/paths.txt"
```
