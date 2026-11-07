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
            "SSL_CERT_FILE": f"{REMOTE}/ca.crt",
            "DUN_HOME": HOME,
            "DUN_URL": os.environ["DUN_URL"],
            "DUN_LLM_KEY": os.environ["DUN_LLM_KEY"],
            "DUN_MODEL": self.model_name or os.environ["DUN_MODEL"],
            "DUN_REASONING": "openai",
            # GLM-5.3 on Fireworks cannot stop thinking: it refuses reasoning_effort
            # "none", so a fold (effort off) is sent as low instead.
            "DUN_EFFORT_OFF": os.environ.get("DUN_EFFORT_OFF", "low"),
            "DUN_NO_AUTOBUILD": "1",
        }
        # dun's round limit per turn (default 400). A benchmark may set it lower as
        # a cost guard: report it with the results.
        if os.environ.get("DUN_MAX_TURNS"):
            env["DUN_MAX_TURNS"] = os.environ["DUN_MAX_TURNS"]
        event = json.dumps({"type": "user", "content": instruction})
        await self.exec_as_agent(
            environment,
            command=(
                "mkdir -p /logs/agent; "
                # The image's own PATH (an ENV in its Dockerfile) stays: dun goes in front.
                f"export PATH={REMOTE}:$PATH; "
                # The container is the sandbox, and no one is there to /file add a
                # path: open every top-level directory to dun's file tools, for
                # writing, unindexed. Not / itself (dun refuses it), not the
                # kernel's or harbor's own, and not the workspace (already open).
                "p=; for d in /*/; do d=${d%/}; [ -L \"$d\" ] && continue; "
                'case "$d" in /proc|/sys|/dev|/logs|/installed-agent|"$PWD") continue;; esac; '
                '''p="$p${p:+,}\\"$d\\":{\\"on\\":true,\\"mode\\":\\"write\\",\\"index\\":false}"; done; '''
                f"printf '{{\"paths\":{{%s}}}}' \"$p\" > {HOME}/paths.json; "
                f"printf '%s\\n' {shlex.quote(event)} | "
                f'dun -p --non-interactive --workspace "$PWD" --timeout {TIMEOUT} > {LOG} 2> /logs/agent/dun.stderr; '
                "echo dun exit $? >> /logs/agent/dun.stderr"
            ),
            env=env,
        )

    @override
    def populate_context_post_run(self, context: AgentContext) -> None:
        log = self.logs_dir / "dun.jsonl"
        if not log.is_file():
            return
        # usage events are per turn, so they are summed. A turn killed before
        # its usage event (an agent timeout) is counted from its request
        # events instead: those cover the parent's rounds only, not children's.
        processed = cached = generated = 0
        pend = [0, 0, 0]
        seen = False
        for line in log.read_text(errors="replace").splitlines():
            try:
                e = json.loads(line)
            except ValueError:
                continue
            t = e.get("type")
            if t == "usage":
                seen = True
                processed += int(e.get("processed") or 0)
                cached += int(e.get("cached") or 0)
                generated += int(e.get("generated") or 0)
                if e.get("turns"):  # a turn with no rounds says nothing about the ones before it
                    pend = [0, 0, 0]
            elif t == "request" and e.get("phase") == "done":
                seen = True
                c = int(e.get("cached") or 0)
                pend[0] += int(e.get("prompt") or 0) - c
                pend[1] += c
                pend[2] += int(e.get("gen") or 0)
        if not seen:
            return
        processed, cached, generated = processed + pend[0], cached + pend[1], generated + pend[2]
        context.n_input_tokens = processed + cached
        context.n_cache_tokens = cached
        context.n_output_tokens = generated
        prices = [os.environ.get(k) for k in ("DUN_PRICE_IN", "DUN_PRICE_CACHED", "DUN_PRICE_OUT")]
        if all(prices):
            pin, pcached, pout = (float(p) / 1e6 for p in prices)  # type: ignore[arg-type]
            context.cost_usd = processed * pin + cached * pcached + generated * pout
