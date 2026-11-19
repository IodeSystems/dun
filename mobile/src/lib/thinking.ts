// Thinking: the model's reasoning, one line by default — the TUI's
// (internal/tui/thinking.go), ported.
//
// One block per round. `reasoning` events are deltas, each naming its model
// round (`round`): the first of a round opens a block, live, and every later
// one grows it. The engine says when the round's thinking is over
// (`reasoning_end {round, cut?}`, docs/session-events.md "Thinking rounds");
// a reasoning delta of another round opens a new block. Folded, the block is one line: the headline
// (thinkHeadline) and how long and how much (thoughtMeta). Open, it is the
// header (thinkHeader) and the full text, as plain text (the TUI renders it
// as Markdown; Markdown.tsx draws in the reply's colour and size only).

/** A thought: its text, and when it started and ended (ms; 0 = unknown, as from history). */
export type Thought = { text: string; start: number; end: number; live: boolean; cut?: boolean; round?: number };

/**
 * Fallback, an event log from before round ids (no `round`, no
 * reasoning_end): the events that ended a round's thinking then, as the
 * clients guessed it. Used only until an event names its round.
 */
export const endsThinkingOld = new Set(['token', 'tool_call', 'message', 'ask', 'done', 'error', 'retry']);

/** An event's time in ms: its ts, else now. */
export function evTime(ev: Record<string, unknown>): number {
  const t = typeof ev.ts === 'string' ? Date.parse(ev.ts) : NaN;
  return Number.isNaN(t) ? Date.now() : t;
}

/** Collapses runs of whitespace to one space (textfmt.OneLine). */
const oneLine = (s: string) => s.split(/\s+/).filter(Boolean).join(' ');

/** What follows a heading's keyword: ": text", else "" (headingValue). */
function headingValue(rest: string): string {
  rest = rest.trim();
  if (!rest.startsWith(':')) return '';
  return oneLine(rest.slice(1)).trim();
}

/**
 * The line a folded thought shows, from its text (thinkHeadline): its title
 * (thinkTitle), else its first non-blank line. "" for a thought with no text.
 */
export function thinkHeadline(text: string, live: boolean): string {
  const title = thinkTitle(text, live);
  if (title) return title;
  const first = text.split('\n').find((ln) => ln.trim());
  return first ? first.trim().replace(/^#+/, '').trim() : '';
}

/**
 * What the thought says it is doing, "" when it says nothing (thinkTitle).
 * dun's prompt asks the model to open each step with a bold title line,
 * `**<what>**`. A title is a line that is only a bold span, `**…**` or
 * `__…__`, a colon after it or inside it allowed; bold inside a sentence is
 * not one. Older logs have `# Thinking: <what>` step headings and a
 * `# Summary: <what>`, matched without regard to case, leading spaces or
 * extra #s. Live, it is the latest title; finished, the last Summary, else
 * the latest title. A line still arriving counts as it stands: a bold span
 * counts once it is closed.
 */
export function thinkTitle(text: string, live: boolean): string {
  let title = '';
  let summary = '';
  for (const raw of text.split('\n')) {
    const ln = raw.trim();
    const bold = boldTitle(ln);
    if (bold) {
      title = bold;
      continue;
    }
    if (!ln.startsWith('#')) continue;
    const h = ln.slice(1).replace(/^#+/, '').trim();
    if (h.length >= 8 && h.slice(0, 8).toLowerCase() === 'thinking') {
      const v = headingValue(h.slice(8));
      if (v) title = v;
    } else if (h.length >= 7 && h.slice(0, 7).toLowerCase() === 'summary') {
      const v = headingValue(h.slice(7));
      if (v) summary = v;
    }
  }
  if (!live && summary) return summary;
  return title;
}

/** The text of a line that is only a bold span — `**what**`, `__what__`, `**what**:`, `**what:**` — else "" (boldTitle). */
export function boldTitle(ln: string): string {
  if (ln.endsWith(':')) ln = ln.slice(0, -1);
  for (const m of ['**', '__']) {
    if (!ln.startsWith(m)) continue;
    let inner = ln.slice(m.length);
    if (!inner.endsWith(m)) return '';
    inner = inner.slice(0, -m.length);
    if (inner.includes(m)) return '';
    inner = inner.trim();
    if (inner.endsWith(':')) inner = inner.slice(0, -1);
    return oneLine(inner).trim();
  }
  return '';
}

/** A duration the short way: <1s, 12s, 2m05s (thinkDur). */
export function thinkDur(ms: number): string {
  if (ms < 1000) return '<1s';
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m${String(s % 60).padStart(2, '0')}s`;
}

const words = (t: string) => t.split(/\s+/).filter(Boolean).length;

/** How long it took, "–" when not known (thoughtTime). */
function thoughtTime(t: Thought, now: number): string {
  if (t.live) return thinkDur(now - t.start);
  if (!t.end || !t.start) return '–';
  return thinkDur(t.end - t.start);
}

/** The folded line's headline, or what the thought is when it has none (thoughtLine). */
export function thoughtHead(t: Thought): string {
  const head = thinkHeadline(t.text, t.live);
  if (head) return head;
  if (t.live) return 'thinking';
  return t.cut ? 'interrupted' : 'thought';
}

/** The folded line's meta: "(12s, 340 words)"; narrow gives "340w" (thoughtLine's meta). */
export function thoughtMeta(t: Thought, now: number, narrow = false): string {
  const parts: string[] = [];
  let d = thoughtTime(t, now);
  if (d !== '–' || !t.live) {
    if (t.live) d += '…';
    parts.push(d);
  }
  const n = words(t.text);
  if (n > 0) parts.push(narrow ? `${n}w` : `${n} words`);
  let s = parts.join(', ');
  if (t.cut) s = '⏹ ' + s;
  return '(' + s.trim() + ')';
}

/** The open block's top line: what it is (its title, if it has one), how long, how much (thinkHeader). */
export function thinkHeader(t: Thought, now: number): string {
  const w = ` · ${words(t.text)} words`;
  const title = thinkTitle(t.text, t.live);
  if (title) {
    let s = '✻ ' + title;
    let d = thoughtTime(t, now);
    if (d !== '–') {
      if (t.live) d += '…';
      else if (t.cut) d = '⏹ ' + d;
      s += ' · ' + d;
    }
    return s + w;
  }
  if (t.live) return '✻ thinking… ' + thinkDur(now - t.start) + w;
  if (t.cut && t.end) return '✻ interrupted after ' + thinkDur(t.end - t.start) + w;
  if (t.end) return '✻ thought for ' + thinkDur(t.end - t.start) + w;
  return '✻ thought' + w;
}

type Holder = { kind: string; thoughts?: (Thought & { at: number })[] };

/** The index of the thought still streaming in items (also the last thought of a tool run), or -1 (liveThought). */
export function liveThought<T extends { kind: string }>(items: T[]): number {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i] as T & Partial<Thought> & Holder;
    if (it.kind === 'thinking' && it.live) return i;
    if (it.kind === 'tools' && it.thoughts?.at(-1)?.live) return i;
  }
  return -1;
}

/** items with the live thought closed at time at (endThinking); the same array when none is live. */
export function endThought<T extends { kind: string }>(items: T[], at: number, cut = false): T[] {
  const i = liveThought(items);
  if (i < 0) return items;
  const out = [...items];
  const it = out[i] as T & Holder;
  const close = { live: false, end: at, ...(cut ? { cut: true } : {}) };
  if (it.kind === 'tools') {
    const th = (it.thoughts ?? []).slice();
    th[th.length - 1] = { ...(th[th.length - 1] as Thought & { at: number }), ...close };
    out[i] = { ...it, thoughts: th };
  } else out[i] = { ...it, ...close };
  return out;
}
