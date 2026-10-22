"""M3: how often would candidate memory-trigger patterns fire, and on what?"""
import json, glob, os, re, sys, random, collections
random.seed(1)
A = {  # assistant says it will keep something
 "A1 I'll/will remember": r"\bI(?:'ll| will| shall) (?:remember|keep (?:that|this|it) in mind|make a note|note (?:that|this|it)|keep a note)\b",
 "A2 make a note": r"\b(?:make|making|made|take|taking) (?:a )?note(?:s)? of\b|\bmake a note\b",
 "A3 remember for later": r"\bremember (?:that|this|it) (?:for|next time|in future|going forward)",
 "A4 noted": r"(?:^|\n)\s*(?:noted|understood)\b[.,!:—-]",
}
U = {
 "U1 starts remember": r"^\W*remember\b",
 "U2 remember that/this/to": r"\bremember (?:that|this|to|:)",
 "U3 future sessions": r"\b(?:future|later|next) (?:sessions?|conversations?|time)\b",
 "U4 don't forget": r"\b(?:don'?t|do not|never) forget\b",
 "U5 keep in mind": r"\bkeep (?:that |this |it )?in mind\b",
 "U6 from now on": r"\bfrom now on\b|\bgoing forward\b",
 "U7 always/never do": r"(?:^|\. )\s*(?:always|never) [a-z]+",
 "U8 memorize/save note": r"\b(?:memori[sz]e|save (?:this|that|a note)|add (?:this|that) to (?:your )?memory)\b",
}
def dun_entries():
    for f in glob.glob(os.path.expanduser("~/.dun/sessions/*/*.jsonl")):
        for l in open(f, errors="replace"):
            try: e = json.loads(l)
            except Exception: continue
            k, c = e.get("Kind"), e.get("Content") or ""
            if k in ("user", "assistant") and c.strip():
                yield "dun", k, c
def claude_user():
    for f in glob.glob(os.path.expanduser("~/.claude/projects/*/*.jsonl")):
        for l in open(f, errors="replace"):
            try: e = json.loads(l)
            except Exception: continue
            if e.get("type") != "user" or e.get("isMeta") or e.get("isSidechain"): continue
            m = e.get("message") or {}
            c = m.get("content")
            if isinstance(c, list):
                c = "\n".join(p.get("text", "") for p in c if isinstance(p, dict) and p.get("type") == "text")
            if not isinstance(c, str) or not c.strip(): continue
            if c.lstrip().startswith("<") or "tool_use_id" in c: continue  # command output, reminders
            if "This session is being continued" in c: continue
            yield "claude", "user", c
def claude_asst():
    for f in glob.glob(os.path.expanduser("~/.claude/projects/*/*.jsonl")):
        for l in open(f, errors="replace"):
            try: e = json.loads(l)
            except Exception: continue
            if e.get("type") != "assistant" or e.get("isSidechain"): continue
            for p in (e.get("message") or {}).get("content") or []:
                if isinstance(p, dict) and p.get("type") == "text" and p.get("text", "").strip():
                    yield "claude", "assistant", p["text"]
tot = collections.Counter(); hits = collections.defaultdict(list)
for src in (dun_entries(), claude_user(), claude_asst()):
    for s, k, c in src:
        tot[(s, k)] += 1
        pats = A if k == "assistant" else U
        for name, rx in pats.items():
            for mt in re.finditer(rx, c, re.I | re.M):
                i = mt.start()
                hits[(s, name)].append(c[max(0, i-90):i+110].replace("\n", " "))
                break
print("totals", dict(tot))
for key in sorted(hits):
    s, name = key
    k = "assistant" if name.startswith("A") else "user"
    n = len(hits[key]); print(f"\n== {s} {name}: {n} / {tot[(s,k)]} ({100*n/max(1,tot[(s,k)]):.2f}%)")
    for x in random.sample(hits[key], min(int(sys.argv[1]) if len(sys.argv)>1 else 6, n)):
        print("   ·", x[:200])
