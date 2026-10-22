// A session's events, folded into what a screen shows. Pure: the screen
// feeds it what /api/graphql sessionEvents returns, in order.
//
// The events are the protocol's flat JSON frames (dun proto/events.go); only
// the fields read here are relied on.

export type Item =
  | { key: string; kind: 'user'; text: string; queued?: boolean }
  | { key: string; kind: 'assistant'; text: string }
  | { key: string; kind: 'tool'; title: string; body: string }
  | { key: string; kind: 'note'; text: string; error?: boolean };

export type Ask = { question: string; options: string[] };

export type View = {
  items: Item[];
  /** The reply streaming in, before its message event settles it. */
  streaming: string | null;
  busy: boolean;
  status: string;
  ask: Ask | null;
  n: number; // key counter
};

export const empty: View = { items: [], streaming: null, busy: false, status: '', ask: null, n: 0 };

type Ev = Record<string, unknown>;
const str = (v: unknown) => (typeof v === 'string' ? v : '');

/** An item before it has a key (Omit spread over each member of the union). */
type WithoutKey<T> = T extends unknown ? Omit<T, 'key'> : never;
type NewItem = WithoutKey<Item>;

function push(v: View, item: NewItem): View {
  return { ...v, n: v.n + 1, items: [...v.items, { ...item, key: String(v.n) } as Item] };
}

/** A history item (history event, snapshot) as a shown item. */
function fromHistory(v: View, it: Ev): View {
  switch (it.kind) {
    case 'user':
      return push(v, { kind: 'user', text: str(it.content) });
    case 'assistant':
      return push(v, { kind: 'assistant', text: str(it.content) });
    case 'tool_call':
      return push(v, { kind: 'tool', title: '⚙ ' + str(it.tool), body: JSON.stringify(it.args ?? {}, null, 2) });
    case 'tool_result':
      return push(v, { kind: 'tool', title: '↳ ' + str(it.tool), body: str(it.content) });
    case 'notification':
      return push(v, { kind: 'note', text: '🔔 ' + str(it.content) });
  }
  return v;
}

/** Replace what is shown with a conversation (history replaces, never adds). */
export function replaceWith(items: unknown[]): View {
  let v: View = { ...empty };
  for (const it of items) if (it && typeof it === 'object') v = fromHistory(v, it as Ev);
  return v;
}

export function apply(v: View, ev: Ev): View {
  switch (ev.type) {
    case 'snapshot':
      return applySnapshot(ev);
    case 'history':
      return { ...replaceWith((ev.items as unknown[]) ?? []), busy: v.busy, status: v.status, ask: v.ask };
    case 'user':
      return push(v, { kind: 'user', text: str(ev.content), queued: ev.queued === true });
    case 'turn':
      return { ...v, busy: true, status: 'working…' };
    case 'token':
      return { ...v, busy: true, streaming: (v.streaming ?? '') + str(ev.text) };
    case 'message': {
      const text = str(ev.content) || v.streaming || '';
      const w = { ...v, streaming: null };
      return text ? push(w, { kind: 'assistant', text }) : w;
    }
    case 'tool_call': {
      // A tool call ends the text streamed before it.
      let w = v;
      if (v.streaming) w = push({ ...v, streaming: null }, { kind: 'assistant', text: v.streaming });
      return push(w, { kind: 'tool', title: '⚙ ' + str(ev.tool), body: JSON.stringify(ev.args ?? {}, null, 2) });
    }
    case 'tool_result':
      return push(v, { kind: 'tool', title: '↳ ' + str(ev.tool), body: str(ev.result) });
    case 'done':
      return { ...v, busy: false, status: '', streaming: null };
    case 'error':
      return push({ ...v, busy: false, status: '', streaming: null }, { kind: 'note', text: 'error: ' + str(ev.error), error: true });
    case 'ask':
      return { ...v, ask: { question: str(ev.question), options: (ev.options as string[]) ?? [] } };
    case 'answered':
      return push({ ...v, ask: null }, { kind: 'user', text: str(ev.value) });
    case 'notification':
      if (ev.kind === 'docs') return push(v, { kind: 'note', text: `🔎 ${Number(ev.surfaced ?? 0)} doc(s) surfaced` });
      return ev.text ? push(v, { kind: 'note', text: '🔔 ' + str(ev.text) }) : v;
    case 'control':
    case 'server':
      return ev.message ? push(v, { kind: 'note', text: str(ev.message) }) : v;
    case 'engine':
      if (ev.state === 'started') return v;
      return push({ ...v, busy: false, ask: ev.state === 'restarting' ? null : v.ask },
        { kind: 'note', text: `engine ${str(ev.state)}${ev.reason ? ': ' + str(ev.reason) : ''}`, error: ev.state === 'gave_up' });
    case 'retry':
      return { ...v, status: str(ev.text) || 'retrying…' };
    case 'queued':
      return { ...v, status: `${Number(ev.count ?? 0)} message(s) queued for the running turn` };
  }
  return v;
}

/** A snapshot: the conversation, then the latest state events. */
export function applySnapshot(snap: Ev): View {
  let v = replaceWith((snap.items as unknown[]) ?? []);
  for (const s of (snap.state as Ev[]) ?? []) if (s.type !== 'history') v = apply(v, s);
  return v;
}
