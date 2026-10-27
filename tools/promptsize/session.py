#!/usr/bin/env python3
"""The session benchmark: one task, several agents, the same model.

For each agent and run: a fresh fixture repository (fixture.py), a relay
(relay.py) between the agent and the model server that records every
request with its usage, the agent run one-shot on the task in a clean home,
then the fixture's tests run to see whether the task was done. Results go
to OUT/<agent>-<run>/ (requests.jsonl, the agent's output) and a summary to
OUT/summary.json: per run, done or not, requests, prompt tokens on the
first, the 10th and the last request, the largest, and the total.

    KEY=… session.py OUT URL MODEL RUNS AGENT... (dun=PATH pi=PATH opencode)

An agent is dun=<binary>, pi=<pi bin script> or opencode (on PATH).
"""
import json
import os
import shutil
import socket
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
OUT, URL, MODEL, RUNS, AGENTS = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4]), sys.argv[5:]
TASK = subprocess.run([sys.executable, os.path.join(HERE, "fixture.py"), "--task"], capture_output=True, text=True).stdout.strip()
NODE = os.path.join(os.environ.get("NVM_BIN", ""), "node") if os.environ.get("NVM_BIN") else "node"
TIMEOUT = int(os.environ.get("SESSION_TIMEOUT", "1200"))


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def run_agent(kind, path, run_dir, repo, port):
    home = os.path.join(run_dir, "home")
    os.makedirs(home, exist_ok=True)
    env = dict(os.environ, HOME=home, POLY_LSP_NO_DAEMON="1")
    base = f"http://127.0.0.1:{port}"
    if kind == "dun":
        env.update(DUN_HOME=os.path.join(home, ".dun"), DUN_REGISTRY="off")
        cmd = [path, "--url", base, "--model", MODEL, "--no-rag", "--no-lsp", TASK]
    elif kind == "pi":
        agent = os.path.join(home, "pi-agent")
        os.makedirs(agent, exist_ok=True)
        with open(os.path.join(agent, "models.json"), "w") as f:
            json.dump({"providers": {"bench": {"baseUrl": base + "/v1", "api": "openai-completions", "apiKey": "x",
                                               "models": [{"id": MODEL, "name": MODEL, "contextWindow": 131072, "maxTokens": 8192}]}}}, f)
        env.update(PI_CODING_AGENT_DIR=agent)
        env["HOME"] = os.environ["HOME"]  # pi's node shim needs the real HOME; its own state is in PI_CODING_AGENT_DIR
        cmd = [NODE, path, "-p", "--no-skills", "--no-context-files", "--provider", "bench", "--model", MODEL, TASK]
    elif kind == "opencode":
        cfg = os.path.join(home, "cfg", "opencode")
        os.makedirs(cfg, exist_ok=True)
        with open(os.path.join(cfg, "opencode.json"), "w") as f:
            json.dump({"$schema": "https://opencode.ai/config.json", "model": "bench/" + MODEL,
                       "permission": {"edit": "allow", "bash": "allow", "webfetch": "deny"},
                       "provider": {"bench": {"npm": "@ai-sdk/openai-compatible", "name": "bench",
                                              "options": {"baseURL": base + "/v1", "apiKey": "x"},
                                              "models": {MODEL: {"name": MODEL}}}}}, f)
        env["HOME"] = os.environ["HOME"]  # opencode's packages cache; config and state are XDG
        env.update(XDG_CONFIG_HOME=os.path.join(home, "cfg"), XDG_DATA_HOME=os.path.join(home, "data"),
                   XDG_STATE_HOME=os.path.join(home, "state"))
        cmd = ["opencode", "run", "-m", "bench/" + MODEL, TASK]
    else:
        raise SystemExit("unknown agent " + kind)
    t0 = time.time()
    with open(os.path.join(run_dir, "stdout.txt"), "w") as o, open(os.path.join(run_dir, "stderr.txt"), "w") as e:
        try:
            rc = subprocess.run(cmd, cwd=repo, env=env, stdin=subprocess.DEVNULL, stdout=o, stderr=e, timeout=TIMEOUT).returncode
        except subprocess.TimeoutExpired:
            rc = "timeout"
    return rc, round(time.time() - t0)


def summarize(log):
    reqs, cached = [], 0
    if os.path.exists(log):
        for line in open(log):
            r = json.loads(line)
            if not r["path"].split("?")[0].endswith("/chat/completions"):
                continue  # the agent's token counting or model listing, not a turn
            u = r.get("usage") or {}
            reqs.append(u.get("prompt_tokens") or 0)
            cached += (u.get("prompt_tokens_details") or {}).get("cached_tokens") or 0
    pick = lambda i: reqs[i] if len(reqs) > i else None
    return {"requests": len(reqs), "first": pick(0), "tenth": pick(9), "last": reqs[-1] if reqs else None,
            "largest": max(reqs) if reqs else None, "total": sum(reqs), "cached": cached, "per_request": reqs}


def main():
    os.makedirs(OUT, exist_ok=True)
    rows = []
    for spec in AGENTS:
        kind, _, path = spec.partition("=")
        for n in range(1, RUNS + 1):
            run_dir = os.path.join(OUT, f"{kind}-{n}")
            shutil.rmtree(run_dir, ignore_errors=True)
            repo = os.path.join(run_dir, "repo")
            subprocess.run([sys.executable, os.path.join(HERE, "fixture.py"), repo], check=True)
            port = free_port()
            relay = subprocess.Popen([sys.executable, os.path.join(HERE, "relay.py"), run_dir, str(port), URL],
                                     env=os.environ, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            time.sleep(1)
            try:
                rc, secs = run_agent(kind, path, run_dir, repo, port)
            finally:
                relay.terminate()
            check = subprocess.run(["sh", os.path.join(repo, "check.sh")], capture_output=True, text=True).stdout.strip()
            row = {"agent": kind, "run": n, "rc": rc, "secs": secs, "done": check == "OK", "check": check,
                   **summarize(os.path.join(run_dir, "requests.jsonl"))}
            rows.append(row)
            print(json.dumps({k: v for k, v in row.items() if k != "per_request"}), flush=True)
    with open(os.path.join(OUT, "summary.json"), "w") as f:
        json.dump(rows, f, indent=1)


main()
