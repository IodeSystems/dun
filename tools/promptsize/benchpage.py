#!/usr/bin/env python3
"""Render the benchmarks as pages: what was run, how, with what prompt, and
every run's result, followed by the scripts themselves.

Each benchmark below names its scripts (shown whole, so the page is the
method) and its result files in docs/measured (every row shown, so a range
on the home page can be checked against the runs behind it). make site runs
this into dist/site/bench/.

    benchpage.py OUTDIR
"""
import html
import json
import os
import statistics
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
MEASURED = os.path.join(ROOT, "docs", "measured")
REPO = "https://github.com/IodeSystems/dun/blob/main/"

TASK = subprocess.run([sys.executable, os.path.join(HERE, "fixture.py"), "--task"],
                      capture_output=True, text=True, check=True).stdout.strip()

# Which rows of which file, under what label. A label carries the agent, its
# version and the setup, because a number without them cannot be checked.
FIRST = [
    ("first-turn-v0.1.8-2026-10-27.json", {
        "dun-none": "dun v0.1.8 · no MCP servers",
        "dun-direct": "dun v0.1.8 · Playwright + poly-lsp, direct",
        "dun-scripted": "dun v0.1.8 · Playwright scripted, poly-lsp direct"}),
    ("pi-1.4.0-2026-10-27.json", {
        "pi-none": "pi 1.4.0 · no MCP servers",
        "pi-codemode": "pi 1.4.0 · Playwright + poly-lsp, codemode (its default)",
        "pi-direct": "pi 1.4.0 · Playwright + poly-lsp, direct"}),
    ("first-turn-2026-10-27.json", {
        "claude": "Claude Code 2.1.311 · no MCP servers",
        "opencode": "opencode 1.15.3 · no MCP servers"}),
    ("mcp-2026-10-27.json", {
        "claude": "Claude Code 2.1.311 · Playwright + poly-lsp (tool search)",
        "opencode": "opencode 1.15.3 · Playwright + poly-lsp"}),
]

TASK_NOW = [
    ("session-short-v0.1.8-2026-10-27.json", {"dun": "dun v0.1.8"}),
    ("session-short-2026-10-27.json", {"pi": "pi 1.4.0", "claude": "Claude Code 2.1.311", "opencode": "opencode 1.15.3"}),
]
TASK_BEFORE = [
    ("session-short-2026-10-27.json", {"dun": "dun v0.1.5 + skill reader"}),
    ("session-short-dun-file_read-2026-10-27.json", {"dun": "dun 137bd56 (file_read took grep)"}),
    ("session-short-recap-split-2026-10-27.json", {"dun": "dun, first recap-split build (description later changed)"}),
]

BENCHES = [
    {
        "name": "first-request",
        "title": "Tokens before you type",
        "lede": "What each agent sends on its first request, before the person has asked for anything: "
                "system prompt and tool definitions. It is paid again on every request of the session.",
        "setup": [
            "Each agent installed clean, with a clean home: no rules files, skills or extensions of the person's own.",
            "Pointed at <code>capture.py</code>, a stand-in model server that saves every request and answers \"ok\".",
            "Run from an empty folder. The prompt is one word.",
            "The largest saved request that carries tools is the first real turn (a title request carries none).",
            "<code>measure.py</code> sends that request to one model with <code>max_tokens=1</code> and reads the "
            "server's prompt token count: Qwen 27B on llama.cpp, its chat template included, the same for every agent.",
            "MCP rows: Playwright MCP 0.0.83 and poly-lsp-mcp, added to each agent the same way.",
            "A request is deterministic for a given build, so each row is one measurement, not a run of several.",
        ],
        "prompt": "hi",
        "run": "tools/promptsize/capture.py OUT/<agent> <port>   # one per agent\n"
               "# point the agent at http://127.0.0.1:<port>, send \"hi\"\n"
               "KEY=… tools/promptsize/measure.py <url> <model> OUT/*",
        "scripts": ["capture.py", "measure.py"],
        "kind": "first",
        "notes": [
            "Claude Code speaks Anthropic's API; its request is converted to the OpenAI form for counting, each "
            "tool's JSON schema unchanged. It sends MCP tools as names and loads a schema when the model searches.",
            "pi 1.4.0 keeps MCP schemas out by default (codemode), like dun's scripted mode.",
            "dun's poly-lsp cannot be scripted (its file guard needs it direct), so it stays direct in both dun MCP rows.",
            "Not measured: Codex, Gemini CLI, aider.",
        ],
    },
    {
        "name": "whole-task",
        "title": "A whole task",
        "lede": "Every token the model processed, first request to last, for one real task done start to finish.",
        "setup": [
            "<code>fixture.py</code> makes a fresh repository per run: a Python project whose CI failed. "
            "<code>ci.log</code> is 218 KB, about 3,000 lines of build noise with the real error buried in it; "
            "the bug is an off-by-one in <code>inventory.py</code>.",
            "Each agent runs one-shot on it, in a clean home, through <code>relay.py</code>: a relay to the model "
            "server that records every request and the usage the server reports.",
            "The same model for every agent: Qwen 27B through llama.cpp, behind one gateway. Claude Code goes "
            "through the gateway's Anthropic endpoint; its prompt is input + cache reads + cache writes.",
            "Done means <code>python3 -m unittest</code> passes afterwards (<code>check.sh</code> in the fixture).",
        ],
        "prompt": TASK,
        "run": "KEY=… tools/promptsize/session.py OUT <url> <model> <runs> dun=PATH pi=PATH opencode claude",
        "scripts": ["session.py", "fixture.py", "relay.py"],
        "kind": "task",
        "notes": [
            "A short task: no agent reached ten requests, so how a long session grows is not measured here.",
            "\"Cached\" is what the server's prompt cache served; it depends on the server reporting it.",
        ],
    },
]


def esc(s):
    return html.escape(str(s))


def n(v):
    return "–" if v is None else f"{v:,}"


def rows_of(spec):
    out = []
    for fname, labels in spec:
        for r in json.load(open(os.path.join(MEASURED, fname))):
            if r.get("agent") in labels:
                out.append((labels[r["agent"]], fname, r))
    return out


def first_table(rows):
    b = ['<table><thead><tr><th>agent · setup</th><th>tokens</th><th>tools</th><th>tool tokens</th></tr></thead><tbody>']
    for label, fname, r in sorted(rows, key=lambda x: x[2]["tokens_total"]):
        b.append(f'<tr><td>{esc(label)}</td><td>{n(r["tokens_total"])}</td><td>{n(r["tools"])}</td>'
                 f'<td>{n(r.get("tokens_tools"))}</td></tr>')
    b.append("</tbody></table>")
    return "".join(b)


def task_table(rows):
    b = ['<table><thead><tr><th>agent</th><th>run</th><th>done</th><th>requests</th><th>first</th>'
         '<th>last</th><th>whole task</th><th>cached</th></tr></thead><tbody>']
    for label, fname, r in rows:
        pct = f'{100 * r["cached"] / r["total"]:.0f}%' if r.get("total") else "–"
        b.append(f'<tr><td>{esc(label)}</td><td>{r["run"]}</td><td>{"yes" if r["done"] else "no"}</td>'
                 f'<td>{r["requests"]}</td><td>{n(r["first"])}</td><td>{n(r["last"])}</td>'
                 f'<td>{n(r["total"])}</td><td>{pct}</td></tr>')
    b.append("</tbody></table>")
    return "".join(b)


def task_summary(rows):
    by = {}
    for label, _, r in rows:
        by.setdefault(label, []).append(r)
    b = ['<table><thead><tr><th>agent</th><th>runs</th><th>done</th><th>requests</th><th>whole task</th>'
         '<th>median</th></tr></thead><tbody>']
    for label, rs in sorted(by.items(), key=lambda kv: statistics.median(r["total"] for r in kv[1])):
        t = sorted(r["total"] for r in rs)
        q = sorted(r["requests"] for r in rs)
        b.append(f'<tr><td>{esc(label)}</td><td>{len(rs)}</td><td>{sum(r["done"] for r in rs)} of {len(rs)}</td>'
                 f'<td>{q[0]}–{q[-1]}</td><td>{n(t[0])}–{n(t[-1])}</td><td>{n(round(statistics.median(t)))}</td></tr>')
    b.append("</tbody></table>")
    return "".join(b)


def files_of(spec):
    seen = []
    for fname, _ in spec:
        if fname not in seen:
            seen.append(fname)
    return ", ".join(f'<a href="{REPO}docs/measured/{f}">{esc(f)}</a>' for f in seen)


CSS = """
:root{--bg:#0d1117;--panel:#161b22;--border:#2a313b;--fg:#e6edf3;--muted:#a3adb9;--dim:#7d8792;--accent:#ff7ab6;
--mono:"JetBrains Mono",ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
--sans:Inter,-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.6 var(--sans)}
a{color:var(--accent);text-decoration:none}a:hover{text-decoration:underline}
.wrap{max-width:960px;margin:0 auto;padding:0 24px 64px}
nav{font:13px/1 var(--mono);padding:16px 0;border-bottom:1px solid var(--border);margin-bottom:32px}
nav a{margin-right:16px}nav b{background:var(--accent);color:#1a0d14;padding:4px 9px;border-radius:3px;margin-right:16px}
h1{font-size:2rem;margin:0 0 8px;letter-spacing:-.02em}h2{font-size:1.2rem;margin:36px 0 12px}
.lede{color:var(--muted);margin:0 0 24px}ul{padding-left:20px;color:var(--muted)}li{margin:4px 0}
code,pre{font-family:var(--mono)}code{font-size:.88em;background:var(--panel);border:1px solid var(--border);border-radius:4px;padding:.05em .35em}
pre{background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:14px 16px;overflow-x:auto;font-size:13px;line-height:1.55}
blockquote{margin:0;padding:12px 16px;border-left:3px solid var(--accent);background:var(--panel);font-family:var(--mono);font-size:14px}
.scroll{overflow-x:auto}table{border-collapse:collapse;width:100%;font:13.5px/1.35 var(--mono)}
th,td{text-align:right;padding:7px 8px;border-bottom:1px solid var(--border);white-space:nowrap}
th:first-child,td:first-child{text-align:left;white-space:normal}th{color:var(--dim);font-weight:500;font-size:12px}
details{margin:10px 0}summary{cursor:pointer;font-family:var(--mono);color:var(--muted)}
.fine{color:var(--dim);font-size:.85rem}
@media (max-width:600px){.wrap{padding:0 16px 48px}th,td{padding:6px 4px;font-size:12px}}
"""


def page(b, others):
    nav = " ".join(f'<a href="{o["name"]}.html">{esc(o["title"].lower())}</a>' for o in others)
    parts = [f'<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">'
             f'<meta name="viewport" content="width=device-width, initial-scale=1">'
             f'<title>dun bench · {esc(b["title"])}</title><link rel="icon" href="../favicon.png">'
             f'<style>{CSS}</style></head><body><div class="wrap">'
             f'<nav><a href="../"><b>dun</b></a>{nav}</nav>'
             f'<h1>{esc(b["title"])}</h1><p class="lede">{esc(b["lede"])}</p>',
             "<h2>Setup</h2><ul>" + "".join(f"<li>{s}</li>" for s in b["setup"]) + "</ul>",
             f'<h2>Prompt</h2><blockquote>{esc(b["prompt"])}</blockquote>',
             f'<h2>Run it</h2><pre>{esc(b["run"])}</pre>']
    if b["kind"] == "first":
        parts.append('<h2>Results</h2><div class="scroll">' + first_table(rows_of(FIRST)) + "</div>")
        parts.append(f'<p class="fine">Data: {files_of(FIRST)}. Measured 2026-10-27.</p>')
    else:
        now = rows_of(TASK_NOW)
        parts.append('<h2>Results</h2><div class="scroll">' + task_summary(now) + "</div>")
        parts.append('<h2>Every run</h2><div class="scroll">' + task_table(now) + "</div>")
        parts.append(f'<p class="fine">Data: {files_of(TASK_NOW)}. Measured 2026-10-27.</p>')
        before = rows_of(TASK_BEFORE)
        parts.append('<details><summary>earlier dun builds, same task</summary><div class="scroll">'
                     + task_table(before) + f'</div><p class="fine">Data: {files_of(TASK_BEFORE)}.</p></details>')
    parts.append("<h2>Notes</h2><ul>" + "".join(f"<li>{s}</li>" for s in b["notes"]) + "</ul>")
    parts.append("<h2>The scripts</h2>")
    for s in b["scripts"]:
        src = open(os.path.join(HERE, s)).read()
        parts.append(f'<details><summary>tools/promptsize/{esc(s)}</summary><pre>{esc(src)}</pre>'
                     f'<p class="fine"><a href="{REPO}tools/promptsize/{s}">on GitHub</a></p></details>')
    parts.append("</div></body></html>\n")
    return "".join(parts)


def main():
    out = sys.argv[1]
    os.makedirs(out, exist_ok=True)
    for b in BENCHES:
        others = [o for o in BENCHES if o is not b]
        with open(os.path.join(out, b["name"] + ".html"), "w") as f:
            f.write(page(b, others))


if __name__ == "__main__":
    main()
