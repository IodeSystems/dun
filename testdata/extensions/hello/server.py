"""A fake MCP server (stdio) for dun's test extension: `greet` answers a
slash command, and `find` is search-shaped, so dun's proactive search
(hook turn.before) surfaces its hit as an aside."""
import json, sys

FACT = {"doc_id": "hello://zebra-fact", "title": "Zebra fact", "score": 1.0,
        "snippet": "Zebra deploys need the --frostbite flag; without it the rollout hangs at 40%."}
TOOLS = [
    {"name": "greet", "description": "Say hello.",
     "inputSchema": {"type": "object", "properties": {"name": {"type": "string"}}}},
    {"name": "find", "description": "Search the zebra notes.",
     "inputSchema": {"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]}},
]

def call(name, args):
    if name == "greet":
        return "hello, " + (args.get("name") or "nobody")
    if name == "find":
        hits = [FACT] if "zebra" in (args.get("query") or "").lower() else []
        return json.dumps({"hits": hits})
    raise KeyError(name)

for line in sys.stdin:
    msg = json.loads(line)
    if "id" not in msg:
        continue
    m, p = msg["method"], msg.get("params") or {}
    out = {"jsonrpc": "2.0", "id": msg["id"]}
    if m == "initialize":
        out["result"] = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}},
                         "serverInfo": {"name": "hello", "version": "1"}}
    elif m == "tools/list":
        out["result"] = {"tools": TOOLS}
    elif m == "tools/call":
        try:
            out["result"] = {"content": [{"type": "text", "text": call(p["name"], p.get("arguments") or {})}]}
        except KeyError:
            out["error"] = {"code": -32602, "message": "no tool " + p["name"]}
    elif m == "ping":
        out["result"] = {}
    else:
        out["error"] = {"code": -32601, "message": "no method " + m}
    sys.stdout.write(json.dumps(out) + "\n")
    sys.stdout.flush()
