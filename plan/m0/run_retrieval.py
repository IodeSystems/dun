"""M0 retrieval half: run every probe in plan/m0/probes.md through raglit
search, in bm25 and hybrid, and report whether the right note is in the top
MAXHITS (dun's docsPreparer surfaces MaxHits=2, MinScore 0).

Usage: python3 -I plan/m0/run_retrieval.py <raglit home> <index>
The index must hold ONLY plan/m0/notes/*.md (ingested with --embed).
"""
import os, re, subprocess, sys, json

MAXHITS = 2
home, index = sys.argv[1], sys.argv[2]
here = os.path.dirname(os.path.abspath(__file__))
text = open(os.path.join(here, "probes.md")).read()

probes = []  # (note, kind, query)
for line in text.splitlines():
    m = re.match(r'^\| ([a-z0-9-]+) \| "(.+?)" \| "(.+?)" \|$', line)
    if m:
        probes.append((m[1], "L", m[2]))
        probes.append((m[1], "P", m[3]))
for m in re.finditer(r'^- "(.+?)"$', text.split("Negative probes")[1], re.M):
    probes.append(("", "N", m[1]))


def search(q, mode):
    out = subprocess.run(["raglit", "search", "--embedded", "--home", home, "--index", index,
                          "--mode", mode, "-n", str(MAXHITS), q],
                         capture_output=True, text=True, timeout=120).stdout
    hits = []
    for l in out.splitlines():
        m = re.match(r"^\d+\. \[([\d.e+-]+)\] (\S+)", l)
        if m:
            hits.append((os.path.basename(m[2]).removesuffix(".md"), float(m[1])))
    return hits


rows = []
for note, kind, q in probes:
    row = {"note": note, "kind": kind, "query": q}
    for mode in ("bm25", "hybrid"):
        hits = search(q, mode)
        names = [h[0] for h in hits]
        row[mode] = {"hits": hits,
                     "right": (note in names) if kind != "N" else None,
                     "rank": (names.index(note) + 1) if note in names else None,
                     "wrong": len([n for n in names if n != note])}
    rows.append(row)

json.dump(rows, open(os.path.join(here, "results_retrieval.json"), "w"), indent=1)


def tally(kind, mode):
    rs = [r for r in rows if r["kind"] == kind]
    if kind == "N":
        surfaced = sum(len(r[mode]["hits"]) for r in rs)
        return f"{surfaced} hits surfaced on {len(rs)} negatives"
    right = sum(1 for r in rs if r[mode]["right"])
    first = sum(1 for r in rs if r[mode]["rank"] == 1)
    wrong = sum(r[mode]["wrong"] for r in rs)
    return f"right note in top {MAXHITS}: {right}/{len(rs)} (rank 1: {first}); wrong notes surfaced: {wrong}"


for kind, label in (("L", "lexical"), ("P", "paraphrase"), ("N", "negative")):
    for mode in ("bm25", "hybrid"):
        print(f"{label:10} {mode:6} {tally(kind, mode)}")
print()
for r in rows:
    if r["kind"] == "N":
        print(f"N  {r['query'][:50]:50} bm25={r['bm25']['hits']} hybrid={[(n, round(s, 3)) for n, s in r['hybrid']['hits']]}")
    else:
        print(f"{r['kind']}  {r['note']:28} bm25 rank={r['bm25']['rank']} hybrid rank={r['hybrid']['rank']}")
