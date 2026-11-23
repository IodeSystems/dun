#!/usr/bin/env bash
# Run dun on Terminal-Bench tasks one at a time, stopping once the spend measured
# from dun's usage events passes $BUDGET, or at the first clear harness error.
#
#   HARBOR=<venv>/bin/harbor TASKS=<terminal-bench-2-1>/tasks ./run.sh task1 task2 ...
#
# DUN_URL / DUN_LLM_KEY / DUN_MODEL (default: ~/.dun/config.json's url and key),
# DUN_EFFORT (low), DUN_PRICE_IN/CACHED/OUT (USD per 1M tokens), BUDGET (0.80),
# ATTEMPTS (1, harbor -k), OUT (a jobs dir to add to; default a new one),
# DUN_HARBOR_BIN (the binaries to upload; default ./bin), HARBOR_ARGS (more harbor
# run flags, e.g. "--agent-timeout-multiplier 3" for a slower model),
# DUN_MEMORY (0: memory off; set 1 for a run that wants it), DUN_CODE_SEARCH (0: dund's
# code and docs search off; set 1 to turn it on), DUN_USER_RULES (0: the home
# directory's AGENTS.md / CLAUDE.md / .claude/CLAUDE.md not read; set 1 to read them),
# DUN_FORMAT_PROMPT (0: the answer-formatting prompt off; set 1 to add it).
#
# A clear error stops the run (exit 2): a trial exception other than an agent
# timeout, or dun ending before its first model request. Those are the harness's
# failures, not the model's, and every trial after one would be noise.
set -euo pipefail
cd "$(dirname "$0")"
: "${HARBOR:?path to the harbor CLI}" "${TASKS:?path to the tasks directory}"
cfg=~/.dun/config.json
export DUN_URL=${DUN_URL:-$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["url"])' "$cfg")}
export DUN_LLM_KEY=${DUN_LLM_KEY:-$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["key"])' "$cfg")}
export DUN_MODEL=${DUN_MODEL:-fireworks-glm-5p3-flash} DUN_EFFORT=${DUN_EFFORT:-low}
export DUN_PRICE_IN=${DUN_PRICE_IN:-0.15} DUN_PRICE_CACHED=${DUN_PRICE_CACHED:-0.03} DUN_PRICE_OUT=${DUN_PRICE_OUT:-0.5}
export PYTHONPATH=$PWD
BUDGET=${BUDGET:-0.80}
ATTEMPTS=${ATTEMPTS:-1}
out=${OUT:-jobs/$(date +%Y%m%d-%H%M%S)-$DUN_MODEL-$DUN_EFFORT}
mkdir -p "$out"

spent() {
	python3 - "$out" <<'EOF'
import json, pathlib, sys
print(sum((json.loads(p.read_text()).get("agent_result") or {}).get("cost_usd") or 0
          for p in pathlib.Path(sys.argv[1]).glob("*/*__*/result.json")))
EOF
}

# clear_error prints why the trials of job $1 show a harness failure, if any.
# AgentTimeoutError is the one tolerated exception: the agent ran and timed out,
# which is a model outcome, not a harness failure. Every other exception —
# including RewardFileNotFoundError, which is what a broken test shim produces
# (the reward file is never written) — is a harness failure and aborts the run.
clear_error() {
	python3 - "$out/$1" <<'EOF'
import json, pathlib, sys
for p in pathlib.Path(sys.argv[1]).glob("*__*/result.json"):
    r = json.loads(p.read_text())
    exc = (r.get("exception_info") or {}).get("exception_type")
    if exc and exc not in ("AgentTimeoutError",):
        print(f"{p.parent.name}: {exc}"); break
    log = p.parent / "agent" / "dun.jsonl"
    reqs = sum('"type":"request"' in l for l in log.open()) if log.exists() else 0
    if reqs == 0:
        err = p.parent / "agent" / "dun.stderr"
        tail = err.read_text().strip().splitlines()[-1:] if err.exists() else ["no stderr"]
        print(f"{p.parent.name}: dun made no model request ({tail[0] if tail else ''})"); break
EOF
}

# task_path resolves task $1 to a dir that has a task.toml. Harbor 0.26.0
# requires task.toml; TB2.1's original-tasks use task.yaml. If the task dir
# has no task.toml but has a task.yaml, convert it into a STABLE temp dir
# (named after the task, so the jobconfig path and the trial's path agree
# across runs and a resume of the same job works) with the task.toml added,
# and return that path. Prints the path to stdout.
task_path() {
	local task="$1" src="$TASKS/$1"
	if [ -f "$src/task.toml" ]; then
		echo "$src"
		return
	fi
	if [ -f "$src/task.yaml" ]; then
		local tmp="/tmp/harbor-tasks-$task"
		[ -d "$tmp" ] || { rm -rf "$tmp"; cp -r "$src" "$tmp"; }
		python3 "$(dirname "$0")/convert_task_yaml.py" "$src" "$tmp/task.toml"
		echo "$tmp"
		return
	fi
	echo "no task.toml or task.yaml in $src" >&2
	return 1
}

# jobconfig writes a JobConfig YAML for task $1 to $2.
# The dun agent reads its URL and key from DUN_URL / DUN_LLM_KEY env vars
# (exported above), not from the harbor AgentConfig's base_url field.
jobconfig() {
	local task="$1" dest="$2" taskdir
	taskdir=$(task_path "$task") || exit 1
	cat > "$dest" <<EOF
tasks:
  - path: "$taskdir"
environment:
  type: docker
agents:
  - import_path: "dun_agent:Dun"
    model_name: "$DUN_MODEL"
EOF
}

for t in "$@"; do
	s=$(spent)
	if python3 -c "import sys; sys.exit(float('$s') < float('$BUDGET'))"; then
		echo "stop: spent \$$s >= \$$BUDGET before $t"
		break
	fi
	echo "== $t (spent so far \$$s)"
	cfgfile=$(mktemp /tmp/harbor-jobconfig-XXXXXX.yaml)
	jobconfig "$t" "$cfgfile"
	# No grep filter: harbor's real exit code must reach set -e, and a crash
	# traceback must not be hidden. The summary lines are shown for convenience.
	if ! "$HARBOR" run --config "$cfgfile" \
		-o "$out" -k "$ATTEMPTS" -n 1 \
		--job-name "$t" ${HARBOR_ARGS:-} 2>&1 | tee "$out/harbor-$t.log"; then
		echo "abort: harbor run for $t failed; see $out/harbor-$t.log"
		rm -f "$cfgfile"
		exit 2
	fi
	grep -E "Mean|Exceptions|│ +[0-9]" "$out/harbor-$t.log" || true
	rm -f "$cfgfile"
	if why=$(clear_error "$t") && [ -n "$why" ]; then
		echo "abort: $why"
		exit 2
	fi
done
echo "spent \$$(spent) -> $out"
