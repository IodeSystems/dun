// What the session header shows beside the conversation: the background work
// (agents, jobs), the plan, the context figures, and what the running turn is
// doing. Folded from the events by noteBackground (events.ts apply calls it
// for every event).

import { parseAgents, parseJobs, type Agent, type Job } from './activity';
import { noCtx, noteCtx, type Ctx } from './ctxstats';
import { noCtxView, noteCtxView, type CtxView } from './ctxview';
import { parsePlan, type Plan } from './plan';
import { evTime } from './thinking';
import { callText } from './tooltext';

export type Background = {
  agents: Agent[];
  jobs: Job[];
  /** Null until the session has a plan with a row in it. */
  plan: Plan | null;
  ctx: Ctx;
  /** The /context view's own figures (ctxview.ts). */
  ctxv: CtxView;
  /** The call the turn is running, as the TUI says it (activeTool: callText, 40 wide); '' between calls. */
  activeTool: string;
  /** When the running turn started (ms), 0 when idle: the header's clock (turn_state since). */
  busySince: number;
  /** The provider wait the turn is in (turn_state retry); '' when none. */
  retry: string;
};

export const noBackground: Background = {
  agents: [],
  jobs: [],
  plan: null,
  ctx: noCtx,
  ctxv: noCtxView,
  activeTool: '',
  busySince: 0,
  retry: '',
};

type Ev = Record<string, unknown>;
const str = (v: unknown) => (typeof v === 'string' ? v : '');
const args = (v: unknown) => (v && typeof v === 'object' ? (v as Record<string, unknown>) : null);

export function noteBackground(b: Background, ev: Ev): Background {
  const n = noteWork(b, ev);
  const ctxv = noteCtxView(n.ctxv, ev);
  return ctxv === n.ctxv ? n : { ...n, ctxv };
}

function noteWork(b: Background, ev: Ev): Background {
  switch (ev.type) {
    case 'agents':
      return { ...b, agents: parseAgents(ev, evTime(ev)) };
    case 'jobs':
      return { ...b, jobs: parseJobs(ev) };
    case 'plan': {
      const p = parsePlan(ev);
      return { ...b, plan: p.rows.length || p.mode ? p : null };
    }
    case 'usage':
    case 'context_cost':
    case 'spend':
      return { ...b, ctx: noteCtx(b.ctx, ev) };
    case 'turn_state': {
      // Where the turn is (dun cmd/dun/turnstate.go): its clock and its wait.
      const idle = ev.phase === 'idle';
      const r = (ev.retry ?? null) as Record<string, unknown> | null;
      return {
        ...b,
        busySince: idle ? 0 : Number(ev.since ?? 0) || b.busySince || evTime(ev),
        retry: ev.phase === 'retrying' && r ? str(r.text) || 'retrying…' : '',
        activeTool: idle ? '' : b.activeTool,
      };
    }
    // The call running now: from its start (tool_call) to its end (tool_result).
    case 'tool_call':
      return { ...b, activeTool: callText(str(ev.tool), args(ev.args), 40) };
    case 'tool_result':
      return { ...b, activeTool: '' };
  }
  return b;
}
