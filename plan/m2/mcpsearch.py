import json, subprocess, sys
home, q = sys.argv[1], sys.argv[2]
p = subprocess.Popen(["raglit", "serve", "--embedded", "--home", home, "--index", "work"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
def rpc(i, m, params):
    p.stdin.write(json.dumps({"jsonrpc": "2.0", "id": i, "method": m, "params": params}) + "\n"); p.stdin.flush()
    while True:
        r = json.loads(p.stdout.readline())
        if r.get("id") == i: return r
rpc(1, "initialize", {"protocolVersion": "2024-11-05", "capabilities": {}, "clientInfo": {"name": "t", "version": "0"}})
p.stdin.write(json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n"); p.stdin.flush()
for ix in ["", "work", "memory"]:
    args = {"query": q, "limit": 5}
    if ix: args["index"] = ix
    r = rpc(2, "tools/call", {"name": "search", "arguments": args})
    d = json.loads(r["result"]["content"][0]["text"])
    print(repr(ix), [(h["index"], round(h["score"], 9), h["doc_id"].rsplit("/", 1)[-1]) for h in d.get("hits", [])])
p.kill()
