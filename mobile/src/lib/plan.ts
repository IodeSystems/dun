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
};

export type Plan = { mode: boolean; approved: boolean; total: number; done: number; rows: PlanRow[] };

const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
const str = (v: unknown) => (typeof v === 'string' ? v : '');
const strs = (v: unknown) => (Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []);

export function parsePlan(ev: Record<string, unknown>): Plan {
  const rows = (Array.isArray(ev.rows) ? ev.rows : []) as Record<string, unknown>[];
  return {
    mode: ev.mode === true,
    approved: ev.approved === true,
    total: num(ev.total),
    done: num(ev.done),
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
    })),
  };
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
