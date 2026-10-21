"""M1: replay a captured dun turn, then fork variants right after it; report cache_n/prompt_n.
Usage: K=<llm key> python3 -I replay.py <dir>  — <dir>/cap/013-{req.json,resp.txt} is the
turn captured by proxy.py (the final round of a dun -p run); <dir>/instr.txt is the fork instruction."""
import json, sys, time, urllib.request, os
S, UP, KEY = sys.argv[1], "https://llm.iodesystems.com", os.environ.get("K", "")
turn = json.load(open(f"{S}/cap/013-req.json"))
def sse(text):
    content, last = "", {}
    for l in text.splitlines():
        if not l.startswith("data: {"): continue
        j = json.loads(l[6:])
        for c in j.get("choices") or []:
            content += (c.get("delta") or {}).get("content") or ""
        if "timings" in j: last = j["timings"]
        if j.get("usage"): last.setdefault("_u", j["usage"])
    return content, last
reply, _ = sse(open(f"{S}/cap/013-resp.txt").read())
INSTR = open(f"{S}/instr.txt").read()
def post(body):
    req = urllib.request.Request(UP + "/v1/chat/completions", data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", **({"Authorization": "Bearer " + KEY} if KEY else {})})
    t0 = time.time()
    try:
        txt = urllib.request.urlopen(req, timeout=600).read().decode()
    except urllib.error.HTTPError as e:
        return {"err": e.code, "body": e.read().decode()[:300]}
    c, tm = sse(txt)
    return {"cache_n": tm.get("cache_n"), "prompt_n": tm.get("prompt_n"), "dt": round(time.time() - t0, 2), "out": c[-400:]}
fork = turn["messages"] + [{"role": "assistant", "content": reply}, {"role": "user", "content": INSTR}]
base = {k: turn[k] for k in ("model", "stream", "stream_options")}
first = turn["tools"][0]["function"]["name"]
V = {
 "V0 no tools (suggest today)": dict(base, messages=fork, max_tokens=24, response_format={"type": "json_object"}),
 "V1 same tools": dict(base, messages=fork, tools=turn["tools"], max_tokens=24),
 "V1j same tools + json_object": dict(base, messages=fork, tools=turn["tools"], max_tokens=24, response_format={"type": "json_object"}),
 "V2 same tools + forced named": dict(base, messages=fork, tools=turn["tools"], max_tokens=24,
                                      tool_choice={"type": "function", "function": {"name": "session_state"}}),
 "V3 same tools + required": dict(base, messages=fork, tools=turn["tools"], max_tokens=24, tool_choice="required"),
}
warm = dict(turn, max_tokens=200)
for trial in range(2):
    for name, body in V.items():
        for delay in ((0, 3.5) if name.startswith("V1 ") else (0,)):
            w = post(warm)
            time.sleep(delay)
            r = post(body)
            print(json.dumps({"trial": trial, "variant": name, "delay": delay, "warm": w, "fork": r}), flush=True)
