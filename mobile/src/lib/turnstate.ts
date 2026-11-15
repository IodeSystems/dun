// Where the turn is — waiting on the person, retrying, working, or idle — and
// the header's status line for it, in the TUI's words (internal/tui/tui_view.go,
// the status switch). The one place the client derives it: from the turn,
// tool, retry, ask and done events today (View.busy, View.ask, View.bg), and
// from a single state event once dund sends one.

import type { View } from './events';

export type State = 'ask' | 'retry' | 'working' | 'idle';

export type TurnState = {
  state: State;
  /** When the turn started (ms), 0 when unknown or idle. */
  since: number;
  /** The tool running now, '' between calls. */
  tool: string;
  interrupting: boolean;
  retry: string;
};

export function turnState(v: View): TurnState {
  const base = { since: v.bg.busySince, tool: v.bg.activeTool, interrupting: false, retry: v.bg.retry };
  if (v.ask) return { ...base, state: 'ask' };
  if (v.bg.retry) return { ...base, state: 'retry' };
  if (v.busy) return { ...base, state: 'working', interrupting: v.status === 'interrupting…' };
  return { ...base, state: 'idle', since: 0, tool: '' };
}

/** ' · 3s', ' · 2m5s' (busyElapsed: Go's Duration rounded to the second). */
export function elapsed(since: number, now: number): string {
  if (!since) return '';
  let s = Math.max(0, Math.round((now - since) / 1000));
  const h = Math.trunc(s / 3600);
  s -= h * 3600;
  const m = Math.trunc(s / 60);
  s -= m * 60;
  return ' · ' + (h ? `${h}h${m}m${s}s` : m ? `${m}m${s}s` : `${s}s`);
}

/** The header's status line: a question first, then a provider wait, then work, else ready. */
export function statusText(t: TurnState, now: number): string {
  switch (t.state) {
    case 'ask':
      return '❓ a question is waiting';
    case 'retry':
      return '⏳ ' + t.retry;
    case 'working':
      if (t.interrupting) return 'interrupting…' + elapsed(t.since, now);
      return 'working…' + (t.tool ? ' ' + t.tool : '') + elapsed(t.since, now);
  }
  return 'ready';
}

export function headerStatus(v: View, now: number): { state: State; text: string } {
  const t = turnState(v);
  return { state: t.state, text: statusText(t, now) };
}
