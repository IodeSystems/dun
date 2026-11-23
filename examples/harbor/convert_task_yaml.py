#!/usr/bin/env python3
"""Convert a TB2.1 task.yaml to the minimal task.toml harbor 0.26.0 requires.

Usage: convert_task_yaml.py <task_dir> <out_path>

Reads <task_dir>/task.yaml, writes <out_path> as a TOML file with the
instruction and metadata. Harbor's TaskConfig requires a [task] section
with at least a name; the instruction is the core content the agent sees.
"""
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    sys.exit("PyYAML is required: pip install pyyaml")


def toml_escape(s: str) -> str:
    """Escape a string for use in a TOML basic (double-quoted) string."""
    return s.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")


def main() -> None:
    if len(sys.argv) != 3:
        sys.exit(f"usage: {sys.argv[0]} <task_dir> <out_path>")

    task_dir = Path(sys.argv[1])
    out_path = Path(sys.argv[2])

    yaml_path = task_dir / "task.yaml"
    if not yaml_path.exists():
        sys.exit(f"no task.yaml in {task_dir}")

    data = yaml.safe_load(yaml_path.read_text())
    instruction = data.get("instruction", "").strip()
    # Harbor 0.26.0's TaskConfig validates task.name against an 'org/name'
    # package-name pattern; a bare task id like "extract-elf" is rejected.
    # Prefix with a fixed org so the name is stable across runs.
    raw_name = data.get("name") or task_dir.name
    name = raw_name if "/" in raw_name else f"tb2-1/{raw_name}"
    author = data.get("author_name", "")
    difficulty = data.get("difficulty", "")

    # Harbor 0.26.0 requires a [environment] section with docker_image.
    # TB2.1 tasks use alexgshaw/<task-name>:20251031 (from the original
    # 2026-11-06 run's trial.log). The task dir may also carry a Dockerfile
    # and docker-compose.yaml, but the published image is the stable choice.
    image = data.get("docker_image") or f"alexgshaw/{task_dir.name}:20251031"

    lines = [
        "[task]",
        f'name = "{toml_escape(name)}"',
        'instruction = """',
        instruction,
        '"""',
    ]
    if author:
        lines.append(f"author_name = \"{toml_escape(author)}\"")
    if difficulty:
        lines.append(f"difficulty = \"{toml_escape(difficulty)}\"")
    lines.append("")
    lines.append("[environment]")
    lines.append(f'docker_image = "{toml_escape(image)}"')
    lines.append("")

    out_path.write_text("\n".join(lines))

    # Harbor 0.26.0's Task._validate_tests requires a tests/test.sh in the
    # task dir (paths.test_path_for("linux") -> tests/test.sh). TB2.1 tasks
    # instead ship a top-level run-tests.sh (which does `uv run pytest
    # $TEST_DIR/test_outputs.py`) plus tests/test_outputs.py. The old harbor's
    # -p flag ran run-tests.sh directly on the host; the new one runs the
    # verifier INSIDE the docker container (environment_mode: shared), where the
    # task dir's files are mounted at /app. So the shim must reference
    # CONTAINER-relative paths, not host paths: hard-coding the host task dir
    # (dest/tests, task_dir/run-tests.sh) makes TEST_DIR point at a path that
    # does not exist in the container, the reward file is never written, and
    # every trial ends in RewardFileNotFoundError.
    #
    # Derive the paths from the shim's own location ($0) instead: the shim
    # lives at <taskdir>/tests/test.sh, so $(dirname "$0") is the tests dir
    # (containing test_outputs.py) and $(dirname "$0")/.. is the task root
    # (containing run-tests.sh). This is robust to wherever harbor mounts the
    # task dir. Written next to the task.toml.
    if out_path.parent != task_dir:
        dest = out_path.parent
        (dest / "instruction.md").write_text(instruction + "\n")
        if (task_dir / "run-tests.sh").exists():
            shim = dest / "tests" / "test.sh"
            shim.parent.mkdir(parents=True, exist_ok=True)
            shim.write_text(
                "#!/bin/bash\n"
                # No `set -e`: the test run may fail, and the shim must still
                # write the reward file. The reward file (not the exit code) is
                # how harbor reads the pass/fail signal.
                #
                # $(dirname "$0") resolves to the container-side tests dir (the
                # shim's own location, /app/tests), not a host path. export ...
                # on its own line (not `VAR=value export VAR`, which scopes the
                # assignment to the export builtin and leaves TEST_DIR unset
                # here) so run-tests.sh inherits it.
                'export TEST_DIR="$(cd "$(dirname "$0")" && pwd)"\n'
                # Run the task's tests; capture the exit code.
                'bash "$(cd "$(dirname "$0")/.." && pwd)/run-tests.sh"\n'
                'rc=$?\n'
                # Harbor 0.26.0 reads the reward from /logs/verifier/reward.txt
                # (a float: 1.0 = pass, 0.0 = fail). The verifier dir is
                # bind-mounted from trial_dir/verifier on the host. TB2.1's
                # run-tests.sh runs pytest and exits without writing a reward
                # file, so the shim translates the exit code into one.
                'mkdir -p /logs/verifier\n'
                'if [ "$rc" -eq 0 ]; then\n'
                '    echo "1.0" > /logs/verifier/reward.txt\n'
                'else\n'
                '    echo "0.0" > /logs/verifier/reward.txt\n'
                'fi\n'
                'exit 0\n'
            )
            shim.chmod(0o755)


if __name__ == "__main__":
    main()
