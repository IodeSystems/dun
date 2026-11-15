// A tool call as words, the TUI's words: the call line and the result's
// one-line preview. Each function names the Go function it mirrors in
// internal/tui; keep them in step so the phone and the terminal read alike.

import { thinkDur } from './thinking';
import { exitMarker, type ToolCall } from './toolcalls';

type Args = Record<string, unknown>;

const oneLine = (s: string) => s.replace(/\s+/g, ' ').trim();
const clip = (s: string, n: number) => (s.length <= n ? s : s.slice(0, n) + '…'); // textfmt.Clip
const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'}`;
const argStr = (a: Args | null, k: string) => (typeof a?.[k] === 'string' ? String(a[k]).trim() : '');
const show = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v));

/** The result's non-blank lines (renderers.go meaningfulLines). */
export function lines(s: string): string[] {
  return s
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean);
}
const firstLine = (s: string) => lines(s)[0] ?? '';

/** "notes__project_notes" → "notes · project notes" (tui_commands.go toolLabel). */
export function toolLabel(tool: string): string {
  const words = (s: string) => s.split('_').filter(Boolean).join(' ');
  const at = tool.indexOf('__');
  if (at > 0 && at + 2 < tool.length) return tool.slice(0, at) + ' · ' + words(tool.slice(at + 2));
  return words(tool) || tool;
}

/** `k=v, …`, each value clipped (tui_commands.go argPreview). */
export function argPreview(args: Args | null, max: number): string {
  if (!args) return '';
  const parts = Object.keys(args)
    .sort()
    .map((k) => k + '=' + clip(oneLine(show(args[k])), 48));
  return clip(parts.join(', '), max);
}

/** A file_read with a search is a grep (filerender.go readGrep). */
export const readGrep = (a: Args | null) => argStr(a, 'grep') || argStr(a, 'pattern');

/** A file tool's call as a verb phrase (filerender.go fileCallText). */
function fileCallText(tool: string, a: Args | null): string | null {
  if (!tool.startsWith('file_')) return null;
  let verb = tool.slice(5);
  let path = argStr(a, 'path');
  if (verb === 'read' && readGrep(a)) {
    verb = 'grep';
    if (path === '.') path = '';
    a = { pattern: readGrep(a), path, glob: argStr(a, 'glob') };
  }
  switch (verb) {
    case 'read':
    case 'edit':
    case 'write':
    case 'delete':
      return path ? `${verb} ${path}` : null;
    case 'move':
      return `move ${path} → ${argStr(a, 'to')}`;
    case 'grep': {
      const pat = argStr(a, 'pattern');
      if (!pat) return null;
      let s = `grep /${oneLine(pat)}/`;
      if (path) s += ' in ' + path;
      if (argStr(a, 'glob')) s += ' ' + argStr(a, 'glob');
      return s;
    }
    case 'list': {
      let s = 'list';
      if (path) s += ' ' + path;
      if (argStr(a, 'glob')) s += ' ' + argStr(a, 'glob');
      return s;
    }
  }
  return null;
}

/** The one-line call (tui_commands.go callText): `$ cmd` for exec, a verb for file tools. */
export function callText(tool: string, args: Args | null, max = 80): string {
  const cmd = argStr(args, 'command');
  if (tool === 'exec' && cmd) {
    const rest = { ...args };
    delete rest.command;
    const p = argPreview(rest, 40);
    return clip(oneLine('$ ' + cmd + (p ? `  (${p})` : '')), max);
  }
  const f = fileCallText(tool, args);
  if (f) return clip(f, max);
  const p = argPreview(args, max);
  return clip(toolLabel(tool) + (p ? '  ' + p : ''), max);
}

/** The last informative line before a failure (renderers.go execTail). */
export function execTail(result: string): string {
  let ls = lines(result);
  if (ls.length && ls[ls.length - 1]!.startsWith('[exit:')) ls = ls.slice(0, -1);
  if (!ls.length) return '(no output)';
  const informative = (l: string) =>
    !['FAIL', 'FAILED', 'ERROR', 'ERRORS', 'OK', 'PASS'].includes(l.replace(/[.!]+$/, '').toUpperCase()) &&
    !l.startsWith('exit status ') &&
    (l.includes(':') || l.length > 20);
  for (let i = ls.length - 1; i >= 0 && i > ls.length - 6; i--) if (informative(ls[i]!)) return ls[i]!;
  return ls[ls.length - 1]!;
}

/** Up to two lines of a failed call's error (toolrun.go failLines). */
export function failLines(result: string): string[] {
  let ls = lines(result);
  if (ls.length && ls[ls.length - 1]!.startsWith('[exit:')) ls = ls.slice(0, -1);
  return ls.slice(-2);
}

/** A unified diff's +/- line counts (renderers.go diffStat). */
export function diffStat(s: string): { add: number; del: number } {
  let add = 0;
  let del = 0;
  for (const l of s.split('\n')) {
    if (l.startsWith('+') && !l.startsWith('+++')) add++;
    else if (l.startsWith('-') && !l.startsWith('---')) del++;
  }
  return { add, del };
}

/** Whether text looks like a unified diff (render.go isDiff). */
export const isDiff = (s: string) => s.includes('@@ ') || s.includes('diff --git ') || s.trim().startsWith('--- ');

export type Tone = 'ok' | 'err' | 'muted';
export type Preview = { text: string; tone: Tone };

const backgroundJob = (r: string) => /(?:is now background job #|Started background job #)([^:. \n]+)/.exec(r)?.[1];

/** exec's verdict (renderers.go execRender). */
function execPreview(r: string): Preview {
  const job = backgroundJob(r);
  if (job) return { text: `⏱ background job #${job} · jobs(job:${job}) to check on it`, tone: 'muted' };
  const m = exitMarker(r);
  if (m !== null) {
    const head = m.includes('TIMED OUT') ? '⏱ TIMED OUT' : '✗ ' + clip(m, 30);
    return { text: `${head} · ${clip(oneLine(execTail(r)), 70)}`, tone: 'err' };
  }
  const n = lines(r).length;
  if (!n) return { text: '✓ (no output)', tone: 'ok' };
  return { text: `✓ ${plural(n, 'line')} · ${clip(oneLine(firstLine(r)), 70)}`, tone: 'ok' };
}

const fileFailed = (r: string): Preview | null => {
  const t = r.trim();
  if (!t.startsWith('ERROR')) return null;
  return { text: clip(oneLine(firstLine(t.replace(/^ERROR:?/, ''))), 80), tone: 'err' };
};

const reReadHead = /^.*: lines (\d+)-(\d+) of (\d+)$/;
const reReadWhole = /^.*: (\d+) lines?$/;
const reEditHead = /^edited .*: \+(\d+) -(\d+)/;
const reWriteHead = /^wrote .* \((\d+ lines?), (new|overwritten)/;
const reGrepHead = /^(\d+\+?) (match|matches) in (\d+\+?) files?/;
const reGrepFiles = /^(\d+\+?) files? match/;
const reListDir = /^.*\/: (\d+ dirs?), (\d+ files?)/;
const files = (n: string) => (n === '1' ? 'file' : 'files');

/** A read's line range (filerender.go fileReadRender), e.g. ":10-40 (31 lines of 200)". */
export function readRange(r: string): string | null {
  const h = firstLine(r);
  let m = reReadHead.exec(h);
  if (m) return `:${m[1]}-${m[2]} (${plural(Number(m[2]) - Number(m[1]) + 1, 'line')} of ${m[3]})`;
  m = reReadWhole.exec(h);
  if (m) return `(${plural(Number(m[1]), 'line')})`;
  return null;
}

/** A search's hit count (filerender.go fileGrepRender). */
export function grepHits(r: string): string {
  const ls = r.split('\n', 3);
  let h = ls[0] ?? '';
  if (h.startsWith('searched for ') && ls.length > 1) h = ls[1] ?? '';
  let m = reGrepHead.exec(h);
  if (m) return `${m[1]} ${m[2]} in ${m[3]} ${files(m[3]!)}`;
  if (h.startsWith('no matches')) return 'no matches';
  m = reGrepFiles.exec(h);
  if (m) return `${m[1]} ${files(m[1]!)}`;
  return clip(oneLine(h), 80);
}

function jsonSummary(r: string): string | null {
  let v: unknown;
  try {
    v = JSON.parse(r.trim());
  } catch {
    return null;
  }
  if (Array.isArray(v)) return `${v.length} item(s)`;
  if (v && typeof v === 'object') return clip(Object.keys(v).sort().join(', '), 100);
  return clip(oneLine(String(v)), 100);
}

/** Tools whose result is JSON (renderers.go init: jsonRender). */
export const jsonTools = new Set([
  'search',
  'node_query',
  'list_documents',
  'list_indexes',
  'index_status',
  'search_figures',
]);

/** The result's one-line preview: the verdict, as the TUI's renderer for the tool gives it. */
export function preview(c: ToolCall): Preview {
  if (c.state === 'pending' || c.result === undefined) return { text: 'running…', tone: 'muted' };
  const r = c.result;
  const t = c.tool;
  if (t === 'exec') return execPreview(r);
  if (t.startsWith('file_')) {
    const f = fileFailed(r);
    if (f) return f;
    const verb = t.slice(5);
    if (verb === 'grep' || (verb === 'read' && readGrep(c.args))) return { text: grepHits(r), tone: 'muted' };
    if (verb === 'read') {
      const rr = readRange(r);
      if (rr) return { text: rr, tone: 'muted' };
    }
    if (verb === 'edit') {
      const m = reEditHead.exec(firstLine(r));
      if (m) return { text: `+${m[1]} −${m[2]}`, tone: 'muted' };
    }
    if (verb === 'write') {
      const m = reWriteHead.exec(firstLine(r));
      if (m) return { text: `(${m[1]}, ${m[2]})`, tone: 'muted' };
    }
    if (verb === 'read' || verb === 'list') {
      const m = reListDir.exec(firstLine(r));
      if (m) return { text: `${m[1]}, ${m[2]}`, tone: 'muted' };
      const g = reGrepFiles.exec(firstLine(r));
      if (g) return { text: `${g[1]} ${files(g[1]!)}`, tone: 'muted' };
    }
    if (verb === 'delete' || verb === 'move') return { text: '✓', tone: 'ok' };
    return { text: clip(oneLine(firstLine(r)), 80), tone: 'muted' };
  }
  if (c.state === 'error') return { text: clip(oneLine(firstLine(r)), 90), tone: 'err' };
  if (t === 'node_edit') {
    const d = diffStat(r);
    if (d.add || d.del) return { text: `+${d.add} -${d.del}`, tone: 'muted' };
  }
  if (t === 'node_read') {
    const n = lines(r).length;
    return { text: n ? `✓ ${plural(n, 'line')} · ${clip(oneLine(firstLine(r)), 70)}` : '✓ (empty)', tone: 'ok' };
  }
  if (jsonTools.has(t)) {
    const j = jsonSummary(r);
    if (j) return { text: j, tone: 'muted' };
  }
  return { text: clip(oneLine(r), 100), tone: 'muted' };
}

/**
 * The run's header (toolrun.go summary): head is "⚙ N steps — exec ×4, read"
 * (tools by label, most used first, then by name); failed is the " · 1 failed"
 * part, running " · running…" while a call has no result.
 */
export function runSummary(
  calls: ToolCall[],
  thoughts: { start: number; end: number; live: boolean }[] = [],
  now = Date.now(),
): { head: string; thought: string; failed: string; running: string } {
  const counts = new Map<string, number>();
  let bad = 0;
  let pending = 0;
  for (const c of calls) {
    counts.set(c.tool, (counts.get(c.tool) ?? 0) + 1);
    if (c.state === 'error') bad++;
    if (c.state === 'pending') pending++;
  }
  const names = [...counts.keys()].sort((a, b) => counts.get(b)! - counts.get(a)! || (a < b ? -1 : a > b ? 1 : 0));
  const parts = names.map((n) => toolLabel(n) + (counts.get(n)! > 1 ? ` ×${counts.get(n)}` : ''));
  // The TUI counts thoughts as steps and adds the time spent thinking (toolrun.go summary).
  let thought = 0;
  for (const t of thoughts) {
    if (t.live) thought += now - t.start;
    else if (t.end && t.start) thought += t.end - t.start;
  }
  return {
    head: `⚙ ${plural(calls.length + thoughts.length, 'step')} — ${parts.join(', ')}`,
    thought: thought > 0 ? ` · thought ${thinkDur(thought)}` : '',
    failed: bad ? ` · ${bad} failed` : '',
    running: pending ? ' · running…' : '',
  };
}

/** The call's args in full, one `key: value` per line (tui_commands.go argFull). */
export function argFull(args: Args | null): string {
  if (!args) return '';
  return Object.keys(args)
    .sort()
    .map((k) => `${k}: ${show(args[k])}`)
    .join('\n');
}

/** A JSON result pretty-printed (renderers.go jsonRender); null when it is not JSON. */
export function prettyJson(r: string): string | null {
  try {
    return JSON.stringify(JSON.parse(r.trim()), null, 2);
  } catch {
    return null;
  }
}
