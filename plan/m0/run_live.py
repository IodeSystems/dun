import json, os, re, subprocess, sys, time, select
S, dun, probes_md = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(probes_md).read()
probes = []
for line in text.splitlines():
    m = re.match(r'^\| ([a-z0-9-]+) \| "(.+?)" \| "(.+?)" \|$', line)
    if m: probes.append((m[1], "L", m[2]))
for m in re.finditer(r'^- "(.+?)"$', text.split("Negative probes")[1], re.M):
    probes.append(("", "N", m[1]))
env = dict(os.environ, PATH=f"{S}/m0/shim:" + os.environ["PATH"])
results = []
for note, kind, q in probes:
    time.sleep(1.2)  # session files are named to the second: avoid two runs sharing one
    p = subprocess.Popen([dun, "-p", "--rag", "--no-suggest"], cwd=f"{S}/m0/ws", env=env,
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    p.stdin.write(json.dumps({"type": "user", "content": q}) + "\n"); p.stdin.flush()
    docs, usage, t0 = None, None, time.time()
    while time.time() - t0 < 40:
        r, _, _ = select.select([p.stdout], [], [], 5)
        if not r: continue
        line = p.stdout.readline()
        if not line: break
        try: ev = json.loads(line)
        except ValueError: continue
        if ev.get("type") == "notification" and ev.get("kind") == "docs":
            docs = ev
            break  # surfacing is done before the model call; stop here
        if ev.get("type") == "usage": usage = ev
        if ev.get("type") == "done": break
    try:
        p.kill(); p.wait(timeout=10)
    except Exception:
        p.kill()
    surfaced = [os.path.basename(d.get("id", "")).removesuffix(".md") for d in (docs or {}).get("docs", [])]
    res = {"note": note, "kind": kind, "query": q, "surfaced": surfaced,
           "scores": [round(d.get("score", 0), 3) for d in (docs or {}).get("docs", [])],
           "right": (note in surfaced) if kind == "L" else None,
           "bytes": (docs or {}).get("bytes", 0), "tokens": (docs or {}).get("tokens", 0),
           "latency_ms": (docs or {}).get("latency_ms"),
           "surfacing": (usage or {}).get("surfacing"), "secs": round(time.time() - t0, 1)}
    results.append(res)
    print(json.dumps({k: res[k] for k in ("kind", "note", "surfaced", "scores", "bytes", "latency_ms", "secs")}), flush=True)
json.dump(results, open(f"{S}/m0/results_live.json", "w"), indent=1)
