// Where the turn is — waiting on the person, retrying, working, or idle — and
// the header's status line for it, in the TUI's words (internal/tui/tui_view.go,
// the status switch). Read from the session's turn_state event (View.turn;
// dun cmd/dun/turnstate.go), not derived from other events. An event log with
// no turn_state (from before it) reads as idle.

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
  /** turn_state's notes_waiting: news held while idle (dun harness autoturn.go). */
  notes: number;
  /** turn_state's net_wait: waiting for the network (dun harness netwait.go). */
  net?: { since: number; next: number };
};

export function turnState(v: View): TurnState {
  const t = v.turn;
  const base = {
    since: t?.since ?? 0,
    tool: v.bg.activeTool,
    interrupting: false,
    retry: t?.retry ?? '',
    notes: t?.notes ?? 0,
    net: t?.net,
  };
  switch (t?.phase) {
    case 'waiting_answer':
      return { ...base, state: 'ask' };
    case 'retrying':
      return { ...base, state: 'retry' };
    case 'working':
      return { ...base, state: 'working' };
    case 'interrupting':
      return { ...base, state: 'working', interrupting: true };
  }
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
  if (t.net) return netWaitText(t.net, now);
  if (t.notes === 1) return 'idle: 1 note waiting';
  if (t.notes > 1) return `idle: ${t.notes} notes waiting`;
  return 'ready';
}

/** 'waiting for network · since 14:02 · next try 30s' (the TUI's netWaitWord). */
export function netWaitText(w: { since: number; next: number }, now: number): string {
  const d = new Date(w.since);
  const hm = String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
  const left = Math.round((w.next - now) / 1000);
  // elapsed(now - left, now) is ' · 30s', Go's Duration form, as the TUI prints it.
  const tail = left > 0 ? ' · next try ' + elapsed(now - left * 1000, now).slice(3) : ' · trying now';
  return `waiting for network · since ${hm}` + tail;
}

export function headerStatus(v: View, now: number): { state: State; text: string } {
  const t = turnState(v);
  return { state: t.state, text: statusText(t, now) };
}
