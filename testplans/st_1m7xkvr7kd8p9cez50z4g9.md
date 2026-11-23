# Test plan: st_1m7xkvr7kd8p9cez50z4g9

Sweep prerequisites for arm A: harbor CLI, docker, ~/.dun/config.json and the TB2.1 tasks dir must all be present before any trial runs.

```sh produces=prereq.txt
{
  echo "=== date ==="
  date -u +%Y-%m-%dT%H:%M:%SZ
  echo "=== harbor ==="
  command -v harbor && harbor --version 2>&1 | head -2
  echo "=== docker ==="
  docker info --format 'server {{.ServerVersion}}' 2>&1 | head -2
  echo "=== config ==="
  [ -f "$HOME/.dun/config.json" ] && echo "config.json present"
  echo "=== tasks dir ==="
  TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
  [ -d "$TASKS" ] && ls "$TASKS" | tr '\n' ' '
  echo
} > "$OUT/prereq.txt" 2>&1
rc=$?
cat "$OUT/prereq.txt"
[ $rc -eq 0 ] || exit 1
```

Identify the 10-task set of the original 2026-11-06 run so the 9-task sweep set can be pinned (10 minus extract-moves-from-video, the budget-exhaustion case flagged distinct from premature-done).

```sh produces=jobs.txt
set -e
{
  JOBS=/home/nthalk/local/src/iodesystems/dun/examples/harbor/jobs
  echo "=== jobs dirs ==="
  ls -d "$JOBS"/* 2>/dev/null || echo "(none)"
  echo
  j=$(ls -d "$JOBS"/20261106* 2>/dev/null | head -1)
  echo "=== $j ==="
  ls "$j"
} > "$OUT/jobs.txt" 2>&1
cat "$OUT/jobs.txt"
grep -q 20261106 "$OUT/jobs.txt" || { echo "FAIL: no 2026-11-06 jobs dir found"; exit 1; }
```

The 9-task sweep set is the 10-task set of jobs/full-20261106-232928 (the run the issue cites as TB2.1) minus extract-moves-from-video, which the issue flags as a DISTINCT budget-exhaustion case (max turns, no premature done), not a done-after-checks case.

```sh produces=full.txt
set -e
J=/home/nthalk/local/src/iodesystems/dun/examples/harbor/jobs/full-20261106-232928
[ -d "$J" ] || { echo "FAIL: $J missing"; exit 1; }
{
  echo "=== tasks in $J ==="
  ls "$J"
  echo
  echo "=== per-task spend + reward (result.json) ==="
  python3 - "$J" <<'EOF'
import json, pathlib, sys
j = pathlib.Path(sys.argv[1])
for t in sorted(p.parent.name for p in j.glob("*/*__*/result.json")):
    pass
for p in sorted(j.glob("*/*__*/result.json")):
    r = json.loads(p.read_text())
    ar = r.get("agent_result") or {}
    exc = (r.get("exception_info") or {}).get("exception_type")
    print(f"{p.parent.name.split('-trial')[0] if '-trial' in p.parent.name else p.parent.name}\t"
          f"reward={ar.get('reward')}\tcost=${ar.get('cost_usd')}\texc={exc}")
EOF
} > "$OUT/full.txt" 2>&1
cat "$OUT/full.txt"
n=$(ls "$J" | grep -v harbor | wc -l)
[ "$n" -eq 10 ] || { echo "FAIL: expected 10 task dirs, got $n"; exit 1; }
```

Pin the task set. The issue's protocol is "10 tasks x 3" (the recap A/B). The sweep-20261106-232524 base/recap pair is the recap line's own A/B and must share one task set. That set, minus extract-moves-from-video (the distinct budget case), is the 9-task set both arms sweep.

```sh produces=set.txt
set -e
JOBS=/home/nthalk/local/src/iodesystems/dun/examples/harbor/jobs
{
  for j in 20261106-211331-fireworks-glm-5p3-flash-low \
           sweep-20261106-232524-base sweep-20261106-232524-recap; do
    d="$JOBS/$j"
    [ -d "$d" ] || { echo "$j: MISSING"; continue; }
    echo "=== $j ==="
    ls "$d" | grep -v '^harbor' | sort | tr '\n' ' '; echo
    echo "  count: $(ls "$d" | grep -v '^harbor' | wc -l)"
    echo
  done
} > "$OUT/set.txt" 2>&1
cat "$OUT/set.txt"
# The two recap A/B arms must share the identical task set (that is the protocol's
# invariant). Capture it.
b=$(ls "$JOBS/sweep-20261106-232524-base" 2>/dev/null | grep -v '^harbor' | sort | tr '\n' ',')
r=$(ls "$JOBS/sweep-20261106-232524-recap" 2>/dev/null | grep -v '^harbor' | sort | tr '\n' ',')
[ "$b" = "$r" ] && [ -n "$b" ] || { echo "FAIL: recap base/recap task sets differ or empty"; exit 1; }
echo "recap base == recap recap: $b"
```

Confirm the 9-task sweep set. The canonical set is the recap A/B's 10 tasks; headless-terminal is absent from the stable TB2.1 checkout (per the invalidated arm-A node), leaving 9. All 9 must have a task.yaml for the converter.

```sh produces=nine.txt
set -e
TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
set9="cancel-async-tasks configure-git-webserver extract-elf financial-document-processor kv-store-grpc pypi-server pytorch-model-cli sanitize-git-repo sparql-university"
{
  echo "=== 9-task sweep set (recap 10 minus headless-terminal) ==="
  for t in $set9; do
    if [ -d "$TASKS/$t" ] && [ -f "$TASKS/$t/task.yaml" ]; then
      echo "  $t: ok (task.yaml)"
    else
      echo "  $t: MISSING"
    fi
  done
  echo
  echo "=== headless-terminal (expected absent) ==="
  [ -d "$TASKS/headless-terminal" ] && echo "  present" || echo "  absent (as expected)"
} > "$OUT/nine.txt" 2>&1
cat "$OUT/nine.txt"
missing=0
for t in $set9; do [ -f "$TASKS/$t/task.yaml" ] || missing=$((missing+1)); done
[ "$missing" -eq 0 ] || { echo "FAIL: $missing of 9 tasks missing task.yaml"; exit 1; }
```

Arm A is the UNFLAGGED line: the binary the trials upload must carry systemFinish and NOT systemFinishCandidate. The candidate is not wired to any env var on base (per i_1m84d67g81wxvhp07gt6dd), so a current build is arm A by construction — but the binary must be current, and this checks the actual bytes.

```sh produces=bin.txt
set -e
{
  echo "=== bin/dun ==="
  ls -l /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor/bin/dun 2>&1
  file /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor/bin/dun 2>&1 | head -1
  stat -c 'mtime: %y' /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor/bin/dun
  echo
  echo "=== systemprompt.go mtime (source of the prompt the binary carries) ==="
  stat -c '%y' /home/nthalk/local/src/iodesystems/dun.wt0/internal/harness/systemprompt.go
  echo
  echo "=== which line does the binary carry? ==="
  # The binary is a Go archive; the systemFinish strings are in it. The candidate
  # line's distinctive clause must be ABSENT for arm A to be what runs.
  strings /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor/bin/dun \
    | grep -c "a check you only read is not a check you passed" \
    && echo "FAIL: candidate clause in binary" || echo "candidate clause absent (good)"
  strings /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor/bin/dun \
    | grep -c "check each requirement the task states" \
    && echo "arm A clause present (good)" || { echo "FAIL: no arm A clause"; exit 1; }
} > "$OUT/bin.txt" 2>&1
cat "$OUT/bin.txt"
grep -q "candidate clause absent" "$OUT/bin.txt" || { echo "FAIL: binary carries the candidate line"; exit 1; }
grep -q "arm A clause present" "$OUT/bin.txt" || { echo "FAIL: binary lacks the arm A line"; exit 1; }
```

Size the arm-A budget from REAL per-task spend on this exact task set. The recap baseline arm ran all 10 tasks; the 9-task sum x3 (3 runs) is the expected spend, and the BUDGET cap is that plus margin. This is the measurement the plan requires — not the single cheap extract-elf trial.

```sh produces=spend.txt
set -e
JOBS=/home/nthalk/local/src/iodesystems/dun/examples/harbor/jobs
B="$JOBS/sweep-20261106-232524-base"
{
  echo "=== per-task spend, recap baseline arm (same 10-task set) ==="
  python3 - "$B" <<'EOF'
import json, pathlib, sys
j = pathlib.Path(sys.argv[1])
tot = 0.0
per = {}
for p in sorted(j.glob("*/*__*/result.json")):
    r = json.loads(p.read_text())
    ar = r.get("agent_result") or {}
    c = ar.get("cost_usd") or 0
    task = p.parent.name.split("__")[0]
    per.setdefault(task, 0.0)
    per[task] += c
    tot += c
for t in sorted(per):
    print(f"  {t:34s} ${per[t]:.4f}")
print(f"  {'TOTAL (10 tasks, 1 run)':34s} ${tot:.4f}")
print(f"  x3 runs (27 trials, 9 tasks minus headless-terminal):")
nine = [t for t in per if t != "headless-terminal"]
nine_tot = sum(per[t] for t in nine)
print(f"    9-task one-run total ${nine_tot:.4f}  x3 = ${nine_tot*3:.4f}")
EOF
} > "$OUT/spend.txt" 2>&1
cat "$OUT/spend.txt"
grep -q "TOTAL" "$OUT/spend.txt" || { echo "FAIL: no spend data"; exit 1; }
```

Arm A sweep: 9 tasks x 3 runs (27 trials) with the current unflagged systemFinish line, through the repaired harbor 0.26.0 invocation. Budget cap $2.00 covers the measured ~$0.42 expected spend with margin. The block runs the real harbor CLI against real docker trials and fails (exit 2) on a harness abort.

```sh produces=sweep.log
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=${DUN_MODEL:-fireworks-glm-5p3-flash}
export DUN_EFFORT=${DUN_EFFORT:-low}
export DUN_PRICE_IN=${DUN_PRICE_IN:-0.15} DUN_PRICE_CACHED=${DUN_PRICE_CACHED:-0.03} DUN_PRICE_OUT=${DUN_PRICE_OUT:-0.5}
export PYTHONPATH=$PWD
export BUDGET=2.00
# Arm A = the unflagged line. DUN_FINISH_CANDIDATE is NOT exported (not wired on
# base; the binary carries systemFinish).
unset DUN_FINISH_CANDIDATE
export OUT="jobs/armA-$(date +%Y%m%d-%H%M%S)-$DUN_MODEL-$DUN_EFFORT"
set9="cancel-async-tasks configure-git-webserver extract-elf financial-document-processor kv-store-grpc pypi-server pytorch-model-cli sanitize-git-repo sparql-university"
{
  echo "OUT dir: $OUT"
  echo "model: $DUN_MODEL effort: $DUN_EFFORT budget: $BUDGET"
  echo "tasks: $set9"
  echo
  for run in 1 2 3; do
    echo "########## RUN $run ##########"
    for t in $set9; do
      s=$(python3 - "$OUT" <<'PY'
import json, pathlib, sys
print(sum((json.loads(p.read_text()).get("agent_result") or {}).get("cost_usd") or 0
          for p in pathlib.Path(sys.argv[1]).glob("*/*__*/result.json")))
PY
)
      echo "=== run $run task $t (spent so far \$$s)"
      cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
      taskdir="/tmp/harbor-tasks-$t"
      if [ ! -f "$taskdir/task.toml" ]; then
        rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"
        python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"
      fi
      cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
      if ! "$HARBOR" run --config "$cfgfile" -o "$OUT" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$OUT/harbor-$t-run$run.log"; then
        echo "ABORT: harbor run for $t run$run failed"
        rm -f "$cfgfile"
        exit 2
      fi
      rm -f "$cfgfile"
    done
  done
  echo "SWEEP COMPLETE"
} > "$OUT/sweep.log" 2>&1
rc=$?
echo "sweep exit: $rc"
tail -5 "$OUT/sweep.log"
[ $rc -eq 0 ] || { echo "FAIL: sweep aborted (see sweep.log)"; exit 1; }
```

Arm A sweep: 9 tasks x 3 runs (27 trials) with the current unflagged systemFinish line, through the repaired harbor 0.26.0 invocation. Budget cap $2.00 covers the measured ~$0.42 expected spend with margin. The block runs the real harbor CLI against real docker trials and fails (exit 2) on a harness abort.

```sh produces=sweep.log
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=${DUN_MODEL:-fireworks-glm-5p3-flash}
export DUN_EFFORT=${DUN_EFFORT:-low}
export DUN_PRICE_IN=${DUN_PRICE_IN:-0.15} DUN_PRICE_CACHED=${DUN_PRICE_CACHED:-0.03} DUN_PRICE_OUT=${DUN_PRICE_OUT:-0.5}
export PYTHONPATH=$PWD
export BUDGET=2.00
# Arm A = the unflagged line. DUN_FINISH_CANDIDATE is NOT exported (not wired on
# base; the binary carries systemFinish).
unset DUN_FINISH_CANDIDATE
export OUT="jobs/armA-$(date +%Y%m%d-%H%M%S)-$DUN_MODEL-$DUN_EFFORT"
mkdir -p "$OUT"
set9="cancel-async-tasks configure-git-webserver extract-elf financial-document-processor kv-store-grpc pypi-server pytorch-model-cli sanitize-git-repo sparql-university"
{
  echo "OUT dir: $OUT"
  echo "model: $DUN_MODEL effort: $DUN_EFFORT budget: $BUDGET"
  echo "tasks: $set9"
  echo
  for run in 1 2 3; do
    echo "########## RUN $run ##########"
    for t in $set9; do
      s=$(python3 - "$OUT" <<'PY'
import json, pathlib, sys
print(sum((json.loads(p.read_text()).get("agent_result") or {}).get("cost_usd") or 0
          for p in pathlib.Path(sys.argv[1]).glob("*/*__*/result.json")))
PY
)
      echo "=== run $run task $t (spent so far \$$s)"
      cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
      taskdir="/tmp/harbor-tasks-$t"
      if [ ! -f "$taskdir/task.toml" ]; then
        rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"
        python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"
      fi
      cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
      if ! "$HARBOR" run --config "$cfgfile" -o "$OUT" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$OUT/harbor-$t-run$run.log"; then
        echo "ABORT: harbor run for $t run$run failed"
        rm -f "$cfgfile"
        exit 2
      fi
      rm -f "$cfgfile"
    done
  done
  echo "SWEEP COMPLETE"
} > "$OUT/sweep.log" 2>&1
rc=$?
echo "sweep exit: $rc"
tail -5 "$OUT/sweep.log"
[ $rc -eq 0 ] || { echo "FAIL: sweep aborted (see sweep.log)"; exit 1; }
```

Measure how far the killed sweep got and the per-trial wall-clock, to size the total and confirm the testplan ~6-min wall-clock bound is the blocker (not the harness).

```sh produces=progress.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
# Find the most recent armA dir the killed sweep left.
d=$(ls -dt jobs/armA-*/ 2>/dev/null | head -1)
[ -n "$d" ] || { echo "FAIL: no armA jobs dir"; exit 1; }
{
  echo "dir: $d"
  echo "=== sweep.log ==="
  cat "$d/sweep.log" 2>/dev/null || echo "(no sweep.log)"
  echo
  echo "=== trial result.json files written so far ==="
  for p in "$d"/*/*__*/result.json; do
    [ -f "$p" ] || continue
    python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
ar=r.get("agent_result") or {}
exc=(r.get("exception_info") or {}).get("exception_type")
print(sys.argv[2], "reward=",ar.get("reward"),"cost=$"+str(ar.get("cost_usd")),"exc=",exc)' "$p" "$(basename "$(dirname "$p")")"
  done
  echo
  echo "=== harbor per-trial logs (last lines) ==="
  for f in "$d"/harbor-*.log; do
    [ -f "$f" ] || continue
    echo "--- $(basename "$f") ---"
    tail -3 "$f"
  done
} > "$OUT/progress.txt" 2>&1
cat "$OUT/progress.txt"
```

Arm A sweep, batch 1 (run 1, tasks 1-3). Each trial is one harbor run of one task at GLM-5.3-Flash low effort with the unflagged systemFinish line. Three trials per block stays within the testplan ~6-min wall-clock bound (measured 41-70s per trial).

```sh produces=batch1.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=${DUN_MODEL:-fireworks-glm-5p3-flash}
export DUN_EFFORT=${DUN_EFFORT:-low}
export DUN_PRICE_IN=${DUN_PRICE_IN:-0.15} DUN_PRICE_CACHED=${DUN_PRICE_CACHED:-0.03} DUN_PRICE_OUT=${DUN_PRICE_OUT:-0.5}
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE   # arm A = the unflagged line
OUT=jobs/armA-20261123
mkdir -p "$OUT"
# Batch 1: run 1, first three tasks.
BATCH="1:cancel-async-tasks 1:configure-git-webserver 1:extract-elf"
for spec in $BATCH; do
  run=${spec%%:*}; t=${spec##*:}
  taskdir="/tmp/harbor-tasks-$t"
  if [ ! -f "$taskdir/task.toml" ]; then
    rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"
    python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"
  fi
  cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
  cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
  if ! "$HARBOR" run --config "$cfgfile" -o "$OUT" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$OUT/harbor-$t-run$run.log"; then
    rm -f "$cfgfile"; echo "ABORT: harbor run $t run$run failed"; exit 2
  fi
  rm -f "$cfgfile"
done
# Summarize this batch to the testplan output dir.
{
  for spec in $BATCH; do
    run=${spec%%:*}; t=${spec##*:}
    r=$(ls "$OUT/$t"/*__*/result.json 2>/dev/null | tail -1)
    [ -f "$r" ] || { echo "$t run$run: NO result.json"; continue; }
    python3 -c 'import json,sys
r=json.load(open(sys.argv[1])); ar=r.get("agent_result") or {}
exc=(r.get("exception_info") or {}).get("exception_type")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={ar.get(\"reward\")} cost=${ar.get(\"cost_usd\")} exc={exc}")' "$r" "$t" "$run"
  done
} > "$OUT/batch1.txt" 2>&1
cp "$OUT/batch1.txt" "$OUT/batch1.txt"
cat "$OUT/batch1.txt"
# also stage a copy for the testplan artifact
cp "$OUT/batch1.txt" "${OUT//jobs\//}/batch1.txt" 2>/dev/null || true
cp "$OUT/batch1.txt" ./batch1.txt
cp ./batch1.txt "$OUT/../batch1.txt" 2>/dev/null || true
```

State check after the killed batch: which trials already have result.json in jobs/armA-20261123, so the one-trial-per-block sweep resumes where it left off.

```sh produces=state.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
OUT=jobs/armA-20261123
{
  echo "=== trial result.json files present in $OUT ==="
  for t in cancel-async-tasks configure-git-webserver extract-elf financial-document-processor kv-store-grpc pypi-server pytorch-model-cli sanitize-git-repo sparql-university; do
    for r in "$OUT/$t"/*__*/result.json; do
      [ -f "$r" ] || continue
      python3 -c 'import json,sys
r=json.load(open(sys.argv[1])); ar=r.get("agent_result") or {}
exc=(r.get("exception_info") or {}).get("exception_type")
print(sys.argv[2], "reward="+str(ar.get("reward")), "cost=$"+str(ar.get("cost_usd")), "exc="+str(exc))' "$r" "$t"
    done
  done
  echo "=== harbor logs present ==="
  ls "$OUT"/harbor-*.log 2>/dev/null | wc -l
} > "$OUT/state.txt" 2>&1
cat "$OUT/state.txt"
cp "$OUT/state.txt" ./state.txt
```

Read the full result.json structure to find the real reward / pass-fail / completion fields (agent_result.reward came back None). HD is the harbor jobs dir; $OUT stays the testplan artifact dir.

```sh produces=structure.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
HD=jobs/armA-20261123
# Read one full result.json to find the reward / pass-fail fields.
r=$(ls "$HD/cancel-async-tasks"/*__*/result.json 2>/dev/null | head -1)
[ -f "$r" ] || { echo "FAIL: no result.json"; exit 1; }
{
  echo "=== full result.json: $(basename "$(dirname "$r")") ==="
  python3 -m json.tool "$r"
} > "$OUT/structure.txt" 2>&1
cat "$OUT/structure.txt"
```

Find where reward / pass-fail lives: top-level keys of the per-trial result.json and the job-level result.json stats.

```sh produces=structure2.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
HD=jobs/armA-20261123
r=$(ls "$HD/cancel-async-tasks"/*__*/result.json 2>/dev/null | head -1)
[ -f "$r" ] || { echo "FAIL: no result.json"; exit 1; }
{
  echo "=== top-level keys ==="
  python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
print(list(r.keys()))
print()
for k in ("agent_result","exception_info","reward","reward_valid","stats","verifier_result"):
    if k in r:
        print(k, "=", json.dumps(r[k])[:400])
' "$r"
  echo
  echo "=== job-level result.json (stats) ==="
  jr="$HD/cancel-async-tasks/result.json"
  [ -f "$jr" ] && python3 -m json.tool "$jr" | head -40 || echo "(no job-level result.json)"
} > "$OUT/structure2.txt" 2>&1
cat "$OUT/structure2.txt"
```

Extract reward, cost, exception for the 3 completed trials from their result.json files.

```sh produces=armA_existing.csv
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
HD=jobs/armA-20261123
{
  echo "trial_task,run,reward,cost_usd,exception"
  for task in cancel-async-tasks configure-git-webserver extract-elf; do
    r=$(ls "$HD/$task"/*__*/result.json 2>/dev/null | head -1)
    [ -f "$r" ] || { echo "$task,1,MISSING,0,missing"; continue; }
    python3 -c '
import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]},{sys.argv[3]},{rw},{cost},{exc}")
' "$r" "$task" 1
  done
} > "$OUT/armA_existing.csv" 2>&1
cat "$OUT/armA_existing.csv"
```

Arm A trial: cancel-async-tasks run 2.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
JD=jobs/armA-20261123
mkdir -p "$JD"
t=cancel-async-tasks; run=2
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t-run$run.log" || { rm -f "$cfgfile"; echo "ABORT"; exit 2; }
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Find all result.json files to understand the directory structure and whether run-1 vs run-2 results are distinguished.

```sh produces=findings.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
JD=jobs/armA-20261123
{
  echo "=== all result.json files in $JD ==="
  find "$JD" -name result.json | sort
  echo
  echo "=== trial dirs under cancel-async-tasks ==="
  ls "$JD/cancel-async-tasks/" 2>/dev/null
  echo
  echo "=== harbor logs ==="
  ls "$JD"/harbor-*.log 2>/dev/null
} > "$OUT/findings.txt" 2>&1
cat "$OUT/findings.txt"
# Real check: at least one per-trial result.json must exist.
n=$(find "$JD" -name result.json | wc -l)
[ "$n" -ge 1 ] || { echo "FAIL: no result.json files"; exit 1; }
```

Confirm run 2 overwrote run 1's per-trial result (identical trial dir, job-level shows 2 runs cumulative).

```sh produces=overwrite.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
JD=jobs/armA-20261123
{
  echo "=== cancel-async-tasks job-level result.json (cumulative) ==="
  python3 -c 'import json
r=json.load(open("jobs/armA-20261123/cancel-async-tasks/result.json"))
s=r.get("stats",{})
print("n_total_trials:", s.get("n_total_trials"))
print("n_completed_trials:", s.get("n_completed_trials"))
ev=s.get("evals",{})
for k,v in ev.items():
    print("eval:", k, "n_trials:", v.get("n_trials"), "reward_stats:", v.get("reward_stats"))
'
  echo
  echo "=== per-trial result.json (which run is this?) ==="
  python3 -c 'import json
r=json.load(open("jobs/armA-20261123/cancel-async-tasks/harbor-tasks-cancel-async-tasks__cHMkMGn/result.json"))
print("started_at:", r.get("started_at"))
print("finished_at:", r.get("finished_at"))
print("reward:", r.get("verifier_result",{}).get("rewards",{}).get("reward"))
'
} > "$OUT/overwrite.txt" 2>&1
cat "$OUT/overwrite.txt"
```

Arm A r1t1: cancel-async-tasks run 1.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=cancel-async-tasks; run=1
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r1t2: configure-git-webserver run 1.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=configure-git-webserver; run=1
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r1t3: extract-elf run 1.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=extract-elf; run=1
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r1t4: financial-document-processor run 1.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=financial-document-processor; run=1
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r1t5: kv-store-grpc run 1.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=kv-store-grpc; run=1
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r1t6: pypi-server run 1.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=pypi-server; run=1
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r1t7: pytorch-model-cli run 1.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=pytorch-model-cli; run=1
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r1t8: sanitize-git-repo run 1.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=sanitize-git-repo; run=1
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r1t9: sparql-university run 1. Completes run 1 (9/27).

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=sparql-university; run=1
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r2t1: cancel-async-tasks run 2.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=cancel-async-tasks; run=2
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r2t2: configure-git-webserver run 2.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=configure-git-webserver; run=2
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r2t3: extract-elf run 2.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=extract-elf; run=2
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r2t4: financial-document-processor run 2.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=financial-document-processor; run=2
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r2t5: kv-store-grpc run 2.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=kv-store-grpc; run=2
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r2t6: pypi-server run 2.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=pypi-server; run=2
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r2t7: pytorch-model-cli run 2.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=pytorch-model-cli; run=2
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r2t8: sanitize-git-repo run 2.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=sanitize-git-repo; run=2
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r2t9: sparql-university run 2. Completes run 2 (18/27).

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=sparql-university; run=2
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r3t1: cancel-async-tasks run 3.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=cancel-async-tasks; run=3
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r3t2: configure-git-webserver run 3.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=configure-git-webserver; run=3
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r3t3: extract-elf run 3.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=extract-elf; run=3
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r3t4: financial-document-processor run 3.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=financial-document-processor; run=3
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r3t5: kv-store-grpc run 3.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=kv-store-grpc; run=3
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r3t6: pypi-server run 3.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=pypi-server; run=3
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r3t7: pytorch-model-cli run 3.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=pytorch-model-cli; run=3
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r3t8: sanitize-git-repo run 3.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=sanitize-git-repo; run=3
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Arm A r3t9: sparql-university run 3 — final trial of the 27.

```sh produces=result.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
export TASKS=/home/nthalk/local/src/iodesystems/terminal-bench-2-1/original-tasks
export HARBOR=$(which harbor)
export DUN_URL=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["url"])')
export DUN_LLM_KEY=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.dun/config.json")))["key"])')
export DUN_MODEL=fireworks-glm-5p3-flash DUN_EFFORT=low
export DUN_PRICE_IN=0.15 DUN_PRICE_CACHED=0.03 DUN_PRICE_OUT=0.5
export PYTHONPATH=$PWD
unset DUN_FINISH_CANDIDATE
t=sparql-university; run=3
JD="jobs/armA-20261123-r$run"
mkdir -p "$JD"
taskdir="/tmp/harbor-tasks-$t"
[ -f "$taskdir/task.toml" ] || { rm -rf "$taskdir"; cp -r "$TASKS/$t" "$taskdir"; python3 convert_task_yaml.py "$TASKS/$t" "$taskdir/task.toml"; }
cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
cat > "$cfgfile" <<YAML
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
YAML
"$HARBOR" run --config "$cfgfile" -o "$JD" -k 1 -n 1 --job-name "$t" 2>&1 | tee "$JD/harbor-$t.log"
rm -f "$cfgfile"
r=$(ls "$JD/$t"/*__*/result.json 2>/dev/null | tail -1)
[ -f "$r" ] || { echo "NO result.json"; exit 1; }
python3 -c 'import json,sys
r=json.load(open(sys.argv[1]))
rw=r.get("verifier_result",{}).get("rewards",{}).get("reward",None)
cost=r.get("agent_result",{}).get("cost_usd",0)
exc=r.get("exception_info")
print(f"{sys.argv[2]} run{sys.argv[3]}: reward={rw} cost=${cost} exc={exc}")' "$r" "$t" "$run" > "$OUT/result.txt"
cat "$OUT/result.txt"
```

Build the per-trial table from all 27 result.json files and harbor logs, counting tool calls and premature-done claims.

```sh produces=table.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
tasks="cancel-async-tasks configure-git-webserver extract-elf financial-document-processor kv-store-grpc pypi-server pytorch-model-cli sanitize-git-repo sparql-university"
echo "| task | run | reward | cost_usd | tool_calls | premature_done |"
echo "|------|-----|--------|----------|------------|----------------|"
total=0; premature=0
for run in 1 2 3; do
  for t in $tasks; do
    JD="jobs/armA-20261123-r$run"
    # Get cost from result.json
    r=$(ls "$JD/$t"/*/result.json 2>/dev/null | tail -1)
    if [ -z "$r" ]; then
      echo "| $t | $run | MISSING | - | - | - |"
      continue
    fi
    cost=$(python3 -c "import json;r=json.load(open('$r'));print(f\"{r.get('agent_result',{}).get('cost_usd',0):.4f}\")")
    reward=$(python3 -c "import json;r=json.load(open('$r'));print(r.get('verifier_result',{}).get('rewards',{}).get('reward',0))")
    # Count tool calls from harbor log
    log="$JD/harbor-$t.log"
    tc=0
    if [ -f "$log" ]; then
      tc=$(grep -c 'tool_call\|function_call\|Tool call\|Running tool' "$log" 2>/dev/null || echo 0)
    fi
    # Check for premature done: low tool calls + agent claims done
    pd="no"
    if [ "$tc" -le 9 ] 2>/dev/null; then
      if [ -f "$log" ] && grep -qi 'done\|complete\|finished\|I have\|I\'ve\|success' "$log" 2>/dev/null; then
        pd="yes"
        premature=$((premature+1))
      fi
    fi
    total=$((total+1))
    echo "| $t | $run | $reward | \$$cost | $tc | $pd |"
  done
done
echo ""
echo "Total trials: $total, Premature-done: $premature"
```

Build per-trial table from all 27 result.json + harbor logs: reward, cost, tool-call count, premature-done flag.

```sh produces=table.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
tasks="cancel-async-tasks configure-git-webserver extract-elf financial-document-processor kv-store-grpc pypi-server pytorch-model-cli sanitize-git-repo sparql-university"
{
echo "| task | run | reward | cost_usd | tool_calls | premature_done |"
echo "|------|-----|--------|----------|------------|----------------|"
total=0; premature=0
for run in 1 2 3; do
  for t in $tasks; do
    JD="jobs/armA-20261123-r$run"
    r=$(ls "$JD/$t"/*/result.json 2>/dev/null | tail -1)
    if [ -z "$r" ]; then
      echo "| $t | $run | MISSING | - | - | - |"
      continue
    fi
    cost=$(python3 -c "import json;r=json.load(open('${r}'));print(f\"{r.get('agent_result',{}).get('cost_usd',0):.4f}\")")
    reward=$(python3 -c "import json;r=json.load(open('${r}'));print(r.get('verifier_result',{}).get('rewards',{}).get('reward',0))")
    log="$JD/harbor-$t.log"
    tc=0
    if [ -f "$log" ]; then
      tc=$(grep -cE 'tool_call|function_call|Tool call|Running tool' "$log" 2>/dev/null || true)
      [ -z "$tc" ] && tc=0
    fi
    pd="no"
    if [ "$tc" -le 9 ] 2>/dev/null; then
      if [ -f "$log" ] && grep -qiE 'done|complete|finished|success' "$log" 2>/dev/null; then
        pd="yes"
        premature=$((premature+1))
      fi
    fi
    total=$((total+1))
    echo "| $t | $run | $reward | \$$cost | $tc | $pd |"
  done
done
echo ""
echo "Total trials: $total, Premature-done: $premature"
} > "$OUT/table.txt"
cat "$OUT/table.txt"
```

Per-trial table with tool-call counts from dun.jsonl (type:tool_call events). Premature-done = <=9 tool calls.

```sh produces=table.txt
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0/examples/harbor
tasks="cancel-async-tasks configure-git-webserver extract-elf financial-document-processor kv-store-grpc pypi-server pytorch-model-cli sanitize-git-repo sparql-university"
{
echo "| task | run | reward | cost_usd | tool_calls | premature_done |"
echo "|------|-----|--------|----------|------------|----------------|"
total=0; premature=0
for run in 1 2 3; do
  for t in $tasks; do
    JD="jobs/armA-20261123-r$run"
    r=$(ls "$JD/$t"/*/result.json 2>/dev/null | tail -1)
    if [ -z "$r" ]; then
      echo "| $t | $run | MISSING | - | - | - |"
      continue
    fi
    cost=$(python3 -c "import json;r=json.load(open('${r}'));print(f\"{r.get('agent_result',{}).get('cost_usd',0):.4f}\")")
    reward=$(python3 -c "import json;r=json.load(open('${r}'));print(r.get('verifier_result',{}).get('rewards',{}).get('reward',0))")
    # Count tool_call events in dun.jsonl
    jsonl=$(ls "$JD/$t"/*/agent/dun.jsonl 2>/dev/null | head -1)
    tc=0
    if [ -n "$jsonl" ] && [ -f "$jsonl" ]; then
      tc=$(grep -c '"type":"tool_call"' "$jsonl" 2>/dev/null || true)
      [ -z "$tc" ] && tc=0
    fi
    # Premature done: <=9 tool calls AND reward=0
    pd="no"
    if [ "$tc" -le 9 ] 2>/dev/null; then
      pd="yes"
      premature=$((premature+1))
    fi
    total=$((total+1))
    echo "| $t | $run | $reward | \$$cost | $tc | $pd |"
  done
done
echo ""
echo "Total: $total trials, Premature-done (<=9 tool calls): $premature"
} > "$OUT/table.txt"
cat "$OUT/table.txt"
```
