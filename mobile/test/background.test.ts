/// <reference types="jest" />
import { agentRow, badge, compactDur, jobRow, rows } from '../src/lib/activity';
import { sections } from '../src/lib/ctxstats';
import { apply, applySnapshot, empty, lastUser, unwatchAgent, watchAgent, type View } from '../src/lib/events';
import { modeWord, motion, planMark } from '../src/lib/plan';
import { elapsed, headerStatus } from '../src/lib/turnstate';

const run = (evs: Record<string, unknown>[], v: View = empty) => evs.reduce(apply, v);
const T0 = '2026-11-14T10:00:00Z';
const t0 = Date.parse(T0);

// The shapes dund sends (proto/events.go Agents, Jobs, Plan, Usage).
const agentsEv = {
  type: 'agents',
  ts: T0,
  agents: [
    {
      id: 2,
      model: 'qwen',
      state: 'running',
      status: 'reading plan.go',
      prompt: 'map the plan tool',
      tokens: 18400,
      seconds: 30,
    },
    { id: 3, model: 'qwen', state: 'idle', status: '', prompt: 'write tests', tokens: 900, seconds: 12 },
  ],
};
const jobsEv = {
  type: 'jobs',
  jobs: [
    {
      id: 1,
      command: 'go test ./...',
      log: '/tmp/j1.log',
      state: 'running',
      bytes: 120,
      started: t0 / 1000 - 5,
      ended: 0,
      code: 0,
      muted: false,
    },
    {
      id: 4,
      command: 'make lint',
      log: '/tmp/j4.log',
      state: 'failed',
      bytes: 80,
      started: 100,
      ended: 107,
      code: 2,
      muted: false,
    },
  ],
};
const planEv = {
  type: 'plan',
  mode: true,
  approved: false,
  total: 3,
  done: 1,
  rows: [
    { id: 'a', title: 'read the code', status: 'done' },
    { id: 'b', title: 'write the header', status: 'doing', depth: 1 },
    { id: 'c', title: 'map the plan tool', status: 'todo', owner: 'agent:2', depth: 1, waiting: ['b'] },
  ],
};

test('agents, jobs and the plan fold into bg', () => {
  const v = run([agentsEv, jobsEv, planEv]);
  expect(v.bg.agents.map((a) => a.id)).toEqual([2, 3]);
  expect(v.bg.jobs[1]).toMatchObject({ id: 4, state: 'failed', code: 2 });
  expect(v.bg.plan).toMatchObject({ mode: true, total: 3, done: 1 });
  expect(v.bg.plan?.rows[2]).toMatchObject({ owner: 'agent:2', depth: 1, waiting: ['b'] });
  expect(badge(v.bg.agents, v.bg.jobs)).toEqual({ running: 2, failed: true });
});

test('an empty plan is no plan: the header shows no plan icon', () => {
  expect(run([{ type: 'plan', total: 0, done: 0, rows: [] }]).bg.plan).toBeNull();
});

test('activity rows read as the TUI rows', () => {
  const now = t0 + 10_000;
  const v = run([agentsEv, jobsEv]);
  const [a2, a3, j1, j4] = rows(v.bg.agents, v.bg.jobs, now);
  expect(a2).toMatchObject({ mark: '▸', tone: 'working', label: '#2 running 40s 18.4kt', detail: 'reading plan.go' });
  expect(a3).toMatchObject({ mark: '✓', label: '#3 idle 12s 900t', detail: 'write tests' });
  expect(j1).toMatchObject({ mark: '⚙', label: 'job 1 running 15s', running: true });
  expect(j4).toMatchObject({ mark: '✗', tone: 'failed', label: 'job 4 failed (exit 2) 7s', attn: true });
  expect(jobRow({ ...v.bg.jobs[1]!, state: 'killed' }, now).mark).toBe('⏹');
  expect(agentRow({ ...v.bg.agents[0]!, state: 'stopped' }, now).mark).toBe('⏸');
  expect(compactDur(187)).toBe('3m07s');
});

test('the header status: working with the tool and the clock, waiting, retrying, ready', () => {
  let v = run([
    { type: 'turn', kind: 'user', ts: T0 },
    { type: 'tool_call', tool: 'exec', args: { command: 'go test ./internal/tui/... -run TestPlanViewMotion' } },
  ]);
  // The call as the TUI says it (callText), cut at 40.
  expect(headerStatus(v, t0 + 3000)).toEqual({
    state: 'working',
    text: 'working… $ go test ./internal/tui/... -run TestPl… · 3s',
  });
  v = run([{ type: 'tool_result', tool: 'exec', result: 'a' }], v);
  expect(headerStatus(v, t0 + 65_000).text).toBe('working… · 1m5s');
  v = run([{ type: 'retry', kind: 'wait', text: 'provider 503 — retrying in 4s' }], v);
  expect(headerStatus(v, t0)).toEqual({ state: 'retry', text: '⏳ provider 503 — retrying in 4s' });
  v = run(
    [
      { type: 'retry', kind: 'recovered', text: 'back' },
      { type: 'ask', question: 'which?', options: ['a', 'b'] },
    ],
    v,
  );
  expect(headerStatus(v, t0).state).toBe('ask');
  v = run([{ type: 'done' }], v);
  expect(headerStatus(v, t0)).toEqual({ state: 'idle', text: 'ready' });
  expect(elapsed(0, t0)).toBe('');
});

test('done clears the question the turn asked, as the TUI does', () => {
  const v = run([{ type: 'turn', kind: 'user' }, { type: 'ask', question: 'ok?', options: [] }, { type: 'done' }]);
  expect(v.ask).toBeNull();
});

test('a continue turn (background work started it) is busy', () => {
  const v = run([{ type: 'turn', kind: 'continue', ts: T0 }]);
  expect(v.busy).toBe(true);
  expect(v.bg.busySince).toBe(t0);
});

test('a snapshot brings back the plan, the work and a running turn, its clock from the turn event', () => {
  const v = applySnapshot({
    type: 'snapshot',
    items: [{ kind: 'user', content: 'build the header' }],
    state: [planEv, agentsEv, jobsEv, { type: 'turn', kind: 'user', ts: T0 }],
  });
  expect(v.busy).toBe(true);
  expect(v.bg.busySince).toBe(t0);
  expect(v.bg.plan?.rows).toHaveLength(3);
  expect(v.bg.jobs).toHaveLength(2);
  expect(lastUser(v)).toBe('build the header');
});

test('a history event (the engine respawned) keeps the background state', () => {
  const v = run([agentsEv, planEv, { type: 'history', items: [{ kind: 'user', content: 'again' }] }]);
  expect(v.bg.agents).toHaveLength(2);
  expect(v.bg.plan).not.toBeNull();
});

test('usage, context_cost and spend fold into the context figures', () => {
  const usage = {
    type: 'usage',
    total: 5000,
    active: 41300,
    cached: 3000,
    processed: 4000,
    generated: 1000,
    turns: 1,
    system_tokens: 9000,
    system_exact: true,
    system_prompt: 4000,
    system_parts: [{ name: 'tools', tokens: 5000 }],
    window: 131072,
    window_reserved: 8192,
    window_cap: 4096,
    prompt_budget: 120000,
    prompt_tokens: 41300,
    cache: { rounds: 4, hit_pct: 75, cached: 3000, processed: 4000 },
    spend: { calls: 4, costed_calls: 4, session_cost: 0.0123, currency: 'USD' },
  };
  const v = run([{ type: 'context_cost', system_tokens: 8800, system_exact: false, window: 131072 }, usage, usage]);
  expect(v.bg.ctx).toMatchObject({ total: 10000, generated: 2000, active: 41300, systemTokens: 9000, window: 131072 });
  const s = sections(v.bg.ctx);
  expect(s.map((x) => x.title)).toEqual(['window', 'system', 'tokens', 'cache', 'cost']);
  expect(s[0]!.rows[1]).toMatchObject({ label: 'prompt', value: '41,300 of 120,000 budget', note: '(31%)' });
  expect(s[3]!.rows[0]!.value).toBe('4 · 75% hit');
  expect(s[4]!.rows[0]!.value).toBe('0.01 USD');
  const w = run([{ type: 'spend', calls: 5, costed_calls: 4, session_cost: 0.5, currency: 'USD' }], v);
  expect(sections(w.bg.ctx)[4]!.rows[0]).toMatchObject({ value: '0.50 USD', note: '4 of 5 calls priced' });
});

test('plan marks and motion: the asking node alternates, a working agent spins, idle is still', () => {
  expect(['todo', 'doing', 'done', 'blocked', 'dropped'].map(planMark)).toEqual(['○', '◐', '✓', '⚠', '✗']);
  const v = run([planEv]);
  const doing = v.bg.plan!.rows[1]!;
  const owned = v.bg.plan!.rows[2]!;
  const live = { busy: true, asking: true, agents: { 2: 'running' }, phase: 0 };
  expect(motion(doing, live)).toMatchObject({ mark: '?', tone: 'ask', tag: 'asking you' });
  expect(motion(doing, { ...live, phase: 4 }).tone).toBe('notice');
  expect(motion(doing, { ...live, asking: false, phase: 1 })).toMatchObject({ mark: '⠙', tone: 'working' });
  expect(motion(doing, { ...live, asking: false, busy: false }).mark).toBe('◐');
  expect(motion(owned, live)).toMatchObject({ mark: '⠋', tag: 'agent 2 working' });
  expect(motion(owned, { ...live, agents: {} }).tag).toBe('agent 2 gone');
  expect(modeWord(v.bg.plan!)).toBe('on · waiting for approval');
});

test('an agent view follows its own history and entries, not another agent’s', () => {
  let v = watchAgent(empty, 2);
  v = run(
    [
      { type: 'agent_history', agent: 3, items: [{ kind: 'user', content: 'other' }] },
      { type: 'agent_history', agent: 2, prompt: 'map it', items: [{ kind: 'user', content: 'map it' }] },
      { type: 'agent_entry', agent: 2, items: [{ kind: 'assistant', content: 'done mapping' }] },
    ],
    v,
  );
  expect(v.agentView).toMatchObject({ id: 2, prompt: 'map it', loaded: true });
  expect(v.agentView!.view.items.map((i) => i.kind)).toEqual(['user', 'assistant']);
  expect(v.items).toHaveLength(0);
  expect(unwatchAgent(v).agentView).toBeNull();
});
