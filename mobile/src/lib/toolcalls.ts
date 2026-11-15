// Tool calls and their results, merged: one call is one entry that holds its
// args and, once it arrives, its result. Pure, like events.ts.
//
// The engine sends a call and its result as two events (proto/events.go
// ToolCall, ToolResult). Live events carry no call id; history items
// (proto.HistoryItem) carry call_id. So a result is paired:
//   1. by call_id, when both sides have one;
//   2. else with the newest pending call of the same tool (the TUI's rule,
//      internal/tui/toolrun.go runSetResult: the run's last pending call);
//   3. else it is shown on its own, a call with no args (the TUI does too).
// A call whose id matches an earlier orphan result fills that entry's args.
//
// Consecutive calls fold into one run (internal/tui/toolrun.go): a call joins
// the last transcript item when that item is a run; a finished thought just
// above takes over into the run's first place, as the TUI does (runAddCall).
// Thinking is added to the open run (withThought) and does not end it;
// anything else shown after a run (a reply, a note, a user message) ends it, and the next call starts a
// new one. Pairing looks at every run, not only the last, so a result that
// arrives late still finds its call.

import type { Thought } from './thinking';

export type CallState = 'pending' | 'done' | 'error';

export type ToolCall = {
  /** proto.HistoryItem call_id; absent on live events. */
  id?: string;
  tool: string;
  /** null: a result arrived with no call seen. */
  args: Record<string, unknown> | null;
  result?: string;
  state: CallState;
};

/**
 * A thought inside a run: at is how many calls came before it, so the run
 * shows calls and thoughts in order (toolrun.go runItem). Thinking between
 * calls does not end a run; a reply, a user message or a notice does.
 */
export type RunThought = Thought & { at: number };

export type Run = { key: string; kind: 'tools'; calls: ToolCall[]; thoughts?: RunThought[] };

/** The smallest shape a transcript item needs for these functions. */
type Keyed = { key: string; kind: string };

const lastLine = (s: string) => {
  const lines = s.trimEnd().split('\n');
  return (lines[lines.length - 1] ?? '').trim();
};

/** exec's "[exit: …]" tail, read from the last line only (renderers.go execExitMarker). */
export function exitMarker(result: string): string | null {
  const last = lastLine(result);
  if (!last.startsWith('[exit:')) return null;
  return last
    .replace(/^\[exit:/, '')
    .replace(/\]$/, '')
    .trim();
}

/** Whether a result is a failure (toolrun.go toolFailed). */
export function failed(tool: string, result: string): boolean {
  if (tool === 'exec' && exitMarker(result) !== null) return true;
  const r = result.trim();
  return r.startsWith('ERROR') || r.startsWith('error:');
}

function settle(c: ToolCall, result: string): ToolCall {
  return { ...c, result, state: failed(c.tool, result) ? 'error' : 'done' };
}

/** Indexes of [item, call] for the call a result belongs to, newest first. */
function find(items: Keyed[], pick: (c: ToolCall) => boolean): [number, number] | null {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i];
    if (it?.kind !== 'tools') continue;
    const calls = (it as Run).calls;
    for (let k = calls.length - 1; k >= 0; k--) if (pick(calls[k]!)) return [i, k];
  }
  return null;
}

function replaceCall<T extends Keyed>(items: T[], at: [number, number], c: ToolCall): T[] {
  const [i, k] = at;
  const run = items[i] as unknown as Run;
  const calls = run.calls.slice();
  calls[k] = c;
  const out = items.slice();
  out[i] = { ...run, calls } as unknown as T;
  return out;
}

/**
 * items with c added: into the last item when it is a run, else as a new run
 * keyed newKey. added says whether newKey was used.
 */
function addCall<T extends Keyed>(items: T[], c: ToolCall, newKey: string): { items: T[]; added: boolean } {
  const last = items[items.length - 1];
  if (last?.kind === 'tools') {
    const out = items.slice();
    const run = last as unknown as Run;
    out[out.length - 1] = { ...run, calls: [...run.calls, c] } as unknown as T;
    return { items: out, added: false };
  }
  if (last?.kind === 'thinking') {
    const t = last as unknown as Thought;
    if (!t.live) {
      const { text, start, end, cut } = t;
      const th: RunThought = { text, start, end, live: false, at: 0, ...(cut ? { cut } : {}) };
      const out = items.slice();
      out[out.length - 1] = { key: last.key, kind: 'tools', calls: [c], thoughts: [th] } as unknown as T;
      return { items: out, added: false };
    }
  }
  return { items: [...items, { key: newKey, kind: 'tools', calls: [c] } as unknown as T], added: true };
}

/** A thought into the open run (the last item), after its calls so far; false when no run is open. */
export function withThought<T extends Keyed>(items: T[], t: Thought): T[] | null {
  const last = items[items.length - 1];
  if (last?.kind !== 'tools') return null;
  const run = last as unknown as Run;
  const out = items.slice();
  out[out.length - 1] = {
    ...run,
    thoughts: [...(run.thoughts ?? []), { ...t, at: run.calls.length }],
  } as unknown as T;
  return out;
}

/** A call: into the open run, or a new one; or the args of its orphan result. */
export function withCall<T extends Keyed>(
  items: T[],
  tool: string,
  args: unknown,
  id: string | undefined,
  newKey: string,
): { items: T[]; added: boolean } {
  const a = args && typeof args === 'object' ? (args as Record<string, unknown>) : {};
  if (id) {
    const at = find(items, (c) => c.id === id);
    if (at) {
      const [i, k] = at;
      const c = (items[i] as unknown as Run).calls[k]!;
      return { items: replaceCall(items, at, { ...c, tool: c.tool || tool, args: a }), added: false };
    }
  }
  const c: ToolCall = { tool, args: a, state: 'pending' };
  if (id) c.id = id;
  return addCall(items, c, newKey);
}

/** A result: onto its call (see the pairing rule above), or shown on its own. */
export function withResult<T extends Keyed>(
  items: T[],
  tool: string,
  result: string,
  id: string | undefined,
  newKey: string,
): { items: T[]; added: boolean } {
  const at =
    (id ? find(items, (c) => c.id === id && c.state === 'pending') : null) ??
    find(items, (c) => c.state === 'pending' && c.tool === tool && (!id || !c.id));
  if (at) {
    const [i, k] = at;
    return { items: replaceCall(items, at, settle((items[i] as unknown as Run).calls[k]!, result)), added: false };
  }
  const c: ToolCall = settle({ tool, args: null, state: 'pending' }, result);
  if (id) c.id = id;
  return addCall(items, c, newKey);
}
