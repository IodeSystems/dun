"""Re-read rate in dun session logs: how often a read-like tool call reads a
path the same session already read. Mirrors aw4's measurement (52% of
file.read/base.read re-read a path). Read-only over ~/.dun/sessions."""
import glob, json, os, re, shlex, sys, collections

ROOT = os.path.expanduser("~/.dun/sessions")
READ_CMDS = {"cat", "head", "tail", "nl", "less", "sed", "bat"}
BLOB = "\x00dun-blob:"


def size(content, blobdir):
    if content.startswith(BLOB):
        p = os.path.join(blobdir, content[len(BLOB):] + ".blob")
        try:
            return os.path.getsize(p)
        except OSError:
            return 0
    return len(content)


def exec_paths(cmd):
    """File paths read by simple read commands in a shell line (best effort)."""
    out = []
    for seg in re.split(r"&&|\|\||;|\n", cmd):
        seg = seg.split("|")[0].strip()
        try:
            toks = shlex.split(seg)
        except ValueError:
            continue
        if not toks or toks[0] not in READ_CMDS:
            continue
        for t in toks[1:]:
            if t.startswith("-") or re.fullmatch(r"[\d,]+p?", t) or t.startswith("'") :
                continue
            if "/" in t or "." in t:
                out.append(t.lstrip("./"))
    return out


def node_path(node):
    # "#dun/tui.go#View" or "dun/tui.go" -> "dun/tui.go"
    n = node.lstrip("#")
    return n.split("#")[0]


tot = collections.Counter()
per_tool = collections.Counter()
per_tool_re = collections.Counter()
per_tool_ex = collections.Counter()
sessions = 0
for d in glob.glob(os.path.join(ROOT, "*")):
    blobdir = os.path.join(d, "blobs")
    groups = collections.defaultdict(list)
    for f in glob.glob(os.path.join(d, "*.jsonl")):
        base = os.path.basename(f).split(".")[0]  # sidecars share the session stem
        groups[base].append(f)
    for stem, files in groups.items():
        entries = []
        for f in files:
            for line in open(f, errors="replace"):
                try:
                    entries.append(json.loads(line))
                except json.JSONDecodeError:
                    pass
        entries.sort(key=lambda e: e.get("CreatedAt", 0))
        calls = {}
        seen = set()
        seen_exact = set()
        any_read = False
        for e in entries:
            k = e.get("Kind")
            if k == "tool_call":
                calls[e.get("ToolCallID")] = e
            elif k == "tool_result":
                c = calls.get(e.get("ToolCallID"))
                if not c:
                    continue
                tool = c.get("ToolName")
                try:
                    args = json.loads(c.get("Content") or "{}")
                except json.JSONDecodeError:
                    args = {}
                paths = []
                if tool == "exec":
                    paths = exec_paths(args.get("command", ""))
                elif tool == "node_read":
                    if args.get("node"):
                        paths = [node_path(args["node"])]
                elif tool == "get_document":
                    paths = [str(args.get("id") or args.get("path") or "")]
                paths = [p for p in paths if p]
                if not paths:
                    continue
                any_read = True
                n = size(e.get("Content") or "", blobdir)
                re_read = any(p in seen for p in paths)
                seen.update(paths)
                exact = tool + "|" + (args.get("node") or args.get("command") or str(args.get("id")))
                if exact in seen_exact:
                    tot["exact"] += 1
                    tot["exact_bytes"] += n
                    per_tool_ex[tool] += 1
                seen_exact.add(exact)
                tot["reads"] += 1
                tot["bytes"] += n
                per_tool[tool] += 1
                if re_read:
                    tot["rereads"] += 1
                    tot["reread_bytes"] += n
                    per_tool_re[tool] += 1
        sessions += any_read

print(f"sessions with reads: {sessions}")
r = tot["reads"]
print(f"read calls: {r}, re-reads of a path already read in the session: {tot['rereads']} ({100*tot['rereads']/max(r,1):.0f}%)")
print(f"read result bytes: {tot['bytes']:,}, of which re-reads: {tot['reread_bytes']:,} ({100*tot['reread_bytes']/max(tot['bytes'],1):.0f}%)")
for t in per_tool:
    print(f"  {t}: {per_tool[t]} reads, {per_tool_re[t]} re-reads ({100*per_tool_re[t]/per_tool[t]:.0f}%)")
print(f"exact same target again: {tot['exact']} ({100*tot['exact']/max(r,1):.0f}%), {tot['exact_bytes']:,} bytes ({100*tot['exact_bytes']/max(tot['bytes'],1):.0f}%)")
for t in per_tool:
    print(f"  {t}: {per_tool_ex[t]} exact repeats ({100*per_tool_ex[t]/per_tool[t]:.0f}%)")
