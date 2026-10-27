#!/usr/bin/env python3
"""Measure an agent's first model request, as capture.py saved it.

For each DIR (one agent's capture), the largest request that carries the
agent's tools is taken as its first real turn (a title or a check request
carries none). It reports the system prompt's size, the tools' count and
size, and the prompt tokens one model counts for it: the request is sent,
in OpenAI chat form, to MODEL at URL with max_tokens=1, and the server's
usage.prompt_tokens is read. That count includes the model's chat template
(how it lays out tool definitions), and is the same model for every agent.
An Anthropic-format request (Claude Code) is converted first; the JSON
schema of each tool is passed through unchanged.

    measure.py URL MODEL DIR... (the key from $KEY)
"""
import json
import os
import sys
import urllib.request

URL, MODEL, DIRS = sys.argv[1].rstrip("/"), sys.argv[2], sys.argv[3:]
KEY = os.environ.get("KEY", "")


def text_of(c):
    if isinstance(c, str):
        return c
    if isinstance(c, list):
        return "\n".join(text_of(x.get("text", "") if isinstance(x, dict) else x) for x in c)
    return ""


def to_openai(body):
    """(messages, tools, system text) of an OpenAI or Anthropic request."""
    if "input" in body and "messages" not in body:  # OpenAI responses
        msgs = []
        if body.get("instructions"):
            msgs.append({"role": "system", "content": body["instructions"]})
        inp = body["input"] if isinstance(body["input"], list) else [{"role": "user", "content": body["input"]}]
        for m in inp:
            if m.get("type", "message") == "message":
                role = {"developer": "system"}.get(m.get("role"), m.get("role"))
                msgs.append({"role": role, "content": text_of(m.get("content"))})
        tools = [{"type": "function", "function": {"name": t.get("name"), "description": t.get("description", ""),
                                                   "parameters": t.get("parameters") or {"type": "object"}}}
                 for t in body.get("tools", []) if t.get("type") == "function"]
        return msgs, tools
    if "system" in body or any("input_schema" in t for t in body.get("tools", [])):  # Anthropic
        msgs = []
        if body.get("system"):
            msgs.append({"role": "system", "content": text_of(body["system"])})
        for m in body.get("messages", []):
            msgs.append({"role": m["role"], "content": text_of(m.get("content"))})
        tools = [{"type": "function", "function": {"name": t["name"], "description": t.get("description", ""),
                                                   "parameters": t.get("input_schema") or {"type": "object"}}}
                 for t in body.get("tools", []) if "name" in t]
        return msgs, tools
    msgs = [{"role": m.get("role"), "content": text_of(m.get("content"))} for m in body.get("messages", [])]
    tools = [t for t in body.get("tools", []) if t.get("type") == "function"]
    return msgs, tools


def prompt_tokens(msgs, tools):
    # Some chat templates (Qwen's) take a system message only first: every
    # system message's text goes into one, at the front, unchanged.
    system = "\n\n".join(m["content"] for m in msgs if m["role"] == "system")
    msgs = ([{"role": "system", "content": system}] if system else []) + [m for m in msgs if m["role"] != "system"]
    req = {"model": MODEL, "messages": msgs, "max_tokens": 1, "temperature": 0}
    if tools:
        req["tools"] = tools
    r = urllib.request.Request(URL + "/v1/chat/completions", json.dumps(req).encode(),
                               {"content-type": "application/json", "authorization": "Bearer " + KEY})
    with urllib.request.urlopen(r, timeout=600) as f:
        return json.load(f)["usage"]["prompt_tokens"]


def first_turn(d):
    best = None
    for name in sorted(os.listdir(d)):
        if not name.endswith(".json"):
            continue
        cap = json.load(open(os.path.join(d, name)))
        body = cap.get("body") or {}
        if not isinstance(body, dict):
            continue
        msgs, tools = to_openai(body)
        if not msgs:
            continue
        size = len(json.dumps(body))
        if best is None or (len(tools), size) > (len(best[1]), best[2]):
            best = (msgs, tools, size, name)
    return best


rows = []
for d in DIRS:
    got = first_turn(d)
    if not got:
        print(f"{d}: no request captured", file=sys.stderr)
        continue
    msgs, tools, _, name = got
    system = "\n".join(m["content"] for m in msgs if m["role"] == "system")
    user = [m for m in msgs if m["role"] != "system"]
    whole = prompt_tokens(msgs, tools)
    bare = prompt_tokens(msgs, [])  # the same, without the tool definitions
    rows.append({"agent": os.path.basename(d.rstrip("/")), "request": name, "system_chars": len(system),
                 "tools": len(tools), "tool_names": [t["function"]["name"] for t in tools],
                 "tool_schema_chars": len(json.dumps(tools)), "tokens_total": whole, "tokens_tools": whole - bare,
                 "tokens_without_tools": bare, "messages": len(user)})
print(json.dumps(rows, indent=1))
