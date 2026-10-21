import json, sys, time
sys.argv = [sys.argv[0], sys.argv[1]]
src = open(sys.argv[0].replace("replay2", "replay")).read().split("V = {")[0]
exec(src)
tools = turn["tools"]; warm = dict(turn, max_tokens=200)
V = {
 "V4 same tools + none": dict(base, messages=fork, tools=tools, max_tokens=1500, tool_choice="none"),
 "V1 same tools (output check)": dict(base, messages=fork, tools=tools, max_tokens=1500),
}
for trial in range(2):
    for name, body in V.items():
        w = post(warm); r = post(body)
        if trial == 0: body = None
        print(json.dumps({"trial": trial, "variant": name, "warm": {k: w.get(k) for k in ("cache_n","prompt_n")}, "fork": r}), flush=True)
