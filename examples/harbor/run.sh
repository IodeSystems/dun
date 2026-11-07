#!/usr/bin/env bash
# Run dun on Terminal-Bench tasks one at a time, stopping once the spend measured
# from dun's usage events passes $BUDGET.
#
#   HARBOR=<venv>/bin/harbor TASKS=<terminal-bench-2-1>/tasks ./run.sh task1 task2 ...
#
# DUN_URL / DUN_LLM_KEY / DUN_MODEL (default: ~/.dun/config.json's url and key),
# DUN_EFFORT (low), DUN_PRICE_IN/CACHED/OUT (USD per 1M tokens), BUDGET (0.80).
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
out=jobs/$(date +%Y%m%d-%H%M%S)-$DUN_MODEL-$DUN_EFFORT
mkdir -p "$out"

spent() {
	python3 - "$out" <<'EOF'
import json, pathlib, sys
print(sum((json.loads(p.read_text()).get("agent_result") or {}).get("cost_usd") or 0
          for p in pathlib.Path(sys.argv[1]).glob("*/*__*/result.json")))
EOF
}

for t in "$@"; do
	s=$(spent)
	if python3 -c "import sys; sys.exit(float('$s') < float('$BUDGET'))"; then
		echo "stop: spent \$$s >= \$$BUDGET before $t"
		break
	fi
	echo "== $t (spent so far \$$s)"
	"$HARBOR" run -p "$TASKS/$t" --agent dun_agent:Dun -m "$DUN_MODEL" -e docker -k 1 -n 1 \
		-o "$out" --job-name "$t" 2>&1 | grep -E "Mean|Exceptions|│ +[0-9]" || true
done
echo "spent \$$(spent) -> $out"
