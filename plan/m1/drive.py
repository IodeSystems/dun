import json, subprocess, sys, os, time, threading, queue
S = sys.argv[1]
env = dict(os.environ, DUN_URL="http://127.0.0.1:18732", PATH=f"{S}/m0/shim:" + os.environ["PATH"])
p = subprocess.Popen([f"{S}/dun", "-p"], cwd=f"{S}/m1/ws", env=env, stdin=subprocess.PIPE,
                     stdout=subprocess.PIPE, stderr=open(f"{S}/m1/drive.err", "w"), text=True)
q = queue.Queue()
threading.Thread(target=lambda: ([q.put(l) for l in p.stdout], q.put(None)), daemon=True).start()
def send(o): p.stdin.write(json.dumps(o) + "\n"); p.stdin.flush()
send({"type": "user", "content": "Read sidecall.go and tell me in two sentences what noteSideCall records. Do not edit anything."})
t0, asked, last = time.time(), False, None
while time.time() - t0 < 600:
    try: l = q.get(timeout=5)
    except queue.Empty: continue
    if l is None: break
    ev = json.loads(l)
    if ev["type"] == "usage": last = ev
    if ev["type"] == "done" and not asked:
        asked = True; print("done at", round(time.time() - t0, 1), flush=True); send({"type": "control", "id": "suggest"})
    if ev["type"] == "suggestions":
        print("suggestions", ev["items"], flush=True); break
p.kill()
