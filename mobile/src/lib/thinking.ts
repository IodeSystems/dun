// Thinking: the model's reasoning, one line by default — the TUI's
// (internal/tui/thinking.go), ported.
//
// One block per round. `reasoning` events are deltas: the first of a round
// opens a block, live, and every later one grows it. The round's first reply
// token, its first tool call, the end of the turn (or an error, a question,
// a retry) closes it. Folded, the block is one line: the headline
// (thinkHeadline) and how long and how much (thoughtMeta). Open, it is the
// header (thinkHeader) and the full text.

/** A thought: its text, and when it started and ended (ms; 0 = unknown, as from history). */
export type Thought = { text: string; start: number; end: number; live: boolean; cut?: boolean };

/** The events that end the round's thinking (tui_events.go: token, tool_call, ask, done, error, retry). */
export const endsThinking = new Set(['token', 'tool_call', 'message', 'ask', 'done', 'error', 'retry']);

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
 * The line a folded thought shows, from its text (thinkHeadline). A line
 * `# Thinking: <what>` is a step's heading and `# Summary: <what>` the
 * conclusion, matched without regard to case, leading spaces or extra #s.
 * Live, it is the latest Thinking heading; finished, the last Summary. A
 * model that ignores the instruction gets the last Thinking, else its first
 * non-blank line. A line still arriving counts as it stands. "" for a
 * thought with no text.
 */
export function thinkHeadline(text: string, live: boolean): string {
  let thinking = '';
  let summary = '';
  let first = '';
  for (const raw of text.split('\n')) {
    const ln = raw.trim();
    if (!ln) continue;
    if (!first) first = ln;
    if (!ln.startsWith('#')) continue;
    const h = ln.slice(1).replace(/^#+/, '').trim();
    if (h.length >= 8 && h.slice(0, 8).toLowerCase() === 'thinking') {
      const v = headingValue(h.slice(8));
      if (v) thinking = v;
    } else if (h.length >= 7 && h.slice(0, 7).toLowerCase() === 'summary') {
      const v = headingValue(h.slice(7));
      if (v) summary = v;
    }
  }
  if (!live && summary) return summary;
  if (thinking) return thinking;
  return first.replace(/^#+/, '').trim();
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

/** The open block's top line: what it is, how long, how much (thinkHeader). */
export function thinkHeader(t: Thought, now: number): string {
  const w = ` · ${words(t.text)} words`;
  if (t.live) return '✻ thinking… ' + thinkDur(now - t.start) + w;
  if (t.cut && t.end) return '✻ interrupted after ' + thinkDur(t.end - t.start) + w;
  if (t.end) return '✻ thought for ' + thinkDur(t.end - t.start) + w;
  return '✻ thought' + w;
}

/** The index of the thought still streaming in items, or -1 (liveThought). */
export function liveThought<T extends { kind: string }>(items: T[]): number {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i] as T & Partial<Thought>;
    if (it.kind === 'thinking' && it.live) return i;
  }
  return -1;
}

/** items with the live thought closed at time at (endThinking); the same array when none is live. */
export function endThought<T extends { kind: string }>(items: T[], at: number, cut = false): T[] {
  const i = liveThought(items);
  if (i < 0) return items;
  const out = [...items];
  out[i] = { ...out[i], live: false, end: at, ...(cut ? { cut: true } : {}) } as unknown as T;
  return out;
}
