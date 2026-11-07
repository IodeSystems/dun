"""dun as a Harbor installed agent, for running dun on the Harness Leaderboard's
benchmarks (huggingface.co/spaces/joelniklaus/harness-leaderboard) locally.

    harbor run --agent dun_agent:Dun -m fireworks-glm-5p3-flash -p <tasks> ...

Install uploads prebuilt static binaries (make -C examples/harbor bin) instead of
building in the trial container: dun and mcpshell, plus the host's CA bundle,
because many task images have no ca-certificates and dun is a static Go binary.
poly-lsp and raglit stay off, as with dun's defaults.

The model is reached through an OpenAI-compatible endpoint (DUN_URL, DUN_LLM_KEY
from the worker's environment). The reasoning effort is dun's own /effort level,
written to $DUN_HOME/effort.json and sent as reasoning_effort (DUN_REASONING=openai).

dun runs in engine mode (-p): the JSON event log goes to /logs/agent/dun.jsonl,
and its usage event gives the token counts. cost_usd is priced from those counts
with DUN_PRICE_IN / DUN_PRICE_CACHED / DUN_PRICE_OUT (USD per 1M tokens); without
them it is left unset.
"""

from __future__ import annotations

import json
import os
import shlex
from pathlib import Path
from typing import override

from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

HERE = Path(__file__).resolve().parent
BIN = Path(os.environ.get("DUN_HARBOR_BIN", HERE / "bin"))
REMOTE = "/installed-agent/dun"
HOME = "/installed-agent/dun-home"
LOG = "/logs/agent/dun.jsonl"
# dun's own cut. Harbor's per-task agent timeout is the one that decides; this only
# keeps dun from running forever if Harbor's is unset.
TIMEOUT = os.environ.get("DUN_HARBOR_TIMEOUT", "3h")


class Dun(BaseInstalledAgent):
    @staticmethod
    @override
    def name() -> str:
        return "dun"

    @override
    def get_version_command(self) -> str | None:
        return f"{REMOTE}/dun --version"

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        for f in ("dun", "mcpshell", "ca.crt"):
            if not (BIN / f).is_file():
                raise RuntimeError(f"{BIN / f} missing: run make -C examples/harbor bin")
        await self.exec_as_root(environment, f"mkdir -p {REMOTE} {HOME}")
        await environment.upload_dir(BIN, REMOTE)
        effort = os.environ.get("DUN_EFFORT", "low")
        await self.exec_as_root(
            environment,
            f"chmod 755 {REMOTE}/dun {REMOTE}/mcpshell && "
            f"echo {shlex.quote(json.dumps({'level': effort}))} > {HOME}/effort.json && "
            f"chmod -R a+rwX {HOME}",
        )

    @override
    @with_prompt_template
    async def run(self, instruction: str, environment: BaseEnvironment, context: AgentContext) -> None:
        env = {
            "PATH": f"{REMOTE}:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
            "SSL_CERT_FILE": f"{REMOTE}/ca.crt",
            "DUN_HOME": HOME,
            "DUN_URL": os.environ["DUN_URL"],
            "DUN_LLM_KEY": os.environ["DUN_LLM_KEY"],
            "DUN_MODEL": self.model_name or os.environ["DUN_MODEL"],
            "DUN_REASONING": "openai",
            "DUN_NO_AUTOBUILD": "1",
        }
        event = json.dumps({"type": "user", "content": instruction})
        await self.exec_as_agent(
            environment,
            command=(
                "mkdir -p /logs/agent; "
                f"printf '%s\\n' {shlex.quote(event)} | "
                f'dun -p --workspace "$PWD" --timeout {TIMEOUT} > {LOG} 2> /logs/agent/dun.stderr; '
                "echo dun exit $? >> /logs/agent/dun.stderr"
            ),
            env=env,
        )

    @override
    def populate_context_post_run(self, context: AgentContext) -> None:
        log = self.logs_dir / "dun.jsonl"
        if not log.is_file():
            return
        usage = None
        for line in log.read_text(errors="replace").splitlines():
            try:
                e = json.loads(line)
            except ValueError:
                continue
            if e.get("type") == "usage":
                usage = e
        if usage is None:
            return
        processed, cached, generated = (int(usage.get(k) or 0) for k in ("processed", "cached", "generated"))
        context.n_input_tokens = processed + cached
        context.n_cache_tokens = cached
        context.n_output_tokens = generated
        prices = [os.environ.get(k) for k in ("DUN_PRICE_IN", "DUN_PRICE_CACHED", "DUN_PRICE_OUT")]
        if all(prices):
            pin, pcached, pout = (float(p) / 1e6 for p in prices)  # type: ignore[arg-type]
            context.cost_usd = processed * pin + cached * pcached + generated * pout
