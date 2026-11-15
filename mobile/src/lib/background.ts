// What the session header shows beside the conversation: the background work
// (agents, jobs), the plan, the context figures, and what the running turn is
// doing. Folded from the events by noteBackground (events.ts apply calls it
// for every event).

import { parseAgents, parseJobs, type Agent, type Job } from './activity';
import { noCtx, noteCtx, type Ctx } from './ctxstats';
import { parsePlan, type Plan } from './plan';
import { evTime } from './thinking';
import { callText } from './tooltext';

export type Background = {
  agents: Agent[];
  jobs: Job[];
  /** Null until the session has a plan with a row in it. */
  plan: Plan | null;
  ctx: Ctx;
  /** The call the turn is running, as the TUI says it (activeTool: callText, 40 wide); '' between calls. */
  activeTool: string;
  /** When the running turn started (ms), 0 when idle: the header's clock. */
  busySince: number;
  /** The provider wait (retry event) the turn is in; '' when none. */
  retry: string;
};

export const noBackground: Background = {
  agents: [],
  jobs: [],
  plan: null,
  ctx: noCtx,
  activeTool: '',
  busySince: 0,
  retry: '',
};

type Ev = Record<string, unknown>;
const str = (v: unknown) => (typeof v === 'string' ? v : '');
const args = (v: unknown) => (v && typeof v === 'object' ? (v as Record<string, unknown>) : null);

export function noteBackground(b: Background, ev: Ev): Background {
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
    case 'turn':
      // A user turn or a continue (background work started it): busy either way.
      return { ...b, busySince: b.busySince || evTime(ev), activeTool: '' };
    case 'tool_call':
      return { ...b, activeTool: callText(str(ev.tool), args(ev.args), 40) };
    case 'tool_result':
      return { ...b, activeTool: '' };
    case 'retry':
      return { ...b, retry: ev.kind === 'recovered' || ev.kind === 'giveup' ? '' : str(ev.text) || 'retrying…' };
    case 'done':
    case 'error':
      return { ...b, busySince: 0, activeTool: '', retry: '' };
    case 'engine':
      // A stopping engine may still finish its turn; its done says so.
      return ev.state === 'started' || ev.state === 'stopping' ? b : { ...b, busySince: 0, activeTool: '', retry: '' };
  }
  return b;
}
