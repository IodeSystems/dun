"""M2 live: session A says a fact and runs /remember; session B asks about it."""
import json, subprocess, sys, os, time, threading, queue
S, ask, ctrl = sys.argv[1], sys.argv[2], (sys.argv[3] if len(sys.argv) > 3 else None)
if ctrl == "-": ctrl = ""
env = dict(os.environ, DUN_URL="http://127.0.0.1:18733", DUN_HOME=f"{S}/m2/home", PATH=f"{S}/m0/shim:" + os.environ["PATH"])
p = subprocess.Popen([f"{S}/dun", "-p", "--rag", "--no-suggest"], cwd=f"{S}/m2/ws", env=env, stdin=subprocess.PIPE,
                     stdout=subprocess.PIPE, stderr=open(f"{S}/m2/err-{int(time.time())}.log", "w"), text=True)
q = queue.Queue()
threading.Thread(target=lambda: ([q.put(l) for l in p.stdout], q.put(None)), daemon=True).start()
def send(o): p.stdin.write(json.dumps(o) + "\n"); p.stdin.flush()
send({"type": "user", "content": ask})
t0, asked = time.time(), False
while time.time() - t0 < 500:
    try: l = q.get(timeout=5)
    except queue.Empty: continue
    if l is None: break
    ev = json.loads(l)
    t = ev["type"]
    if t == "notification" and ev.get("kind") == "docs": print("DOCS", json.dumps(ev)[:900], flush=True)
    if t == "tool_call": print("TOOL", json.dumps(ev)[:300], flush=True)
    if t == "message": print("REPLY", ev.get("content", "")[:400], flush=True)
    if t == "done":
        print("done at", round(time.time() - t0, 1), flush=True)
        if ctrl is not None and not asked:
            asked = True; send({"type": "control", "id": "remember", "action": ctrl}); continue
        break
    if t == "control" and ev.get("id") == "remember":
        print("CONTROL", ev.get("message"), flush=True); break
p.kill()
