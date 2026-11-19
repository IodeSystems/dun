// The session's task plan (proto.Plan), as the TUI's /plan screen shows it
// (internal/tui/planview.go): rows in tree order, a mark per status, and the
// motion that says who is working on what.

export type PlanRow = {
  id: string;
  title: string;
  details: string;
  status: string; // todo | doing | done | blocked | dropped
  owner: string;
  why: string;
  waiting: string[];
  ready: boolean;
  depth: number;
  repeat: boolean;
  /** The number of the ask the node serves; 0 for none. */
  ask: number;
};

/**
 * One of the person's asks (proto.PlanAsk): the asks ledger, not a plan node
 * (internal/tui/askfold.go). state: open | blocked | done | filed | dropped;
 * at: unix seconds; working: the one ask the turn works.
 */
export type PlanAsk = {
  n: number;
  title: string;
  state: string;
  why: string;
  at: number;
  working: boolean;
  nodes: string[];
};

/** The person's messages the model has not acknowledged (proto.PlanIntake). */
export type PlanIntake = { messages: { at: number; text: string }[]; working: boolean };

export type Plan = {
  mode: boolean;
  approved: boolean;
  total: number;
  done: number;
  rows: PlanRow[];
  asks?: PlanAsk[];
  intake?: PlanIntake;
};

const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
const str = (v: unknown) => (typeof v === 'string' ? v : '');
const strs = (v: unknown) => (Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []);
const list = (v: unknown) => (Array.isArray(v) ? v : []) as Record<string, unknown>[];

function parseIntake(v: unknown): PlanIntake | undefined {
  if (!v || typeof v !== 'object') return undefined;
  const o = v as Record<string, unknown>;
  const messages = list(o.messages).map((m) => ({ at: num(m.at), text: str(m.text) }));
  return messages.length ? { messages, working: o.working === true } : undefined;
}

export function parsePlan(ev: Record<string, unknown>): Plan {
  const rows = (Array.isArray(ev.rows) ? ev.rows : []) as Record<string, unknown>[];
  return {
    mode: ev.mode === true,
    approved: ev.approved === true,
    total: num(ev.total),
    done: num(ev.done),
    intake: parseIntake(ev.intake),
    asks: list(ev.asks).map((a) => ({
      n: num(a.n),
      title: str(a.title),
      state: str(a.state) || 'open',
      why: str(a.why),
      at: num(a.at),
      working: a.working === true,
      nodes: strs(a.nodes),
    })),
    rows: rows.map((r) => ({
      id: str(r.id),
      title: str(r.title),
      details: str(r.details),
      status: str(r.status) || 'todo',
      owner: str(r.owner),
      why: str(r.why),
      waiting: strs(r.waiting),
      ready: r.ready === true,
      depth: num(r.depth),
      repeat: r.repeat === true,
      ask: num(r.ask),
    })),
  };
}

/** A done, filed or dropped ask: folded into one row. */
export const askClosed = (a: PlanAsk) => a.state !== 'open' && a.state !== 'blocked';

/** How long ago unix second at was, short: now, 5m, 3h, 2d (askAge). */
export function askAge(nowMs: number, at: number): string {
  const m = Math.floor((nowMs / 1000 - at) / 60);
  if (!at || m < 1) return 'now';
  if (m < 60) return `${m}m`;
  if (m < 48 * 60) return `${Math.floor(m / 60)}h`;
  return `${Math.floor(m / 1440)}d`;
}

/** The intake's row: ▸ new · 3 messages · "the tools and stubs dir…" +2. */
export function intakeLine(i: PlanIntake): string {
  const n = i.messages.length;
  const first = (i.messages[0]?.text ?? '').trim().split('\n')[0] ?? '';
  const clip = first.length > 30 ? first.slice(0, 30) + '…' : first;
  return `▸ new · ${n} message${n === 1 ? '' : 's'} · "${clip}"` + (n > 1 ? ` +${n - 1}` : '');
}

/** An open ask's tag: working on the one the turn works while it runs, else open · <age>. */
export function askTag(a: PlanAsk, busy: boolean, nowMs = Date.now()): string {
  if (a.state === 'blocked') return 'blocked: ' + a.why;
  return a.working && busy ? 'working' : `open · ${askAge(nowMs, a.at)}`;
}

/** The fold row's head: ▸ 3 done · 2 filed, with dropped when any. */
export function askFoldLine(asks: PlanAsk[]): string {
  const c = { done: 0, filed: 0, dropped: 0 };
  for (const a of asks) if (a.state in c) c[a.state as keyof typeof c]++;
  return `▸ ${c.done} done · ${c.filed} filed` + (c.dropped ? ` · ${c.dropped} dropped` : '');
}

/** A status in one cell (planMark). */
export function planMark(status: string): string {
  switch (status) {
    case 'doing':
      return '◐';
    case 'done':
      return '✓';
    case 'blocked':
      return '⚠';
    case 'dropped':
      return '✗';
  }
  return '○';
}

/** plan mode and the approval, in words (modeWord). */
export function modeWord(p: Plan): string {
  if (!p.mode) return 'off';
  return p.approved ? 'on · approved' : 'on · waiting for approval';
}

export const spin = ['⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'];

/** The sub-agent a node's owner names ("agent:2"), 0 for anyone else (planAgentOf). */
export function ownerAgent(owner: string): number {
  const m = /^agent:(\d+)$/.exec(owner);
  return m ? Number(m[1]) : 0;
}

export type Live = { busy: boolean; asking: boolean; agents: Record<number, string>; phase: number };

/** How a row moves (motion): tone 'ask' or 'notice' alternate while the main agent asks. */
export type Motion = { mark: string; tone: 'ask' | 'notice' | 'working' | 'agent' | null; tag: string };

/**
 * A working owner's open nodes spin; the main agent's node in progress shows
 * ? while it asks you, its colour alternating; an idle owner's are still.
 * phase counts 100 ms ticks (0 always, with reduced motion).
 */
export function motion(r: PlanRow, live: Live): Motion {
  const mark = planMark(r.status);
  const open = r.status === 'todo' || r.status === 'doing';
  const n = ownerAgent(r.owner);
  if (n) {
    const state = live.agents[n] || 'gone';
    if (state === 'running' && open)
      return { mark: spin[live.phase % spin.length]!, tone: 'agent', tag: `agent ${n} working` };
    return { mark, tone: 'agent', tag: `agent ${n} ${state}` };
  }
  if (r.status !== 'doing') return { mark, tone: null, tag: '' };
  if (live.asking) return { mark: '?', tone: Math.trunc(live.phase / 4) % 2 ? 'notice' : 'ask', tag: 'asking you' };
  if (live.busy) return { mark: spin[live.phase % spin.length]!, tone: 'working', tag: 'working' };
  return { mark, tone: null, tag: '' };
}

/** Whether a row can still be dropped (an open node). */
export const droppable = (r: PlanRow) => r.status === 'todo' || r.status === 'doing' || r.status === 'blocked';
